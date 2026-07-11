package ingestcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/otaticket"
)

// --- DB property tests for the OTA serve path (skipped unless TEST_DATABASE_URL is set) ---

const strongKey = "0123456789abcdef0123456789abcdef" // 32 B — arms the serve

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
	// FK-safe reset (channels FK-references firmware_versions, so no TRUNCATE CASCADE on the FW table).
	// `logs` has NO FK on serial, so `DELETE FROM devices` does NOT cascade it (device_log_cursor /
	// device_log_fragment DO cascade) — truncate it explicitly so A21 reassembly counts start clean.
	for _, stmt := range []string{
		`TRUNCATE logs`,
		`TRUNCATE rollout_targets`,
		`UPDATE channels SET default_version = NULL`,
		`DELETE FROM faas_functions`, // FK CASCADE drops device_render_binding + faas_frame_lastgood
		`DELETE FROM devices`,
		`DELETE FROM firmware_versions`,
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("reset %q: %v", stmt, err)
		}
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedServeable writes a real blob, registers a firmware version whose sha256 is the ACTUAL hash of
// those bytes (so the served SHA matches what the FW would compute, D20.7), points the stable channel
// default at it, and returns an armed Server plus the version/bytes/sha.
func seedServeable(t *testing.T, pool *pgxpool.Pool) (s *Server, version string, blob []byte, sha string) {
	t.Helper()
	dir := t.TempDir()
	blob = []byte("\x00FIRMWARE-IMAGE-BYTES\xff")
	sum := sha256.Sum256(blob)
	sha = hex.EncodeToString(sum[:])
	version = "1.4.0"
	const blobName = "fw-1_4_0.bin"
	if err := os.WriteFile(filepath.Join(dir, blobName), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `INSERT INTO firmware_versions (version, sha256, blob_path, size_bytes) VALUES ($1,$2,$3,$4)`,
		version, sha, blobName, len(blob))
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('dev1','stable')`)
	mustExec(t, pool, `UPDATE channels SET default_version=$1 WHERE name='stable'`, version)
	s = &Server{pool: pool, otaServeEnabled: true, otaKey: strongKey, otaTTL: time.Minute, fwBlobDir: dir}
	return s, version, blob, sha
}

func fwReq(ticket string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/TOKEN/firmware.bin", nil)
	if ticket != "" {
		r.Header.Set("X-Firmware-Ticket", ticket)
	}
	return r
}

// TestHandleFirmware_DB exercises the binary serve gates: anonymous/expired/forged → 403 (T1/T2/T3),
// unknown version → 404, default-off → 404 (D20.11), weak key → 503 (D20.9/T13), and the happy path
// streaming the blob with the fail-closed SHA carrier (D20.6/D20.7).
func TestHandleFirmware_DB(t *testing.T) {
	pool := dbPool(t)
	s, version, blob, sha := seedServeable(t, pool)

	t.Run("happy path serves blob + sha", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleFirmware(w, fwReq(otaticket.Mint(s.otaKey, "dev1", version, time.Minute)))
		if w.Code != http.StatusOK {
			t.Fatalf("code=%d, want 200", w.Code)
		}
		if got := w.Header().Get("X-Firmware-SHA256"); got != sha {
			t.Errorf("X-Firmware-SHA256=%q, want %q", got, sha)
		}
		if !bytes.Equal(w.Body.Bytes(), blob) {
			t.Errorf("served body != blob")
		}
	})

	t.Run("no ticket -> 403 (T1)", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleFirmware(w, fwReq(""))
		if w.Code != http.StatusForbidden {
			t.Errorf("code=%d, want 403", w.Code)
		}
	})

	t.Run("forged ticket (wrong key) -> 403 (T3)", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleFirmware(w, fwReq(otaticket.Mint("WRONGWRONGWRONGWRONGWRONGWRONGWR", "dev1", version, time.Minute)))
		if w.Code != http.StatusForbidden {
			t.Errorf("code=%d, want 403", w.Code)
		}
	})

	t.Run("expired ticket -> 403 (T2)", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleFirmware(w, fwReq(otaticket.Mint(s.otaKey, "dev1", version, -time.Second)))
		if w.Code != http.StatusForbidden {
			t.Errorf("code=%d, want 403", w.Code)
		}
	})

	t.Run("valid ticket, unknown version -> 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleFirmware(w, fwReq(otaticket.Mint(s.otaKey, "dev1", "9.9.9-ghost", time.Minute)))
		if w.Code != http.StatusNotFound {
			t.Errorf("code=%d, want 404", w.Code)
		}
	})

	t.Run("default-off -> 404 even with valid ticket (D20.11)", func(t *testing.T) {
		off := *s
		off.otaServeEnabled = false
		w := httptest.NewRecorder()
		off.handleFirmware(w, fwReq(otaticket.Mint(s.otaKey, "dev1", version, time.Minute)))
		if w.Code != http.StatusNotFound {
			t.Errorf("code=%d, want 404", w.Code)
		}
	})

	t.Run("enabled + weak key -> 503 (D20.9/T13)", func(t *testing.T) {
		weak := *s
		weak.otaKey = "tooshort"
		w := httptest.NewRecorder()
		weak.handleFirmware(w, fwReq("anything"))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("code=%d, want 503", w.Code)
		}
	})
}

// TestInjectOTASignal_DB proves the /pp signal: armed+resolves → version + a verifiable ticket;
// disarmed/unknown/weak → no headers; and the fail-open property (D20.8) — a resolve error drops the
// headers but never errors the caller (handleIngest still writes 200).
func TestInjectOTASignal_DB(t *testing.T) {
	pool := dbPool(t)
	s, version, _, _ := seedServeable(t, pool)

	t.Run("armed + resolves -> version + valid ticket", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.injectOTASignal(context.Background(), w, "dev1")
		if got := w.Header().Get("X-Firmware-Version"); got != version {
			t.Errorf("X-Firmware-Version=%q, want %q", got, version)
		}
		sn, ver, err := otaticket.Verify(s.otaKey, w.Header().Get("X-Firmware-Ticket"))
		if err != nil || sn != "dev1" || ver != version {
			t.Errorf("minted ticket: sn=%q ver=%q err=%v", sn, ver, err)
		}
	})

	t.Run("unknown serial -> no headers", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.injectOTASignal(context.Background(), w, "ghost")
		if w.Header().Get("X-Firmware-Version") != "" || w.Header().Get("X-Firmware-Ticket") != "" {
			t.Error("OTA headers set for an unknown serial")
		}
	})

	t.Run("serve disabled -> no headers (D20.11)", func(t *testing.T) {
		off := *s
		off.otaServeEnabled = false
		w := httptest.NewRecorder()
		off.injectOTASignal(context.Background(), w, "dev1")
		if w.Header().Get("X-Firmware-Version") != "" {
			t.Error("OTA headers set while serving disabled")
		}
	})

	t.Run("weak key -> no headers (D20.9)", func(t *testing.T) {
		weak := *s
		weak.otaKey = "tooshort"
		w := httptest.NewRecorder()
		weak.injectOTASignal(context.Background(), w, "dev1")
		if w.Header().Get("X-Firmware-Version") != "" {
			t.Error("OTA headers set with a weak key")
		}
	})

	t.Run("resolve error -> fail-open, no headers (D20.8)", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // a cancelled ctx forces ResolveTarget's query to error
		w := httptest.NewRecorder()
		s.injectOTASignal(ctx, w, "dev1")
		if w.Header().Get("X-Firmware-Version") != "" || w.Header().Get("X-Firmware-Ticket") != "" {
			t.Error("OTA headers set on a resolve error — not fail-open")
		}
	})
}
