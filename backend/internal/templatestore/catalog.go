package templatestore

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"
)

// builtinsFS holds the embedded prefab catalog: a catalog.json (metadata) plus one source file per
// entry (.js for render_fn, .be for berry_snippet). Data, never a hand-typed const — the same
// go:embed discipline as internal/berry's manifest.json.
//
//go:embed builtins/catalog.json builtins/*.js builtins/*.be
var builtinsFS embed.FS

// seedAdvisoryLock is the constant key SeedBuiltins holds for the duration of its tx, so concurrent
// admin-replica boots serialise the upsert loop instead of contending row locks (§3.3). Arbitrary
// but fixed ("picpak.templatestore.seed").
const seedAdvisoryLock int64 = 0x7069637061705431

// catalogEntry is one row of builtins/catalog.json. SourceFile names the sibling source file; the
// trust profile (egress_allow/secret_bindings/trigger_config) rides along so a builtin carries its
// A24 least-privilege on the wire (Deliverable-4 needs a pinned egress host, §4.3).
type catalogEntry struct {
	Name           string            `json:"name"`
	Kind           string            `json:"kind"`
	Description    map[string]string `json:"description"`
	SourceFile     string            `json:"source_file"`
	Params         json.RawMessage   `json:"params"`
	EgressAllow    []string          `json:"egress_allow"`
	SecretBindings []string          `json:"secret_bindings"`
	TriggerConfig  json.RawMessage   `json:"trigger_config"`
}

// SeedResult reports the outcome of a SeedBuiltins run. Unchanged>0 with Inserted==Updated==0 on a
// second boot without a catalog change is the idempotency gate (no version bump, no updated_at churn).
type SeedResult struct {
	Inserted  int
	Updated   int
	Unchanged int
}

// loadCatalog parses the embedded catalog and attaches each entry's source. It fails loud (a broken
// embedded catalog is a build/packaging defect, not a runtime condition to swallow).
func loadCatalog() ([]catalogEntry, []string, error) {
	raw, err := builtinsFS.ReadFile("builtins/catalog.json")
	if err != nil {
		return nil, nil, err
	}
	var entries []catalogEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, nil, fmt.Errorf("templatestore: catalog.json invalid: %w", err)
	}
	sources := make([]string, len(entries))
	for i, e := range entries {
		if !strings.HasPrefix(e.Name, builtinPrefix) {
			return nil, nil, fmt.Errorf("templatestore: builtin %q must live in the %q namespace", e.Name, builtinPrefix)
		}
		if !validKind(e.Kind) {
			return nil, nil, fmt.Errorf("templatestore: builtin %q has invalid kind %q", e.Name, e.Kind)
		}
		src, err := builtinsFS.ReadFile("builtins/" + e.SourceFile)
		if err != nil {
			return nil, nil, fmt.Errorf("templatestore: builtin %q source %q: %w", e.Name, e.SourceFile, err)
		}
		if e.Kind == KindBerrySnippet && len(src) > BerryScriptMax {
			return nil, nil, fmt.Errorf("templatestore: builtin %q source is %d bytes, over the %d cap", e.Name, len(src), BerryScriptMax)
		}
		sources[i] = string(src)
	}
	return entries, sources, nil
}

// SeedBuiltins idempotently upserts the embedded prefab catalog into `templates` at admin startup.
//
// Idempotency (the gate): the ON CONFLICT DO UPDATE fires ONLY on a real content change
// (WHERE templates.builtin AND (... IS DISTINCT FROM ...)). Without the guard every boot would bump
// version on every builtin row and, with rolling replicas, contend row locks on unchanged rows. With
// it, a restart on an unchanged catalog is a true no-op (Unchanged only).
//
// The WHERE templates.builtin also fail-closes the silent-install collision: an operator-authored row
// with a builtin name would suppress the update — but operators cannot mint a 'builtin/' name
// (ValidName), so the collision is unreachable; a post-upsert WARN still flags any expected builtin
// that is not a builtin=true row (telemetry, not swallow).
//
// The whole loop runs in one tx under pg_advisory_xact_lock so concurrent replica boots serialise.
func SeedBuiltins(ctx context.Context, pool Pool) (SeedResult, error) {
	entries, sources, err := loadCatalog()
	if err != nil {
		return SeedResult{}, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return SeedResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, seedAdvisoryLock); err != nil {
		return SeedResult{}, err
	}

	var res SeedResult
	for i, e := range entries {
		// RETURNING (xmax <> 0): no row → the guard suppressed the update (unchanged); xmax=0 → a
		// fresh insert; xmax<>0 → a real update. This is the exact per-row signal the idempotency gate
		// reads without a second query.
		var wasUpdate bool
		err := tx.QueryRow(ctx, `
			INSERT INTO templates (name, kind, description, source, params, egress_allow, secret_bindings, trigger_config, builtin)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)
			ON CONFLICT (name) DO UPDATE SET
			    kind = EXCLUDED.kind,
			    description = EXCLUDED.description,
			    source = EXCLUDED.source,
			    params = EXCLUDED.params,
			    egress_allow = EXCLUDED.egress_allow,
			    secret_bindings = EXCLUDED.secret_bindings,
			    trigger_config = EXCLUDED.trigger_config,
			    version = templates.version + 1,
			    updated_at = now()
			WHERE templates.builtin AND (
			    templates.kind IS DISTINCT FROM EXCLUDED.kind OR
			    templates.description IS DISTINCT FROM EXCLUDED.description OR
			    templates.source IS DISTINCT FROM EXCLUDED.source OR
			    templates.params IS DISTINCT FROM EXCLUDED.params OR
			    templates.egress_allow IS DISTINCT FROM EXCLUDED.egress_allow OR
			    templates.secret_bindings IS DISTINCT FROM EXCLUDED.secret_bindings OR
			    templates.trigger_config IS DISTINCT FROM EXCLUDED.trigger_config)
			RETURNING (xmax <> 0)`,
			e.Name, e.Kind, coalesceDescription(e.Description), sources[i], coalesceParams(e.Params),
			coalesceSlice(e.EgressAllow), coalesceSlice(e.SecretBindings), nullableJSON(e.TriggerConfig),
		).Scan(&wasUpdate)
		switch {
		case err == pgx.ErrNoRows:
			res.Unchanged++
		case err != nil:
			return SeedResult{}, fmt.Errorf("templatestore: seed %q: %w", e.Name, err)
		case wasUpdate:
			res.Updated++
		default:
			res.Inserted++
		}
	}

	// Telemetry: flag any expected builtin that did not land as a builtin=true row (should be
	// unreachable given the reserved namespace, but we surface it rather than swallow it, §3.3).
	for _, e := range entries {
		var isBuiltin bool
		if err := tx.QueryRow(ctx, `SELECT builtin FROM templates WHERE name = $1`, e.Name).Scan(&isBuiltin); err != nil || !isBuiltin {
			log.Printf("templatestore: WARN builtin %q not installed as a builtin row (err=%v)", e.Name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return SeedResult{}, err
	}
	return res, nil
}
