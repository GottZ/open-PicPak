// Package operator resolves a bearer token to an operator identity for the admin control plane.
//
// The plaintext bearer is never stored: operator_keys holds sha256(token) only (migration 0007).
// A revoked key is a soft delete (disabled_at IS NOT NULL) and authenticates as if absent (SEC-M1).
package operator

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx, so the auth path can run on a pool and the
// tests can inject a fake row source without a live DB.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// AuthResult is the resolved identity of an authenticated operator.
type AuthResult struct {
	KeyID   int64  `json:"key_id"`
	IsAdmin bool   `json:"is_admin"`
	Label   string `json:"label"`
}

// HashToken returns sha256(token) — the form stored in operator_keys.token_hash.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// dummyHash is a fixed 32-byte value the not-found path compares against so an unknown key runs the
// SAME constant-time compare as a live one — see Authenticate / design §5 B2.
var dummyHash = sha256.Sum256([]byte("operator: constant-time dummy hash"))

// compare is subtle.ConstantTimeCompare, indirected only so a test can assert the miss path still
// runs a compare (no early return before the compare, design §5 B2). Production behaviour is
// identical to calling subtle.ConstantTimeCompare directly.
var compare = subtle.ConstantTimeCompare

// Authenticate resolves a bearer token to an operator. It returns (result, true) for a live,
// non-revoked key and (_, false) for an unknown OR disabled key — the disabled_at IS NULL clause
// makes a soft-revoked key indistinguishable from an absent one (SEC-M1 revocation).
//
// The secret is compared app-side in CONSTANT TIME (fetch-then-ConstantTimeCompare, the same S8
// shape apitoken uses), and the not-found path still runs a compare against a dummy hash so an
// unknown key costs the same as a live one — no early return before the compare (design §5 B2).
// Expiry is enforced after the compare (design §5 B8 / W7): a key past expires_at authenticates as
// absent, so the SSO removal (W8) cannot leave a permanently-valid RCE-capable key standing on the
// public boundary. expires_at IS NULL means no expiry (parity apitoken.Resolve). The auth result is
// byte-for-byte what the previous index-equality form produced for a live, unexpired key.
func Authenticate(ctx context.Context, q Querier, token string) (AuthResult, bool, error) {
	h := HashToken(token)
	var r AuthResult
	var stored []byte
	var expiresAt *time.Time
	err := q.QueryRow(ctx,
		`SELECT id, is_admin, label, token_hash, expires_at FROM operator_keys WHERE token_hash = $1 AND disabled_at IS NULL`,
		h).Scan(&r.KeyID, &r.IsAdmin, &r.Label, &stored, &expiresAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		compare(h, dummyHash[:]) // run the compare on the miss path too — uniform timing, no early return
		return AuthResult{}, false, nil
	case err != nil:
		return AuthResult{}, false, err
	}
	if compare(h, stored) != 1 {
		return AuthResult{}, false, nil
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return AuthResult{}, false, nil // expired key authenticates as absent (design §5 B8)
	}
	return r, true, nil
}

// TouchLastUsed bumps last_used_at. Best-effort: the caller runs it async and ignores the error —
// it must never block or fail a request.
func TouchLastUsed(ctx context.Context, q Querier, keyID int64) error {
	_, err := q.Exec(ctx, `UPDATE operator_keys SET last_used_at = now() WHERE id = $1`, keyID)
	return err
}
