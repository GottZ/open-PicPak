// Package adminaudit is the append-only audit trail for the admin control plane (migration 0014,
// A28 §4.6). After the SSO removal (W8) admin is public, so a who/what/when for token mint/revoke,
// image delete and login is load-bearing. Callers only ever append (Write); rows are never updated
// or deleted in place. A plaintext secret is NEVER written to target — only token_id / username /
// image-id.
package adminaudit

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Entry is one audit record. ActorID, Target and RemoteIP are stored as NULL when empty.
type Entry struct {
	ActorKind string // 'session' | 'bearer' | 'operator' | 'cli' | 'basic'
	ActorID   string // user_id / api_token.token_id / operator_key.id
	Action    string // 'token.mint' | 'token.revoke' | 'image.delete' | 'session.login' | 'session.login_fail' | ...
	Target    string // image-id / token_id / username (NEVER a plaintext secret)
	RemoteIP  string // the non-spoofable source fixed in design §4.4
	OK        bool
}

// Write appends one audit row.
func Write(ctx context.Context, q Querier, e Entry) error {
	_, err := q.Exec(ctx, `
		INSERT INTO admin_audit (actor_kind, actor_id, action, target, remote_ip, ok)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		e.ActorKind, nullStr(e.ActorID), e.Action, nullStr(e.Target), nullStr(e.RemoteIP), e.OK)
	return err
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
