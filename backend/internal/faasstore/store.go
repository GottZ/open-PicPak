package faasstore

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrNotFound is returned when a function / last-good row does not exist.
	ErrNotFound = errors.New("faasstore: not found")
	// ErrNoWebhookToken is returned by WebhookTokenSHA when the function has no token
	// (not a webhook trigger, or never set) — handleWebhook maps it to 401, no run.
	ErrNoWebhookToken = errors.New("faasstore: no webhook token")
)

// nameRe is the operator-chosen function name charset (matches the 0009 comment).
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidName reports whether name is an acceptable function name (validated before the DB).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// IsUniqueViolation reports whether err is a Postgres unique-constraint violation (23505)
// — the admin handler maps a duplicate function name to 409.
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

const functionCols = `id, name, source, version, trigger_type, trigger_config, secret_bindings, egress_allow, enabled, created_at, updated_at`

// Create inserts a function (enabled=false, version=1 by default) and returns its id.
// A duplicate name surfaces as a unique violation (IsUniqueViolation → 409).
func Create(ctx context.Context, q Querier, p CreateParams) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO faas_functions (name, source, trigger_type, trigger_config, secret_bindings, egress_allow, webhook_token_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		p.Name, p.Source, string(triggerOrDefault(p.TriggerType)), coalesceJSON(p.TriggerConfig),
		coalesceSlice(p.SecretBindings), coalesceSlice(p.EgressAllow), p.WebhookTokenSHA,
	).Scan(&id)
	return id, err
}

// Update replaces the mutable body (source/config/bindings/egress) and BUMPS version
// (K7 cache-bust). trigger_type and the webhook token are not touched here. Returns
// whether a row matched.
func Update(ctx context.Context, q Querier, id int64, p UpdateParams) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE faas_functions
		SET source = $2, trigger_config = $3, secret_bindings = $4, egress_allow = $5,
		    version = version + 1, updated_at = now()
		WHERE id = $1`,
		id, p.Source, coalesceJSON(p.TriggerConfig), coalesceSlice(p.SecretBindings), coalesceSlice(p.EgressAllow))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetEnabled toggles the pausability flag.
func SetEnabled(ctx context.Context, q Querier, id int64, enabled bool) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE faas_functions SET enabled = $2, updated_at = now() WHERE id = $1`, id, enabled)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetWebhookToken stores a new token hash (rotate). The caller generated the plaintext and
// showed it once (D24.13); only sha256(token) reaches here.
func SetWebhookToken(ctx context.Context, q Querier, id int64, sha []byte) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE faas_functions SET webhook_token_sha256 = $2, updated_at = now() WHERE id = $1`, id, sha)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Delete removes a function; the FK CASCADE drops its bindings + last-good rows.
func Delete(ctx context.Context, q Querier, id int64) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM faas_functions WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// LoadFunction returns the full function (for the render path) or ErrNotFound.
func LoadFunction(ctx context.Context, q Querier, id int64) (*Function, error) {
	return scanFunction(q.QueryRow(ctx, `SELECT `+functionCols+` FROM faas_functions WHERE id = $1`, id))
}

// LoadFunctionByName returns the full function by name (webhook lookup) or ErrNotFound.
func LoadFunctionByName(ctx context.Context, q Querier, name string) (*Function, error) {
	return scanFunction(q.QueryRow(ctx, `SELECT `+functionCols+` FROM faas_functions WHERE name = $1`, name))
}

// ListFunctions returns the list-view projection (no source, no config, no hash).
func ListFunctions(ctx context.Context, q Querier) ([]Summary, error) {
	rows, err := q.Query(ctx, `SELECT id, name, trigger_type, enabled, version FROM faas_functions ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		var tt string
		if err := rows.Scan(&s.ID, &s.Name, &tt, &s.Enabled, &s.Version); err != nil {
			return nil, err
		}
		s.TriggerType = TriggerType(tt)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Version reads only the current version — the cheap indexed SELECT the sync render path
// runs to form the hot-cache key (K7 cache-bust). ErrNotFound if the function is gone.
func Version(ctx context.Context, q Querier, id int64) (int, error) {
	var v int
	err := q.QueryRow(ctx, `SELECT version FROM faas_functions WHERE id = $1`, id).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return v, err
}

// WebhookTokenSHA reads ONLY the token hash, for the constant-time compare in handleWebhook.
// This is the single reader of the hash — no CRUD read carries it (K8). ErrNoWebhookToken if
// the function has no token; ErrNotFound if the function is gone.
func WebhookTokenSHA(ctx context.Context, q Querier, id int64) ([]byte, error) {
	var sha []byte
	err := q.QueryRow(ctx, `SELECT webhook_token_sha256 FROM faas_functions WHERE id = $1`, id).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(sha) == 0 {
		return nil, ErrNoWebhookToken
	}
	return sha, nil
}

// BindDevice points a serial at a function (upsert — exactly one function per serial).
func BindDevice(ctx context.Context, q Querier, serial string, fnID int64) error {
	_, err := q.Exec(ctx, `
		INSERT INTO device_render_binding (serial, function_id) VALUES ($1, $2)
		ON CONFLICT (serial) DO UPDATE SET function_id = EXCLUDED.function_id, bound_at = now()`,
		serial, fnID)
	return err
}

// BoundSerials returns every serial bound to a function — the cron/prerender fan-out set
// (D24.12). Empty (not an error) when nothing is bound.
func BoundSerials(ctx context.Context, q Querier, fnID int64) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT serial FROM device_render_binding WHERE function_id = $1 ORDER BY serial`, fnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BoundFunctionID returns the function a device shows (its single binding), or ok=false.
func BoundFunctionID(ctx context.Context, q Querier, serial string) (int64, bool, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT function_id FROM device_render_binding WHERE serial = $1`, serial).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// BoundFunction returns the id+name of the function a device renders (the forward binding read, Doc 25
// §4.4) — a single join so the editor can label the binding without loading the whole (heavy) source. The
// binding's serial has no FK to devices (0009), so a binding may name a not-yet-onboarded device; ok=false
// means simply "no binding row".
func BoundFunction(ctx context.Context, q Querier, serial string) (id int64, name string, ok bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT f.id, f.name FROM device_render_binding b
		JOIN faas_functions f ON f.id = b.function_id
		WHERE b.serial = $1`, serial).Scan(&id, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return id, name, true, nil
}

// Unbind drops a device's render binding (the Doc 25 §4.4 unbind — A24 ships only the PUT). The device
// falls back to "no function". The (serial,function) last-good is left intact — it is keyed data a
// re-bind reuses and is never served while unbound; a full purge is the device-delete cascade
// (DeleteDeviceBindings, K2), not an unbind. Returns whether a binding row existed.
func Unbind(ctx context.Context, q Querier, serial string) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM device_render_binding WHERE serial = $1`, serial)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteDeviceBindings removes a serial's binding + last-good rows. Their serial has no FK
// to devices (parity with rollout_targets), so the device-delete tx must call this (K2/§9,
// T11-del). Runs on the caller's Querier — pass the tx.
func DeleteDeviceBindings(ctx context.Context, q Querier, serial string) error {
	if _, err := q.Exec(ctx, `DELETE FROM device_render_binding WHERE serial = $1`, serial); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `DELETE FROM faas_frame_lastgood WHERE serial = $1`, serial)
	return err
}

// LastGoodGet returns the durable last-good frame for a (serial, function) or ErrNotFound.
func LastGoodGet(ctx context.Context, q Querier, serial string, fnID int64) (*LastGood, error) {
	var lg LastGood
	err := q.QueryRow(ctx,
		`SELECT packed, status, rendered_at FROM faas_frame_lastgood WHERE serial = $1 AND function_id = $2`,
		serial, fnID).Scan(&lg.Packed, &lg.Status, &lg.RenderedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &lg, nil
}

// LastGoodPut writes/overwrites the durable last-good for a (serial, function). packed must
// be 30000 bytes (enforced by the 0009 CHECK).
func LastGoodPut(ctx context.Context, q Querier, serial string, fnID int64, packed []byte, status string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO faas_frame_lastgood (serial, function_id, packed, status, rendered_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (serial, function_id) DO UPDATE SET packed = EXCLUDED.packed, status = EXCLUDED.status, rendered_at = now()`,
		serial, fnID, packed, status)
	return err
}

// --- helpers ---

func scanFunction(row pgx.Row) (*Function, error) {
	var f Function
	var cfg []byte
	var tt string
	err := row.Scan(&f.ID, &f.Name, &f.Source, &f.Version, &tt, &cfg, &f.SecretBindings, &f.EgressAllow, &f.Enabled, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.TriggerType = TriggerType(tt)
	f.TriggerConfig = json.RawMessage(cfg)
	return &f, nil
}

func coalesceJSON(j json.RawMessage) []byte {
	if len(j) == 0 {
		return []byte("{}")
	}
	return []byte(j)
}

func coalesceSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func triggerOrDefault(t TriggerType) TriggerType {
	if t == "" {
		return TriggerRender
	}
	return t
}
