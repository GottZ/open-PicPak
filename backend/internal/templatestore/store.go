package templatestore

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// nameRe mirrors faasstore's operator-name charset. It admits no '/', so a 'builtin/'-prefixed name
// already fails here; the explicit HasPrefix check below makes the reservation legible and robust
// if the charset ever widens.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// builtinPrefix is the reserved namespace for prefab templates. Operators cannot create a name in it
// (ValidName rejects), so a seed can never silently lose its ON CONFLICT to an operator-squatted row
// (§3.3 silent-install-fail).
const builtinPrefix = "builtin/"

// ValidName reports whether name is an acceptable OPERATOR-chosen template name: the faas charset AND
// not in the reserved 'builtin/' namespace. SeedBuiltins bypasses this — the catalog is trusted.
func ValidName(name string) bool {
	return nameRe.MatchString(name) && !strings.HasPrefix(name, builtinPrefix)
}

func validKind(kind string) bool {
	switch kind {
	case KindRenderFn, KindBerrySnippet, KindPlaylistPreset:
		return true
	}
	return false
}

const fullCols = `id, name, kind, source, params, egress_allow, secret_bindings, trigger_config, builtin, version, created_at, updated_at`

// Create inserts an operator-authored template (builtin=false, version=1) and returns its id. Name is
// ValidName-checked (rejects the 'builtin/' namespace), kind against the enum, and a berry_snippet
// source against the 8191-byte cap — all before the DB round-trip, so the handler gets clean 4xx
// errors. A duplicate name still surfaces as a unique violation (IsUniqueViolation → 409).
func Create(ctx context.Context, q Querier, p CreateParams) (int64, error) {
	if !ValidName(p.Name) {
		return 0, ErrInvalidName
	}
	if !validKind(p.Kind) {
		return 0, ErrInvalidKind
	}
	if p.Kind == KindBerrySnippet && len(p.Source) > BerryScriptMax {
		return 0, ErrScriptTooLong
	}
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO templates (name, kind, source, params, egress_allow, secret_bindings, trigger_config, builtin)
		VALUES ($1, $2, $3, $4, $5, $6, $7, false)
		RETURNING id`,
		p.Name, p.Kind, p.Source, coalesceParams(p.Params),
		coalesceSlice(p.EgressAllow), coalesceSlice(p.SecretBindings), nullableJSON(p.TriggerConfig),
	).Scan(&id)
	return id, err
}

// Update replaces the mutable body (source/params/egress/secret/trigger) and BUMPS version. It does
// not change kind or name. The berry-length re-check uses the row's stored kind so a berry_snippet
// cannot be grown past the cap. Returns whether a row matched. Builtin-immutability (409) is the
// handler's concern (W2); the store is generic.
func Update(ctx context.Context, q Querier, id int64, p UpdateParams) (bool, error) {
	var kind string
	if err := q.QueryRow(ctx, `SELECT kind FROM templates WHERE id = $1`, id).Scan(&kind); err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	if kind == KindBerrySnippet && len(p.Source) > BerryScriptMax {
		return false, ErrScriptTooLong
	}
	tag, err := q.Exec(ctx, `
		UPDATE templates
		SET source = $2, params = $3, egress_allow = $4, secret_bindings = $5, trigger_config = $6,
		    version = version + 1, updated_at = now()
		WHERE id = $1`,
		id, p.Source, coalesceParams(p.Params), coalesceSlice(p.EgressAllow),
		coalesceSlice(p.SecretBindings), nullableJSON(p.TriggerConfig))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Delete removes a template. The faas_functions.template_id FK is ON DELETE SET NULL, so a running
// function is never severed. Returns whether a row existed. Builtin-immutability (409) is W2.
func Delete(ctx context.Context, q Querier, id int64) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM templates WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Load returns the full template (source + params + trust profile) or ErrNotFound.
func Load(ctx context.Context, q Querier, id int64) (*Template, error) {
	return scanTemplate(q.QueryRow(ctx, `SELECT `+fullCols+` FROM templates WHERE id = $1`, id))
}

// List returns the list-view projection with keyset pagination (constant latency at target scale, §6).
// An empty kindFilter lists all kinds; a non-empty one uses the composite (kind,id) index range scan.
// afterID walks forward (id > afterID); limit is clamped to [1,200].
func List(ctx context.Context, q Querier, kindFilter string, limit, afterID int64) ([]Summary, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	var rows pgx.Rows
	var err error
	if kindFilter == "" {
		rows, err = q.Query(ctx, `
			SELECT id, name, kind, builtin, version, updated_at FROM templates
			WHERE id > $1 ORDER BY id LIMIT $2`, afterID, limit)
	} else {
		rows, err = q.Query(ctx, `
			SELECT id, name, kind, builtin, version, updated_at FROM templates
			WHERE kind = $1 AND id > $2 ORDER BY id LIMIT $3`, kindFilter, afterID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		if err := rows.Scan(&s.ID, &s.Name, &s.Kind, &s.Builtin, &s.Version, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// --- helpers ---

func scanTemplate(row pgx.Row) (*Template, error) {
	var t Template
	var params, trigger []byte
	err := row.Scan(&t.ID, &t.Name, &t.Kind, &t.Source, &params, &t.EgressAllow, &t.SecretBindings,
		&trigger, &t.Builtin, &t.Version, &t.CreatedAt, &t.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Params = json.RawMessage(params)
	if trigger != nil {
		t.TriggerConfig = json.RawMessage(trigger)
	}
	return &t, nil
}

// coalesceParams keeps the params JSONB non-null (schema default '[]').
func coalesceParams(j json.RawMessage) []byte {
	if len(j) == 0 {
		return []byte("[]")
	}
	return []byte(j)
}

// nullableJSON passes trigger_config through as SQL NULL when empty (the column is nullable).
func nullableJSON(j json.RawMessage) any {
	if len(j) == 0 {
		return nil
	}
	return []byte(j)
}

func coalesceSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
