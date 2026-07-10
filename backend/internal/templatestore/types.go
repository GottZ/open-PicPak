// Package templatestore is the template store (A30 W1): prefab + operator-authored template rows
// (source/kind/params + the render_fn trust profile: egress_allow/secret_bindings/trigger_config),
// migration 0016. Policy=Data — a template's content is a row, never a code constant. Prefab
// ("builtin") rows are seeded idempotently from an embedded Go catalog at startup (SeedBuiltins),
// not by a hand migration, so a binary update refreshes the catalog without migration churn.
//
// All CRUD access goes through a Querier so a call can run on the pool or inside a tx. SeedBuiltins
// needs a Pool (it serialises replica boots under an advisory lock).
package templatestore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrNotFound is returned when a template row does not exist.
	ErrNotFound = errors.New("templatestore: not found")
	// ErrScriptTooLong is returned when a berry_snippet source exceeds the 8191-byte cap
	// (K3 poison-pill) — the handler maps it to 422. Mirror of commandstore.ErrScriptTooLong.
	ErrScriptTooLong = errors.New("templatestore: berry_snippet source exceeds the 8191-byte cap")
	// ErrInvalidName is returned when an operator-chosen name fails ValidName (charset or the
	// reserved 'builtin/' prefix) — the handler maps it to 422.
	ErrInvalidName = errors.New("templatestore: invalid template name")
	// ErrInvalidKind is returned when kind is not one of the three CHECK enum values — 422.
	ErrInvalidKind = errors.New("templatestore: invalid template kind")
)

// BerryScriptMax is the fail-closed berry_snippet source cap in BYTES — the same octet cap the
// firmware fetch enforces (C2_RESP_MAX-1 = 8191, cmd.c:363) and a mirror of commandstore.ScriptMax.
// The 0016 tmpl_berry_len CHECK (octet_length) is the fail-closed backstop; this Go guard delivers
// the clean 422 before the DB round-trip.
const BerryScriptMax = 8191

// Kind discriminators (mirror the 0016 CHECK).
const (
	KindRenderFn       = "render_fn"
	KindBerrySnippet   = "berry_snippet"
	KindPlaylistPreset = "playlist_preset"
)

// IsUniqueViolation reports whether err is a Postgres unique-constraint violation (23505) — a
// duplicate template name maps to 409 (parity faasstore.IsUniqueViolation).
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// Template is the full template row (source + params + the render_fn trust profile).
type Template struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Description is the multilingual "what does this template do" copy: a locale→text map
	// ({de,en,fr,…}). JSONB on the row; the SPA resolves one string via localizedDescription
	// (locale→en→de→first fallback). Never SQL NULL — an empty map is the schema default.
	Description    map[string]string `json:"description"`
	Source         string            `json:"source"`
	Params         json.RawMessage   `json:"params"`
	EgressAllow    []string          `json:"egress_allow"`
	SecretBindings []string          `json:"secret_bindings"`
	TriggerConfig  json.RawMessage   `json:"trigger_config,omitempty"`
	Builtin        bool              `json:"builtin"`
	Version        int               `json:"version"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// Summary is the list-view projection: no source, no params, no trust profile — the picker view.
type Summary struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Description map[string]string `json:"description"`
	Builtin     bool              `json:"builtin"`
	Version     int               `json:"version"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// CreateParams is a new operator-authored template (builtin is always false — seeds bypass Create).
type CreateParams struct {
	Name           string
	Kind           string
	Description    map[string]string
	Source         string
	Params         json.RawMessage
	EgressAllow    []string
	SecretBindings []string
	TriggerConfig  json.RawMessage
}

// UpdateParams replaces the mutable body; Update bumps version.
type UpdateParams struct {
	Description    map[string]string
	Source         string
	Params         json.RawMessage
	EgressAllow    []string
	SecretBindings []string
	TriggerConfig  json.RawMessage
}

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx (parity faasstore.Querier).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Pool is a Querier that can open a transaction — SeedBuiltins needs it to run the upsert loop under
// a single pg_advisory_xact_lock (replica-boot serialisation, §3.3). *pgxpool.Pool satisfies it.
type Pool interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}
