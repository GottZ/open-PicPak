package faascore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/bwry"
	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
	"github.com/open-picpak/backend/internal/sealbox"
	"github.com/open-picpak/backend/internal/secrets"
)

const keyA = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0" // 32 bytes hex

// --- secret resolution (T9/T10): DB + sealbox, skipped unless TEST_DATABASE_URL is set ---

func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — secret DB tests skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE secrets`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func testBox(t *testing.T) *sealbox.Box {
	t.Helper()
	b, err := sealbox.New(keyA, "")
	if err != nil {
		t.Fatalf("sealbox: %v", err)
	}
	return b
}

func TestResolveSecrets(t *testing.T) {
	pool := dbPool(t)
	box := testBox(t)
	ctx := context.Background()
	if _, err := secrets.PutSecret(ctx, pool, box, "ha_token", []byte("VALUE-1")); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	// T10 least-privilege: only bound names are resolved, each to its value.
	m, serr := resolveSecrets(ctx, pool, box, []string{"ha_token"}, SecretsReal)
	if serr != nil || len(m) != 1 || m["ha_token"] != "VALUE-1" {
		t.Fatalf("bound resolve: m=%v serr=%v", m, serr)
	}

	// T9(a): a bound-but-missing secret is injected ABSENT (fail-open), the render proceeds.
	m, serr = resolveSecrets(ctx, pool, box, []string{"ha_token", "missing"}, SecretsReal)
	if serr != nil {
		t.Fatalf("missing secret should be fail-open, got %v", serr)
	}
	if _, present := m["missing"]; present {
		t.Errorf("missing secret should be absent, got %v", m)
	}
	if m["ha_token"] != "VALUE-1" {
		t.Errorf("present secret dropped: %v", m)
	}

	// T9(b): a tampered ciphertext is a HARD sealbox.Open fault → abort (kind "secret"), NEVER
	// masked as not-found.
	if _, err := pool.Exec(ctx,
		`UPDATE secrets SET ciphertext = set_byte(ciphertext, 0, (get_byte(ciphertext,0) + 1) % 256) WHERE name = 'ha_token'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	m, serr = resolveSecrets(ctx, pool, box, []string{"ha_token"}, SecretsReal)
	if serr == nil || serr.Kind != "secret" || m != nil {
		t.Fatalf("tampered secret: want hard 'secret' err, got m=%v serr=%v", m, serr)
	}

	// stub mode never touches the DB and yields placeholders (Doc 25 test-run).
	ms, serr := resolveSecrets(ctx, pool, box, []string{"x"}, SecretsStub)
	if serr != nil || ms["x"] != "<secret:x>" {
		t.Fatalf("stub: ms=%v serr=%v", ms, serr)
	}
}

// --- M4 parity (the real cross-language gate): the Go supervisor drives the REAL Bun worker ---

func startWorker(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not on PATH — M4 parity test skipped")
	}
	dir, err := filepath.Abs("../../worker")
	if err != nil {
		t.Fatalf("worker dir: %v", err)
	}
	sock := filepath.Join(t.TempDir(), "m4.sock")
	cmd := exec.Command("bun", "daemon.ts")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FAAS_M4_SOCK="+sock)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(sock); err == nil {
			return sock
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("worker M4 socket never appeared")
	return ""
}

func renderCtx() faasproto.RequestCtx {
	return faasproto.RequestCtx{Serial: "S1", Channel: "c", Trigger: faasproto.Trigger{Type: "render"}, Now: "2026-07-01T00:00:00Z"}
}

func rOpts(sock string) RenderOpts {
	return RenderOpts{Secrets: SecretsReal, Limits: faasproto.Limits{TimeoutMs: 8000, MemMB: 256}, M4Sock: sock, Timeout: 30 * time.Second}
}

func TestM4Parity_RealWorker(t *testing.T) {
	sock := startWorker(t)
	fn := &faasstore.Function{
		ID: 1, Version: 1,
		Source: "export default async (ctx, cap) => ({ image: cap.sharp({create:{width:400,height:300,channels:3,background:{r:10,g:20,b:30}}}) })",
	}
	res := renderOnce(context.Background(), nil, nil, fn, renderCtx(), rOpts(sock))
	if res.Err != nil {
		t.Fatalf("renderOnce err: %+v", res.Err)
	}
	if len(res.Packed) != bwry.PackedSize {
		t.Fatalf("packed %d != %d", len(res.Packed), bwry.PackedSize)
	}
	if !res.Meta.OK {
		t.Fatalf("meta not ok: %+v", res.Meta)
	}
	// The frame is a solid colour (10,20,30) -> a single BWRY code -> every packed byte identical.
	if res.Packed[0] != res.Packed[1] || res.Packed[100] != res.Packed[0] {
		t.Errorf("expected a uniform packed frame, got varied bytes")
	}
}

func TestM4Parity_WorkerErrors(t *testing.T) {
	sock := startWorker(t)
	cases := []struct {
		name, source, wantKind string
	}{
		{"throw", "export default async () => { throw new Error('boom'); }", "throw"},
		{"offsize", "export default async (ctx, cap) => ({ image: cap.sharp({create:{width:100,height:100,channels:3,background:{r:0,g:0,b:0}}}) })", "offsize"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := &faasstore.Function{ID: 1, Version: 1, Source: tc.source}
			res := renderOnce(context.Background(), nil, nil, fn, renderCtx(), rOpts(sock))
			if res.Err == nil || res.Err.Kind != tc.wantKind {
				t.Fatalf("want err kind %q, got %+v", tc.wantKind, res.Err)
			}
			if res.Packed != nil {
				t.Errorf("error render should not produce a packed frame")
			}
		})
	}
}
