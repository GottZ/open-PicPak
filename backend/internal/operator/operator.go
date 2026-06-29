// Package operator resolves a bearer token to an operator identity for the admin control plane.
//
// The plaintext bearer is never stored: operator_keys holds sha256(token) only (migration 0007).
// A revoked key is a soft delete (disabled_at IS NOT NULL) and authenticates as if absent (SEC-M1).
package operator

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

// Authenticate resolves a bearer token to an operator. It returns (result, true) for a live,
// non-revoked key and (_, false) for an unknown OR disabled key — the disabled_at IS NULL clause
// makes a soft-revoked key indistinguishable from an absent one (SEC-M1 revocation).
func Authenticate(ctx context.Context, db *pgxpool.Pool, token string) (AuthResult, bool, error) {
	var r AuthResult
	err := db.QueryRow(ctx,
		`SELECT id, is_admin, label FROM operator_keys WHERE token_hash = $1 AND disabled_at IS NULL`,
		HashToken(token)).Scan(&r.KeyID, &r.IsAdmin, &r.Label)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return AuthResult{}, false, nil
	case err != nil:
		return AuthResult{}, false, err
	default:
		return r, true, nil
	}
}

// TouchLastUsed bumps last_used_at. Best-effort: the caller runs it async and ignores the error —
// it must never block or fail a request.
func TouchLastUsed(ctx context.Context, db *pgxpool.Pool, keyID int64) error {
	_, err := db.Exec(ctx, `UPDATE operator_keys SET last_used_at = now() WHERE id = $1`, keyID)
	return err
}
