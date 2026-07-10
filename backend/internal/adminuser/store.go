// Package adminuser is the password-account store for human admin login (migration 0014, A28,
// E28.1=(a)). Accounts hold an argon2id PHC hash (internal/argon2id); the plaintext password is
// NEVER stored. Verify authenticates a username+password and enforces the disabled_at soft-disable
// (a disabled account fails verify even with the correct password), running a dummy argon2 verify
// for an unknown username so latency cannot leak account existence (design §4.3 / §5).
package adminuser

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-picpak/backend/internal/argon2id"
)

var (
	// ErrNotFound is returned when a user does not exist (GetByUsername).
	ErrNotFound = errors.New("adminuser: not found")
	// ErrUsernameTaken is returned by Create on a duplicate username (23505).
	ErrUsernameTaken = errors.New("adminuser: username already exists")
)

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// User is an admin_users row. It never carries the password hash — that column is read only inside
// Verify and never surfaces.
type User struct {
	ID         int64      `json:"id"`
	Username   string     `json:"username"`
	IsAdmin    bool       `json:"is_admin"`
	CreatedAt  time.Time  `json:"created_at"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
}

const userCols = `id, username, is_admin, created_at, disabled_at`

// Create hashes password with argon2id and inserts a new account. A duplicate username surfaces as
// ErrUsernameTaken. The plaintext password is never stored.
func Create(ctx context.Context, q Querier, username, password string, isAdmin bool) (User, error) {
	hash, err := argon2id.Hash(password)
	if err != nil {
		return User{}, err
	}
	u, err := scanUser(q.QueryRow(ctx, `
		INSERT INTO admin_users (username, password_hash, is_admin)
		VALUES ($1, $2, $3)
		RETURNING `+userCols, username, hash, isAdmin))
	if isUniqueViolation(err) {
		return User{}, ErrUsernameTaken
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// GetByUsername returns a user by username or ErrNotFound (no password hash).
func GetByUsername(ctx context.Context, q Querier, username string) (User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM admin_users WHERE username = $1`, username))
}

// GetByID returns a user by id or ErrNotFound (no password hash). The session carrier uses it to
// resolve a session's user_id to the account identity + admin flag (design §4.1).
func GetByID(ctx context.Context, q Querier, id int64) (User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM admin_users WHERE id = $1`, id))
}

// Verify authenticates username+password. It returns (user, true) ONLY for an existing,
// non-disabled account whose password matches. A wrong password, an unknown username, OR a
// disabled account all return (_, false, nil). An unknown username still runs a dummy argon2id
// verify so its latency matches a real one (no username-enumeration timing oracle, design §4.3).
func Verify(ctx context.Context, q Querier, username, password string) (User, bool, error) {
	var u User
	var hash string
	err := q.QueryRow(ctx,
		`SELECT `+userCols+`, password_hash FROM admin_users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt, &u.DisabledAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		argon2id.VerifyDummy(password) // constant-cost path against a timing oracle
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	ok, err := argon2id.Verify(password, hash)
	if err != nil {
		return User{}, false, err
	}
	// A disabled account fails even with the correct password — but only AFTER the verify has run,
	// so its latency is identical to an enabled account (no disabled-account timing signal).
	if !ok || u.DisabledAt != nil {
		return User{}, false, nil
	}
	return u, true, nil
}

// SetDisabled soft-disables (disabled_at = now()) or re-enables (disabled_at = NULL) an account.
// ErrNotFound if the id does not exist.
func SetDisabled(ctx context.Context, q Querier, id int64, disabled bool) error {
	var expr string
	if disabled {
		expr = `disabled_at = now()`
	} else {
		expr = `disabled_at = NULL`
	}
	ct, err := q.Exec(ctx, `UPDATE admin_users SET `+expr+` WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- helpers ---

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt, &u.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
