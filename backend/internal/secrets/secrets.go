// Package secrets is the DB + policy layer over internal/sealbox: it owns every row in the `secrets`
// table (migration 0008) and every policy rule. It imports sealbox (the stdlib crypto) and pgx; the
// public ingest parser imports NEITHER (the T7 address-space import guard) so an RCE in that parser
// cannot reach Open()/ResolveSecret().
//
// The one invariant that must not blur: ResolveSecret returns ErrSecretNotFound ONLY for a missing
// row. A sealbox.Open failure (tamper, integrity, a rotation bug) propagates verbatim — it is NOT
// folded into the not-found sentinel, because the Doc 24 resolve-empty fail-open path keys off
// ErrSecretNotFound alone, and masking a decrypt failure as "not found" would hide DB tampering behind
// a stale last-good frame.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-picpak/backend/internal/sealbox"
)

// ErrSecretNotFound is the SENTINEL for "no such row" — the ONLY ResolveSecret failure eligible for a
// caller's fail-open path. A sealbox.Open failure is NOT this.
var ErrSecretNotFound = errors.New("secrets: not found")

// validName: lowercase alnum start, then [a-z0-9._-], 1..128 chars total. The charset guarantees no
// ':' so the AAD layout and the break-glass DecodeLine format stay unambiguous.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidSecretName reports whether name is a legal secret key. Validation is a runtime concern (no DB
// CHECK on name) — checked before the DB so a bad name is a 422, not a 500.
func ValidSecretName(name string) bool { return validName.MatchString(name) }

// Querier is any pgx executor — *pgxpool.Pool or pgx.Tx — so every function runs inside or outside a
// transaction (the boot sweep wraps itself in a tx; the request handlers use the pool).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Meta is the non-sensitive view of a secret: identity + provenance, NEVER ciphertext, nonce, value
// or a fingerprint of any of them.
type Meta struct {
	Name       string     `json:"name"`
	KeyVersion int        `json:"key_version"`
	CreatedAt  time.Time  `json:"created_at"`
	RotatedAt  *time.Time `json:"rotated_at"`
}

// PutSecret seals value under name and UPSERTs. created=true ⇒ a new row; created=false ⇒ an existing
// secret was rotated (fresh nonce + ciphertext, rotated_at=now(), key_version untouched — only the
// sweep bumps it). The plaintext value never persists.
func PutSecret(ctx context.Context, q Querier, b *sealbox.Box, name string, value []byte) (created bool, err error) {
	nonce, ct, err := b.Seal(name, value)
	if err != nil {
		return false, err
	}
	// xmax = 0 on the returned row ⇒ the INSERT path fired (created); non-zero ⇒ the DO UPDATE path
	// fired (rotated). key_version is deliberately NOT in the UPDATE set.
	err = q.QueryRow(ctx,
		`INSERT INTO secrets (name, ciphertext, nonce, key_version)
		      VALUES ($1, $2, $3, 1)
		 ON CONFLICT (name) DO UPDATE
		      SET ciphertext = EXCLUDED.ciphertext,
		          nonce      = EXCLUDED.nonce,
		          rotated_at = now()
		 RETURNING (xmax = 0)`, name, ct, nonce).Scan(&created)
	return created, err
}

// ListMeta returns metadata for every secret, ordered by name. ciphertext/nonce are never selected.
func ListMeta(ctx context.Context, q Querier) ([]Meta, error) {
	rows, err := q.Query(ctx, `SELECT name, key_version, created_at, rotated_at FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Meta{}
	for rows.Next() {
		var m Meta
		if err := rows.Scan(&m.Name, &m.KeyVersion, &m.CreatedAt, &m.RotatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMeta returns one secret's metadata, or ErrSecretNotFound. ciphertext/nonce are never selected.
func GetMeta(ctx context.Context, q Querier, name string) (Meta, error) {
	var m Meta
	err := q.QueryRow(ctx, `SELECT name, key_version, created_at, rotated_at FROM secrets WHERE name = $1`, name).
		Scan(&m.Name, &m.KeyVersion, &m.CreatedAt, &m.RotatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Meta{}, ErrSecretNotFound
	}
	return m, err
}

// DeleteSecret removes a secret. found=false means there was no such row (the handler maps it to 404).
func DeleteSecret(ctx context.Context, q Querier, name string) (found bool, err error) {
	tag, err := q.Exec(ctx, `DELETE FROM secrets WHERE name = $1`, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ResolveSecret returns the decrypted value for name. A missing row → ErrSecretNotFound (the only
// fail-open-eligible error). A sealbox.Open failure → the raw cipher error, propagated HARD (T12): it
// must NOT be re-merged into the not-found sentinel.
func ResolveSecret(ctx context.Context, q Querier, b *sealbox.Box, name string) ([]byte, error) {
	var ct, nonce []byte
	err := q.QueryRow(ctx, `SELECT ciphertext, nonce FROM secrets WHERE name = $1`, name).Scan(&ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSecretNotFound
	}
	if err != nil {
		return nil, err
	}
	plaintext, _, err := b.Open(name, nonce, ct)
	if err != nil {
		return nil, err // HARD — distinct from ErrSecretNotFound (D18.6 two-class split)
	}
	return plaintext, nil
}

// Sweep is the rotation step: with a prev key loaded, it opens every secret (current → prev fallback)
// and re-seals the prev-key rows under the current key with a fresh nonce, bumping key_version. It is
// a no-op without a prev key, and runs boot-only (never in a request path — no re-seal-on-read race).
//
// GAP-M3 contracts:
//   - Partial-failure / resumability: a per-row fault (opens under NEITHER key, or its re-seal write
//     fails) is collected into `stranded` and the sweep CONTINUES — one bad row never strands the
//     rest, and a re-run resumes cleanly. `err` is non-nil iff `stranded` is non-empty.
//   - Completion gate: a non-empty `stranded` is the brick signal — the caller MUST keep
//     SECRETS_KEY_PREV (those rows would be unrecoverable once prev drops).
//   - Optimistic concurrency: the re-seal UPDATE only fires while the row still holds the ciphertext
//     we opened, so a concurrent PUT-rotate is never clobbered.
func Sweep(ctx context.Context, q Querier, b *sealbox.Box) (reSealed int, stranded []string, err error) {
	if !b.HasPrev() {
		return 0, nil, nil
	}
	// Gather first: a single pgx connection cannot Exec mid-iteration of its own Query.
	type row struct {
		name      string
		ct, nonce []byte
	}
	var all []row
	rows, err := q.Query(ctx, `SELECT name, ciphertext, nonce FROM secrets`)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.ct, &r.nonce); err != nil {
			rows.Close()
			return 0, nil, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	for _, r := range all {
		plaintext, usedPrev, oerr := b.Open(r.name, r.nonce, r.ct)
		if oerr != nil {
			stranded = append(stranded, r.name) // unopenable under either key — needs attention, keep prev
			continue
		}
		if !usedPrev {
			continue // already under the current key
		}
		nn, nct, serr := b.Seal(r.name, plaintext)
		if serr != nil {
			stranded = append(stranded, r.name)
			continue
		}
		tag, eerr := q.Exec(ctx,
			`UPDATE secrets SET ciphertext = $2, nonce = $3, key_version = key_version + 1
			   WHERE name = $1 AND ciphertext = $4`,
			r.name, nct, nn, r.ct)
		if eerr != nil {
			stranded = append(stranded, r.name)
			continue
		}
		if tag.RowsAffected() == 1 {
			reSealed++
		}
		// RowsAffected()==0 ⇒ a concurrent PUT already rewrote the row under the current key; its value
		// is current, not stranded — leave it.
	}
	if len(stranded) > 0 {
		err = fmt.Errorf("sweep: %d secret(s) could not be re-sealed: %v", len(stranded), stranded)
	}
	return reSealed, stranded, err
}
