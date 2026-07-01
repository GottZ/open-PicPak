// Package faasstore is the FaaS function store: function rows (source/trigger/config/
// bindings/egress/enabled, versioned), per-device render bindings, and the durable
// per-(serial,function) last-good frame (migration 0009). Policy=Data — the renderer's
// behaviour is a row, never a code constant (M3). All access goes through a Querier so a
// call can run on the pool or inside a tx (e.g. the device-delete cascade, W5).
//
// The webhook token hash is NEVER carried on Function (the render/list shape): it is read
// only by WebhookTokenSHA for the constant-time compare, so no CRUD read can ever echo it (K8).
package faasstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// TriggerType is the function's trigger discriminator (matches the 0009 CHECK).
type TriggerType string

const (
	TriggerRender   TriggerType = "render"
	TriggerSchedule TriggerType = "schedule"
	TriggerWebhook  TriggerType = "webhook"
)

// Function is the full render function the supervisor drives — source + config + the
// least-privilege secret/egress lists. It carries NO webhook token hash (K8).
type Function struct {
	ID             int64
	Name           string
	Source         string
	Version        int
	TriggerType    TriggerType
	TriggerConfig  json.RawMessage
	SecretBindings []string
	EgressAllow    []string
	Enabled        bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Summary is the list-view projection: no source, no config, no hash.
type Summary struct {
	ID          int64       `json:"id"`
	Name        string      `json:"name"`
	TriggerType TriggerType `json:"trigger_type"`
	Enabled     bool        `json:"enabled"`
	Version     int         `json:"version"`
}

// CreateParams is a new function. WebhookTokenSHA is set only for webhook triggers
// (the plaintext token is generated + shown once by the admin handler, D24.13); nil otherwise.
type CreateParams struct {
	Name           string
	Source         string
	TriggerType    TriggerType
	TriggerConfig  json.RawMessage
	SecretBindings []string
	EgressAllow    []string
	WebhookTokenSHA []byte
}

// UpdateParams updates the mutable body; the caller bumps version via Update.
type UpdateParams struct {
	Source         string
	TriggerConfig  json.RawMessage
	SecretBindings []string
	EgressAllow    []string
}

// LastGood is a durable packed frame (30000 B) for a (serial, function).
type LastGood struct {
	Packed     []byte
	Status     string
	RenderedAt time.Time
}
