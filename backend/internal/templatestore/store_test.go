package templatestore

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB property tests — skipped unless TEST_DATABASE_URL is set (run serially, -p 1, against a postgres
// with migration 0016 applied). Parity faasstore/store_test.go.
func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — DB property tests skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// CASCADE also clears faas_functions (its template_id FK references templates) — fine, these
	// tests never assert on faas rows.
	if _, err := pool.Exec(context.Background(), `TRUNCATE templates RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// --- ValidName (no DB): charset mirror + reserved builtin/ namespace ---

func TestValidName(t *testing.T) {
	for _, n := range []string{"a", "x0", "weather.frame", "ha-clock_2"} {
		if !ValidName(n) {
			t.Errorf("rejected valid %q", n)
		}
	}
	// Gate: operator-Create of a builtin/ name must be rejected (→ 422 at the handler, W2).
	for _, n := range []string{"", "A", "-lead", ".lead", "has space", "ünïcode", "builtin/clock", "builtin/x"} {
		if ValidName(n) {
			t.Errorf("accepted invalid %q", n)
		}
	}
}

// --- CRUD basics incl. unique-name (409) + version bump ---

func TestCRUDRoundTrip(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	id, err := Create(ctx, pool, CreateParams{
		Name:          "my-image",
		Kind:          KindRenderFn,
		Source:        "export default async()=>({image:null})",
		Params:        json.RawMessage(`[{"name":"url","type":"url","required":true}]`),
		EgressAllow:   []string{"images.example.com:443"},
		TriggerConfig: json.RawMessage(`{"mode":"sync"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tmpl, err := Load(ctx, pool, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tmpl.Name != "my-image" || tmpl.Kind != KindRenderFn || tmpl.Version != 1 || tmpl.Builtin {
		t.Fatalf("unexpected template: %+v", tmpl)
	}
	if len(tmpl.EgressAllow) != 1 || tmpl.EgressAllow[0] != "images.example.com:443" {
		t.Fatalf("egress_allow not round-tripped: %+v", tmpl.EgressAllow)
	}

	// Load(unknown) → ErrNotFound.
	if _, err := Load(ctx, pool, 999999); err != ErrNotFound {
		t.Fatalf("Load(unknown): want ErrNotFound, got %v", err)
	}

	// Duplicate name → unique violation (→ 409).
	if _, err := Create(ctx, pool, CreateParams{Name: "my-image", Kind: KindRenderFn, Source: "x"}); err == nil || !IsUniqueViolation(err) {
		t.Fatalf("duplicate name: want unique violation, got %v", err)
	}

	// Update bumps version.
	ok, err := Update(ctx, pool, id, UpdateParams{Source: "export default async()=>({image:1})", Params: json.RawMessage(`[]`)})
	if err != nil || !ok {
		t.Fatalf("Update: ok=%v err=%v", ok, err)
	}
	tmpl2, _ := Load(ctx, pool, id)
	if tmpl2.Version != 2 {
		t.Fatalf("version not bumped: %d", tmpl2.Version)
	}

	// List projection carries no source; keyset walk works.
	sums, err := List(ctx, pool, "", 10, 0)
	if err != nil || len(sums) != 1 || sums[0].ID != id {
		t.Fatalf("List: %+v err=%v", sums, err)
	}

	// Delete.
	ok, err = Delete(ctx, pool, id)
	if err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}
	if _, err := Load(ctx, pool, id); err != ErrNotFound {
		t.Fatalf("after Delete: want ErrNotFound, got %v", err)
	}
}

// --- kind-filtered keyset pagination ---

func TestListKeysetAndKindFilter(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	mk := func(name, kind string) int64 {
		id, err := Create(ctx, pool, CreateParams{Name: name, Kind: kind, Source: "x"})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		return id
	}
	mk("a", KindRenderFn)
	mk("b", KindBerrySnippet)
	mk("c", KindRenderFn)

	// kind filter.
	rf, _ := List(ctx, pool, KindRenderFn, 100, 0)
	if len(rf) != 2 {
		t.Fatalf("render_fn filter: want 2, got %d", len(rf))
	}
	// keyset walk: limit 1, then after the first id.
	page1, _ := List(ctx, pool, "", 1, 0)
	if len(page1) != 1 {
		t.Fatalf("page1: %+v", page1)
	}
	page2, _ := List(ctx, pool, "", 100, page1[0].ID)
	if len(page2) != 2 || page2[0].ID <= page1[0].ID {
		t.Fatalf("keyset walk broken: page2=%+v", page2)
	}
}

// --- Byte guard: char_length<=8191 but octet_length>8191 rejected (K3 poison-pill), Go + DB ---

func TestBerryByteGuard(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	// 3000 '€' = 3000 codepoints (char_length 3000) but 9000 bytes — a multibyte poison pill that a
	// char_length CHECK would let through. len() is bytes, so the Go guard rejects.
	multibyte := strings.Repeat("€", 3000)
	if len(multibyte) <= BerryScriptMax {
		t.Fatalf("test fixture not >8191 bytes: %d", len(multibyte))
	}
	if _, err := Create(ctx, pool, CreateParams{Name: "toolong", Kind: KindBerrySnippet, Source: multibyte}); err != ErrScriptTooLong {
		t.Fatalf("Go byte-guard: want ErrScriptTooLong, got %v", err)
	}

	// The same content as render_fn is fine (the cap is berry_snippet-only).
	if _, err := Create(ctx, pool, CreateParams{Name: "ok-render", Kind: KindRenderFn, Source: multibyte}); err != nil {
		t.Fatalf("render_fn should not be capped: %v", err)
	}

	// DB CHECK is an independent backstop: a raw INSERT bypassing the Go guard must still fail (23514).
	_, err := pool.Exec(ctx, `INSERT INTO templates (name, kind, source) VALUES ('raw-berry', 'berry_snippet', $1)`, multibyte)
	if err == nil || !strings.Contains(err.Error(), "tmpl_berry_len") {
		t.Fatalf("DB CHECK backstop: want tmpl_berry_len violation, got %v", err)
	}
	// A boundary berry_snippet at exactly 8191 bytes is accepted.
	if _, err := Create(ctx, pool, CreateParams{Name: "boundary", Kind: KindBerrySnippet, Source: strings.Repeat("a", BerryScriptMax)}); err != nil {
		t.Fatalf("8191-byte berry_snippet should be accepted: %v", err)
	}
}

// --- SeedBuiltins idempotency: second boot on an unchanged catalog is a no-op (THE gate) ---

func TestSeedIdempotent(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	res1, err := SeedBuiltins(ctx, pool)
	if err != nil {
		t.Fatalf("seed #1: %v", err)
	}
	if res1.Inserted == 0 || res1.Updated != 0 {
		t.Fatalf("seed #1: want inserts, no updates; got %+v", res1)
	}

	// Snapshot version + updated_at of every builtin.
	type snap struct {
		ver int
		upd time.Time
	}
	before := map[string]snap{}
	rows, _ := pool.Query(ctx, `SELECT name, version, updated_at FROM templates WHERE builtin`)
	for rows.Next() {
		var n string
		var s snap
		_ = rows.Scan(&n, &s.ver, &s.upd)
		before[n] = s
	}
	rows.Close()
	if len(before) != res1.Inserted {
		t.Fatalf("builtin count %d != inserted %d", len(before), res1.Inserted)
	}

	// Second seed, no catalog change → pure no-op.
	res2, err := SeedBuiltins(ctx, pool)
	if err != nil {
		t.Fatalf("seed #2: %v", err)
	}
	if res2.Inserted != 0 || res2.Updated != 0 || res2.Unchanged != len(before) {
		t.Fatalf("seed #2 not a no-op: %+v (want Unchanged=%d)", res2, len(before))
	}
	// No version bump, no updated_at churn on any row.
	rows2, _ := pool.Query(ctx, `SELECT name, version, updated_at FROM templates WHERE builtin`)
	for rows2.Next() {
		var n string
		var s snap
		_ = rows2.Scan(&n, &s.ver, &s.upd)
		b := before[n]
		if s.ver != b.ver || !s.upd.Equal(b.upd) {
			t.Fatalf("row %q churned: version %d->%d, updated_at %v->%v", n, b.ver, s.ver, b.upd, s.upd)
		}
	}
	rows2.Close()
}

// --- SeedBuiltins updates exactly the drifted row, leaves the rest untouched ---

func TestSeedUpdatesOnlyChangedRow(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	if _, err := SeedBuiltins(ctx, pool); err != nil {
		t.Fatalf("seed #1: %v", err)
	}

	// Simulate an older catalog / operator drift on ONE builtin: overwrite its source in the DB.
	const target = "builtin/wifi-preset"
	var ver0 int
	if err := pool.QueryRow(ctx, `SELECT version FROM templates WHERE name=$1`, target).Scan(&ver0); err != nil {
		t.Fatalf("target missing: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE templates SET source='# drifted' WHERE name=$1`, target); err != nil {
		t.Fatalf("drift: %v", err)
	}

	res, err := SeedBuiltins(ctx, pool)
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if res.Updated != 1 {
		t.Fatalf("want exactly 1 updated row, got %+v", res)
	}
	// Drifted row restored + version bumped past the drift baseline.
	var src string
	var ver1 int
	_ = pool.QueryRow(ctx, `SELECT source, version FROM templates WHERE name=$1`, target).Scan(&src, &ver1)
	if strings.Contains(src, "drifted") || ver1 != ver0+1 {
		t.Fatalf("target not refreshed: src=%q version %d->%d", src, ver0, ver1)
	}
}

// --- SeedBuiltins never touches an operator-owned row (WHERE builtin fail-closed) ---

func TestSeedLeavesUserRows(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	if _, err := SeedBuiltins(ctx, pool); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A normal operator template (builtin=false).
	uid, err := Create(ctx, pool, CreateParams{Name: "my-thing", Kind: KindRenderFn, Source: "export default 1"})
	if err != nil {
		t.Fatalf("operator create: %v", err)
	}
	var ver0 int
	var upd0 time.Time
	_ = pool.QueryRow(ctx, `SELECT version, updated_at FROM templates WHERE id=$1`, uid).Scan(&ver0, &upd0)

	if _, err := SeedBuiltins(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	var ver1 int
	var upd1 time.Time
	_ = pool.QueryRow(ctx, `SELECT version, updated_at FROM templates WHERE id=$1`, uid).Scan(&ver1, &upd1)
	if ver1 != ver0 || !upd1.Equal(upd0) {
		t.Fatalf("operator row churned by seed: version %d->%d updated_at %v->%v", ver0, ver1, upd0, upd1)
	}
}

// --- Silent-install fail-closed: a squatted builtin=false name is NOT overwritten by the seed
//     (unreachable in prod because ValidName forbids operators minting builtin/, but the mechanism
//     is the WHERE templates.builtin guard — verified here directly). ---

func TestSeedSquatNotOverwritten(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	// Insert a builtin=false row squatting a catalog name (raw, bypassing ValidName).
	if _, err := pool.Exec(ctx,
		`INSERT INTO templates (name, kind, source, builtin) VALUES ('builtin/clock', 'render_fn', 'SQUAT', false)`); err != nil {
		t.Fatalf("squat insert: %v", err)
	}
	if _, err := SeedBuiltins(ctx, pool); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var src string
	var builtin bool
	_ = pool.QueryRow(ctx, `SELECT source, builtin FROM templates WHERE name='builtin/clock'`).Scan(&src, &builtin)
	if src != "SQUAT" || builtin {
		t.Fatalf("seed overwrote a non-builtin squat row: source=%q builtin=%v", src, builtin)
	}
}
