// Package adminsession is the browser-session store for human admin login (migration 0014, A28).
// A session's primary key is sha256(cookie-secret): the cookie carries the plaintext secret, so a
// DB leak yields no usable cookies (hash-at-rest, same principle as api_tokens). expires_at is the
// server-authoritative absolute lifetime; Resolve rejects an expired session, and Cleanup purges
// them on a ticker (design §3.2).
package adminsession

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const secretLen = 32 // random bytes for the cookie secret (256 bits)

var enc = base64.RawURLEncoding

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Session is an admin_sessions row (no secret — the plaintext lives only in the cookie).
type Session struct {
	UserID     int64     `json:"user_id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// Create opens a session for a user: it generates a random secret, stores sha256(secret) as the
// primary key, and returns the plaintext secret for the cookie. ttl sets the absolute,
// server-authoritative expiry (expires_at = now()+ttl).
func Create(ctx context.Context, q Querier, userID int64, ttl time.Duration) (secret string, sess Session, err error) {
	raw := make([]byte, secretLen)
	if _, err = rand.Read(raw); err != nil {
		return "", Session{}, err
	}
	secret = enc.EncodeToString(raw)
	err = q.QueryRow(ctx, `
		INSERT INTO admin_sessions (id, user_id, expires_at)
		VALUES ($1, $2, $3)
		RETURNING user_id, created_at, expires_at, last_seen_at`,
		hashSecret(secret), userID, time.Now().Add(ttl)).
		Scan(&sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeenAt)
	if err != nil {
		return "", Session{}, err
	}
	return secret, sess, nil
}

// Resolve looks up a session by its cookie secret and returns it only if unexpired. An unknown or
// expired secret returns (_, false, nil) — the caller treats both as unauthenticated.
func Resolve(ctx context.Context, q Querier, secret string) (Session, bool, error) {
	var sess Session
	err := q.QueryRow(ctx, `
		SELECT user_id, created_at, expires_at, last_seen_at
		FROM admin_sessions WHERE id = $1`, hashSecret(secret)).
		Scan(&sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	if !sess.ExpiresAt.After(time.Now()) {
		return Session{}, false, nil
	}
	return sess, true, nil
}

// Delete removes a session (logout). Idempotent: deleting a missing session is not an error.
func Delete(ctx context.Context, q Querier, secret string) error {
	_, err := q.Exec(ctx, `DELETE FROM admin_sessions WHERE id = $1`, hashSecret(secret))
	return err
}

// Cleanup removes all expired sessions (periodic ticker, design §3.2) and returns the count purged.
func Cleanup(ctx context.Context, q Querier) (int64, error) {
	ct, err := q.Exec(ctx, `DELETE FROM admin_sessions WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}
