// Package apitoken is the machine-auth store for the admin control plane (migration 0014, A28):
// externally-mintable bearer tokens of the form ppk_<token_id>_<secret>, kept SEPARATE from
// operator_keys so a leaked image-upload token can never carry the fleet-admin / RCE capability
// (design §5 B3). The plaintext token is shown exactly once at Create and NEVER stored: the row
// holds only sha256(secret). Authentication fetches by the non-secret token_id and compares the
// secret in constant time (crypto/subtle) — the S8 fetch-then-ConstantTimeCompare pattern, not the
// 0007 index-equality pattern — then enforces expiry. The Token struct structurally carries no
// secret hash, so it can never surface in an API read.
package apitoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// TokenPrefix marks a picpak api token. The auth path dispatches on it, and (design §4.1)
	// only ppk_-prefixed bearers are honoured on the public listener.
	TokenPrefix = "ppk_"
	tokenIDLen  = 12 // random bytes for the public lookup handle (96 bits)
	secretLen   = 32 // random bytes for the compared secret (256 bits)
)

var enc = base64.RawURLEncoding

// tokenIDEncLen is the fixed character length of an encoded token_id. Both the token_id and the
// secret are base64url, whose alphabet INCLUDES '_', so the id/secret separator cannot be found by
// splitting on the first '_' (a token_id or secret containing '_' would mis-split, and ~21% of
// token_ids do). Create always writes the separator at exactly this offset, so ParseToken splits at
// the fixed length instead.
var tokenIDEncLen = enc.EncodedLen(tokenIDLen)

var (
	// ErrNotFound is returned when a token id does not exist (Get/Revoke).
	ErrNotFound = errors.New("apitoken: not found")
	// ErrUnknownScope is returned by Create for a scope outside KnownScopes — a clean app-side
	// rejection instead of a raw 23514 from the scopes_known CHECK.
	ErrUnknownScope = errors.New("apitoken: unknown scope")
)

// KnownScopes are the only scopes the schema CHECK (api_tokens_scopes_known) accepts.
var KnownScopes = map[string]bool{"image:read": true, "image:write": true}

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Token is an api_tokens row. It deliberately carries NO secret hash field — the plaintext is
// once-shown at Create and the hash never leaves the store.
type Token struct {
	ID         int64      `json:"id"`
	TokenID    string     `json:"token_id"`
	Label      string     `json:"label"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	CreatedBy  *int64     `json:"created_by,omitempty"`
}

const tokenCols = `id, token_id, label, scopes, created_at, expires_at, last_used_at, disabled_at, created_by`

// Create mints a new token: it generates a random token_id + secret, stores ONLY sha256(secret),
// and returns the full plaintext token EXACTLY ONCE (once-shown, design §4.5 / S7). Unknown scopes
// are rejected app-side (ErrUnknownScope) before the insert. expiresAt=nil means no expiry;
// createdBy=nil attributes the mint to no operator (CLI bootstrap).
func Create(ctx context.Context, q Querier, label string, scopes []string, expiresAt *time.Time, createdBy *int64) (plaintext string, tok Token, err error) {
	for _, s := range scopes {
		if !KnownScopes[s] {
			return "", Token{}, fmt.Errorf("%w: %q", ErrUnknownScope, s)
		}
	}
	if scopes == nil {
		scopes = []string{}
	}
	tokenID, err := randString(tokenIDLen)
	if err != nil {
		return "", Token{}, err
	}
	secret, err := randString(secretLen)
	if err != nil {
		return "", Token{}, err
	}
	sum := sha256.Sum256([]byte(secret))

	tok, err = scanToken(q.QueryRow(ctx, `
		INSERT INTO api_tokens (token_id, secret_hash, label, scopes, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+tokenCols,
		tokenID, sum[:], label, scopes, expiresAt, createdBy))
	if err != nil {
		return "", Token{}, err
	}
	plaintext = TokenPrefix + tokenID + "_" + secret
	return plaintext, tok, nil
}

// ParseToken splits a ppk_<token_id>_<secret> bearer into its parts. It does no DB work and no
// crypto — it only validates the structural shape. A bearer without the prefix, too short, or
// missing the separator at the fixed token_id offset is malformed (ok=false).
//
// The split is at the FIXED token_id length, not on the first '_': the token_id and the secret are
// both base64url and may contain '_', so a first-'_' split mis-parses ~21% of tokens (the token_id
// would be truncated and the DB lookup miss). Create always writes the separator at tokenIDEncLen.
func ParseToken(raw string) (tokenID, secret string, ok bool) {
	rest, found := strings.CutPrefix(raw, TokenPrefix)
	if !found {
		return "", "", false
	}
	if len(rest) < tokenIDEncLen+2 || rest[tokenIDEncLen] != '_' {
		return "", "", false
	}
	tokenID, secret = rest[:tokenIDEncLen], rest[tokenIDEncLen+1:]
	if tokenID == "" || secret == "" {
		return "", "", false
	}
	return tokenID, secret, true
}

// Resolve authenticates a raw bearer token. It fetches the live row by the non-secret token_id
// (disabled_at IS NULL), compares sha256(secret) against the stored hash in CONSTANT TIME, and
// enforces expiry. A malformed token, an unknown token_id, a wrong secret, a disabled row, or an
// expired token all return (_, false, nil) — indistinguishable to the caller (uniform 401, no
// enumeration signal). This is the store primitive the W2 middleware wraps.
func Resolve(ctx context.Context, q Querier, raw string) (Token, bool, error) {
	tokenID, secret, ok := ParseToken(raw)
	if !ok {
		return Token{}, false, nil
	}
	var storedHash []byte
	tok, err := scanTokenRow(q.QueryRow(ctx, `
		SELECT `+tokenCols+`, secret_hash FROM api_tokens
		WHERE token_id = $1 AND disabled_at IS NULL`, tokenID), &storedHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Token{}, false, nil
	case err != nil:
		return Token{}, false, err
	}
	sum := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(sum[:], storedHash) != 1 {
		return Token{}, false, nil
	}
	if tok.ExpiresAt != nil && !tok.ExpiresAt.After(time.Now()) {
		return Token{}, false, nil
	}
	return tok, true, nil
}

// Get returns a token row by id (no secret) or ErrNotFound.
func Get(ctx context.Context, q Querier, id int64) (Token, error) {
	return scanToken(q.QueryRow(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE id = $1`, id))
}

// Revoke soft-disables a token (disabled_at = now()); from the next request on it authenticates as
// absent (SEC-M1). ErrNotFound if the id does not exist. Re-revoking simply refreshes the stamp.
func Revoke(ctx context.Context, q Querier, id int64) error {
	ct, err := q.Exec(ctx, `UPDATE api_tokens SET disabled_at = now() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLastUsed bumps last_used_at. Best-effort, async — mirrors operator.TouchLastUsed; the caller
// runs it off the request path and ignores the error (design §3.1 / §4.1).
func TouchLastUsed(ctx context.Context, q Querier, id int64) error {
	_, err := q.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, id)
	return err
}

// --- helpers ---

func randString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return enc.EncodeToString(b), nil
}

type rowScanner interface{ Scan(dest ...any) error }

// scanTokenRow scans the tokenCols in order, plus any extra destinations (e.g. secret_hash) the
// caller appended to the SELECT.
func scanTokenRow(s rowScanner, extra ...any) (Token, error) {
	var t Token
	dest := []any{&t.ID, &t.TokenID, &t.Label, &t.Scopes, &t.CreatedAt,
		&t.ExpiresAt, &t.LastUsedAt, &t.DisabledAt, &t.CreatedBy}
	dest = append(dest, extra...)
	err := s.Scan(dest...)
	return t, err
}

func scanToken(row pgx.Row) (Token, error) {
	t, err := scanTokenRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, err
	}
	return t, nil
}
