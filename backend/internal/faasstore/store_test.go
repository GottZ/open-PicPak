package faasstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB property tests — skipped unless TEST_DATABASE_URL is set (run in the e2e gate against
// an ephemeral postgres with migration 0009 applied).
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
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE faas_functions, device_render_binding, faas_frame_lastgood RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"a", "x0", "weather.frame", "ha-clock_2"} {
		if !ValidName(n) {
			t.Errorf("rejected valid %q", n)
		}
	}
	for _, n := range []string{"", "A", "-lead", ".lead", "has space", "ünïcode"} {
		if ValidName(n) {
			t.Errorf("accepted invalid %q", n)
		}
	}
}

func TestCRUDRoundTrip(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	id, err := Create(ctx, pool, CreateParams{
		Name:           "weather",
		Source:         "export default async()=>({image:null})",
		TriggerType:    TriggerRender,
		TriggerConfig:  json.RawMessage(`{"mode":"sync","ttl_s":120}`),
		SecretBindings: []string{"ha_token"},
		EgressAllow:    []string{"ha.local:8123"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	f, err := LoadFunction(ctx, pool, id)
	if err != nil {
		t.Fatalf("LoadFunction: %v", err)
	}
	if f.Name != "weather" || f.Version != 1 || f.Enabled {
		t.Fatalf("unexpected function: %+v", f)
	}
	if len(f.SecretBindings) != 1 || f.SecretBindings[0] != "ha_token" {
		t.Fatalf("bindings roundtrip: %v", f.SecretBindings)
	}
	if len(f.EgressAllow) != 1 || f.EgressAllow[0] != "ha.local:8123" {
		t.Fatalf("egress roundtrip: %v", f.EgressAllow)
	}
	var cfg map[string]any
	if err := json.Unmarshal(f.TriggerConfig, &cfg); err != nil || cfg["mode"] != "sync" {
		t.Fatalf("config roundtrip: %s err=%v", f.TriggerConfig, err)
	}

	// Update bumps version + replaces the body (K7 cache-bust).
	if ok, err := Update(ctx, pool, id, UpdateParams{
		Source: "export default async()=>({image:1})", TriggerConfig: json.RawMessage(`{"mode":"prerender"}`),
		SecretBindings: []string{"ha_token", "openweather"}, EgressAllow: []string{},
	}); err != nil || !ok {
		t.Fatalf("Update: ok=%v err=%v", ok, err)
	}
	if v, err := Version(ctx, pool, id); err != nil || v != 2 {
		t.Fatalf("Version after update: v=%d err=%v", v, err)
	}
	f2, _ := LoadFunction(ctx, pool, id)
	if len(f2.SecretBindings) != 2 || len(f2.EgressAllow) != 0 {
		t.Fatalf("update body: bindings=%v egress=%v", f2.SecretBindings, f2.EgressAllow)
	}

	// SetEnabled toggles.
	if ok, err := SetEnabled(ctx, pool, id, true); err != nil || !ok {
		t.Fatalf("SetEnabled: %v %v", ok, err)
	}
	if f3, _ := LoadFunction(ctx, pool, id); !f3.Enabled {
		t.Fatal("enabled not set")
	}

	// List projection: no source field, correct summary.
	list, err := ListFunctions(ctx, pool)
	if err != nil || len(list) != 1 || list[0].Name != "weather" || list[0].Version != 2 || !list[0].Enabled {
		t.Fatalf("ListFunctions: %+v err=%v", list, err)
	}

	// Name uniqueness → unique violation → 409 mapping.
	if _, err := Create(ctx, pool, CreateParams{Name: "weather", Source: "x"}); !IsUniqueViolation(err) {
		t.Fatalf("duplicate name: want unique violation, got %v", err)
	}

	// Not-found paths.
	if _, err := LoadFunction(ctx, pool, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadFunction missing: want ErrNotFound, got %v", err)
	}
}

func TestBindingFanOutAndLastGood(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	id, err := Create(ctx, pool, CreateParams{Name: "clock", Source: "x", TriggerType: TriggerSchedule})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Bind two serials → fan-out returns both (D24.12); one serial rebinds (upsert).
	for _, s := range []string{"SN-A", "SN-B"} {
		if err := BindDevice(ctx, pool, s, id); err != nil {
			t.Fatalf("BindDevice %s: %v", s, err)
		}
	}
	if err := BindDevice(ctx, pool, "SN-A", id); err != nil { // upsert, no dup
		t.Fatalf("rebind: %v", err)
	}
	serials, err := BoundSerials(ctx, pool, id)
	if err != nil || len(serials) != 2 {
		t.Fatalf("BoundSerials: %v err=%v", serials, err)
	}
	if fid, ok, err := BoundFunctionID(ctx, pool, "SN-A"); err != nil || !ok || fid != id {
		t.Fatalf("BoundFunctionID: %d ok=%v err=%v", fid, ok, err)
	}
	if _, ok, _ := BoundFunctionID(ctx, pool, "SN-UNBOUND"); ok {
		t.Fatal("unbound serial reported bound")
	}

	// Last-good roundtrip (30000 B).
	packed := bytes.Repeat([]byte{0xA5}, 30000)
	if err := LastGoodPut(ctx, pool, "SN-A", id, packed, "ok"); err != nil {
		t.Fatalf("LastGoodPut: %v", err)
	}
	if err := LastGoodPut(ctx, pool, "SN-A", id, packed, "stale"); err != nil { // upsert
		t.Fatalf("LastGoodPut upsert: %v", err)
	}
	lg, err := LastGoodGet(ctx, pool, "SN-A", id)
	if err != nil || lg.Status != "stale" || !bytes.Equal(lg.Packed, packed) {
		t.Fatalf("LastGoodGet: status=%q len=%d err=%v", lg.Status, len(lg.Packed), err)
	}
	if _, err := LastGoodGet(ctx, pool, "SN-B", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LastGoodGet missing: want ErrNotFound, got %v", err)
	}

	// DeleteDeviceBindings (the W5 device-delete tx seam): removes SN-A's binding + last-good.
	if err := DeleteDeviceBindings(ctx, pool, "SN-A"); err != nil {
		t.Fatalf("DeleteDeviceBindings: %v", err)
	}
	if serials, _ := BoundSerials(ctx, pool, id); len(serials) != 1 || serials[0] != "SN-B" {
		t.Fatalf("after DeleteDeviceBindings: %v", serials)
	}
	if _, err := LastGoodGet(ctx, pool, "SN-A", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("last-good survived DeleteDeviceBindings: %v", err)
	}

	// Function delete CASCADEs remaining bindings + last-good.
	if err := LastGoodPut(ctx, pool, "SN-B", id, packed, "ok"); err != nil {
		t.Fatalf("seed SN-B last-good: %v", err)
	}
	if ok, err := Delete(ctx, pool, id); err != nil || !ok {
		t.Fatalf("Delete: %v %v", ok, err)
	}
	if serials, _ := BoundSerials(ctx, pool, id); len(serials) != 0 {
		t.Fatalf("bindings survived function delete: %v", serials)
	}
	if _, err := LastGoodGet(ctx, pool, "SN-B", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("last-good survived function delete: %v", err)
	}
}

// K8: the webhook token hash is readable ONLY via WebhookTokenSHA — never on Function, never
// in a list. A non-webhook function returns ErrNoWebhookToken.
func TestWebhookTokenIsolation(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	sha := bytes.Repeat([]byte{0x7F}, 32)
	id, err := Create(ctx, pool, CreateParams{
		Name: "hook", Source: "x", TriggerType: TriggerWebhook, WebhookTokenSHA: sha,
	})
	if err != nil {
		t.Fatalf("Create webhook: %v", err)
	}

	got, err := WebhookTokenSHA(ctx, pool, id)
	if err != nil || !bytes.Equal(got, sha) {
		t.Fatalf("WebhookTokenSHA: got %x err=%v", got, err)
	}

	// The render/list shapes must not carry the hash — proven structurally by marshalling.
	f, _ := LoadFunction(ctx, pool, id)
	fb, _ := json.Marshal(f)
	if bytes.Contains(fb, []byte("7f7f7f")) || bytes.Contains(bytes.ToLower(fb), []byte("webhook_token")) {
		t.Fatalf("Function JSON leaked the token hash: %s", fb)
	}
	list, _ := ListFunctions(ctx, pool)
	lb, _ := json.Marshal(list)
	if bytes.Contains(bytes.ToLower(lb), []byte("token")) {
		t.Fatalf("Summary JSON leaked a token field: %s", lb)
	}

	// Rotate.
	sha2 := bytes.Repeat([]byte{0x11}, 32)
	if ok, err := SetWebhookToken(ctx, pool, id, sha2); err != nil || !ok {
		t.Fatalf("SetWebhookToken: %v %v", ok, err)
	}
	if got, _ := WebhookTokenSHA(ctx, pool, id); !bytes.Equal(got, sha2) {
		t.Fatalf("rotate: got %x", got)
	}

	// A non-webhook function has no token.
	id2, _ := Create(ctx, pool, CreateParams{Name: "plain", Source: "x"})
	if _, err := WebhookTokenSHA(ctx, pool, id2); !errors.Is(err, ErrNoWebhookToken) {
		t.Fatalf("non-webhook token: want ErrNoWebhookToken, got %v", err)
	}
}
