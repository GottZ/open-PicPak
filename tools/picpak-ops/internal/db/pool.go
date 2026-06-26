// Package db owns the picpak-ops PostgreSQL/TimescaleDB connection pool: a single
// long-lived *pgxpool.Pool shared by every read-side axis (telemetry, logs, ota,
// fleet). It is the ONE place a connection is opened, configured, and the
// read-only safety property is enforced; no consuming axis opens its own pool.
//
// Safety property (read_only enforcement). pgx/v5 has no built-in "read-only
// pool" switch. When config database.read_only is true (the default), NewPool
// installs an AfterConnect hook that runs `SET SESSION default_transaction_read_only = on`
// on every pooled connection, so any INSERT/UPDATE/DELETE/DDL attempted through
// the pool is rejected by the server with SQLSTATE 25006. This is W8's analog of
// the flash negative gate and is proven by a NEGATIVE test (pool_test.go): the
// same write is blocked under read_only=true and allowed (in a rolled-back tx)
// under read_only=false.
//
// The DSN is resolved by the config axis at load time; it is host-bearing AND a
// secret under the redaction contract. This package never surfaces the resolved
// host: connection errors are sanitized (connError) so a dial failure cannot leak
// host:port into a log line, scrollback ring, or pasted bug report.
package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// readOnlySessionSQL is the statement the AfterConnect hook runs on every pooled
// connection when database.read_only is true. default_transaction_read_only=on
// makes every transaction (including the implicit single-statement one) default
// to read-only, so the server rejects writes with SQLSTATE 25006. This is the
// enforcement mechanism — there is no pgx-level read-only toggle.
const readOnlySessionSQL = "SET SESSION default_transaction_read_only = on"

// sqlstateReadOnly is the SQLSTATE PostgreSQL returns for a write attempted in a
// read-only transaction (read_only_sql_transaction). The negative safety test
// asserts on it to prove the block is the read-only enforcement and not some
// unrelated failure.
const sqlstateReadOnly = "25006"

// NewPool builds and validates the shared pool from cfg.Database. It reads only
// the [database] section: dsn, max_conns, connect_timeout, statement_timeout,
// read_only. The caller owns the returned pool and MUST call its Close() at
// shutdown — that is the single teardown point releasing every connection.
//
// NewPool establishes at least one connection (an initial Ping bounded by
// connect_timeout) so a bad DSN / unreachable DB fails closed here rather than on
// first query, and so the read-only AfterConnect hook is exercised immediately.
func NewPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	dc := cfg.Database
	if dc.DSN == "" {
		// Never echo the DSN (the contract redacts it); the key name is enough.
		return nil, errors.New("db: database.dsn is empty; set it in the [database] section")
	}

	poolCfg, err := pgxpool.ParseConfig(dc.DSN)
	if err != nil {
		// pgx parse errors can echo DSN fields (host/user); do not surface them.
		return nil, errors.New("db: parsing database.dsn failed; check the [database] section")
	}

	if dc.MaxConns > 0 {
		poolCfg.MaxConns = int32(dc.MaxConns)
	}
	if ct := dc.ConnectTimeout.D(); ct > 0 {
		poolCfg.ConnConfig.ConnectTimeout = ct
	}
	// statement_timeout is a server-side GUC sent in the connection startup packet,
	// so every pooled connection carries the per-query ceiling with no extra
	// round-trip. Policy=data: the value comes from config, not a code constant.
	if st := dc.StatementTimeout.D(); st > 0 {
		poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(st.Milliseconds(), 10)
	}

	// read_only enforcement (the safety property). pgx has no built-in toggle, so
	// every freshly opened connection is put into a read-only session here.
	if dc.ReadOnly {
		poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			if _, err := conn.Exec(ctx, readOnlySessionSQL); err != nil {
				return fmt.Errorf("db: enforcing read-only session: %w", err)
			}
			return nil
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, connError("creating pool", err)
	}

	// Validate connectivity + warm one connection through AfterConnect, bounded by
	// connect_timeout so an unreachable DB fails closed promptly.
	pingCtx := ctx
	if ct := dc.ConnectTimeout.D(); ct > 0 {
		var cancel context.CancelFunc
		pingCtx, cancel = context.WithTimeout(ctx, ct)
		defer cancel()
	}
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, connError("initial ping", err)
	}

	return pool, nil
}

// Ping is a small liveness helper consumers re-check the pool with. It acquires a
// connection and round-trips; the caller supplies the context (and thus the
// timeout). Errors are sanitized so the resolved host never leaks.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("db: Ping called with a nil pool")
	}
	if err := pool.Ping(ctx); err != nil {
		return connError("ping", err)
	}
	return nil
}

// connError sanitizes a connection-path error so it never carries the resolved
// host. A server-side *pgconn.PgError (e.g. the read-only 25006, auth, syntax)
// carries a SQLSTATE + message but NOT the resolved host:port, so it is safe (and
// useful) to surface. Any other error (a network/dial failure whose text contains
// host:port) is replaced with a fixed host-free message, honoring the redaction
// contract: never log the resolved DSN host.
func connError(stage string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("db: %s: %s (SQLSTATE %s)", stage, pgErr.Message, pgErr.Code)
	}
	return fmt.Errorf("db: %s failed; check database.dsn host/port/credentials and that the backend is reachable", stage)
}
