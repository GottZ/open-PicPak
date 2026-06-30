package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/open-picpak/backend/internal/otaticket"
	"github.com/open-picpak/backend/internal/rollout"
)

// otaArmed reports whether the binary serve is fully on: the operator flipped OTA_SERVE_ENABLED AND
// the ticket key is strong enough to sign with (D20.9/D20.11). Both injectOTASignal and handleFirmware
// gate on this — OTA is either fully on (version + downloadable ticket) or fully off (no half-state
// where a device sees a version it can never fetch).
func (s *server) otaArmed() bool {
	return s.otaServeEnabled && otaticket.KeyStrong(s.otaKey)
}

// injectOTASignal sets the OTA response headers on the /pp push AFTER the telemetry tx has committed,
// BEFORE the body is written. It is FAIL-OPEN (D20.8): any resolve error drops the headers and the
// caller still returns 200 "ok" (telemetry is already durable; the device retries the signal next
// wake — no telemetry loss). When the serve is disarmed (default-off or weak key) it sets nothing —
// a version header with no fetchable binary would be a useless half-state (D20.11).
//
// NB (masterplan K11): when A21's log-ack rides this same post-commit /pp path, X-Log-Ack-Offset MUST
// be set BEFORE this call — this fail-open return must not pre-empt the ack, or a committed log delta
// is never acked (FW re-push loop → ring overrun). A20 sets no other post-commit header, so order is
// trivially correct here; A21 inserts the ack ahead of this.
func (s *server) injectOTASignal(ctx context.Context, w http.ResponseWriter, serial string) {
	if !s.otaArmed() {
		return // serve disabled → no signal (D20.11)
	}
	r, err := rollout.ResolveTarget(ctx, s.pool, serial) // routes by server-side devices.channel (D20.5)
	if err != nil {
		log.Printf("ota resolve %s: %v", serial, err)
		return // FAIL-OPEN (D20.8) — no headers, caller still writes 200
	}
	if r.Source == rollout.SourceNone || r.Version == "" {
		return // nothing staged for this device
	}
	w.Header().Set("X-Firmware-Version", r.Version) // the version gate the FW strncmp's (net.c:511)
	// TODO(hotp-serial-source): `serial` is the spoofable /pp ?sn (shared INGEST_TOKEN, no per-device
	// auth). Once /pp is HOTP-gated it MUST come from the auth context — else a token-holder mints a
	// victim's OTA ticket (masterplan R4/§5; grep-collected with Doc 21's same marker).
	w.Header().Set("X-Firmware-Ticket", otaticket.Mint(s.otaKey, serial, r.Version, s.otaTTL)) // D20.4
}

// handleFirmware serves the resolved firmware blob under the download ticket (§4.3). Fail-CLOSED
// (D20.8): no/expired/forged ticket → 403; the SHA the FW verifies fail-closed rides this response,
// not /pp (D20.6). Default-off and key-gated (D20.11/D20.9): disabled → 404 (looks absent), enabled
// with a weak key → 503 (an operator misconfig to fix, never a forgeable serve).
func (s *server) handleFirmware(w http.ResponseWriter, r *http.Request) {
	if !s.otaServeEnabled {
		http.NotFound(w, r) // D20.11 default-off: the route reads as absent
		return
	}
	if !otaticket.KeyStrong(s.otaKey) {
		http.Error(w, "", http.StatusServiceUnavailable) // D20.9: serving on, key weak → fail-closed
		return
	}
	token := r.Header.Get("X-Firmware-Ticket")
	if token == "" {
		http.Error(w, "", http.StatusForbidden) // T1 — no anonymous binary
		return
	}
	sn, version, err := otaticket.Verify(s.otaKey, token)
	if err != nil {
		http.Error(w, "", http.StatusForbidden) // T2/T3 expired/forged — constant message, no detail
		return
	}

	// Look up the ticket's bound version (NOT a re-resolve — the ticket IS the capability, §4.2).
	var sha, blobPath string
	var size int64
	switch err := s.pool.QueryRow(r.Context(),
		`SELECT sha256, blob_path, size_bytes FROM firmware_versions WHERE version = $1`, version).
		Scan(&sha, &blobPath, &size); {
	case errors.Is(err, pgx.ErrNoRows):
		http.Error(w, "", http.StatusNotFound)
		return
	case err != nil:
		log.Printf("ota firmware lookup %q: %v", version, err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Path-traversal guard: blob_path is operator-written (admin register), but Base strips any
	// directory components so a crafted row can never escape FW_BLOB_DIR (host-volume topology, §4.7).
	full := filepath.Join(s.fwBlobDir, filepath.Base(blobPath))
	f, err := os.Open(full)
	if err != nil {
		// register co-writes row+blob (§4.4.1), so a missing blob is an operator/storage fault, not
		// a normal serve — alert via the log, fail-closed 500 (never a partial/empty 200 binary).
		log.Printf("ota blob missing %q (version %q): %v", full, version, err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	defer f.Close() //nolint:errcheck // read-only

	w.Header().Set("X-Firmware-SHA256", sha) // D20.6 — the fail-closed carrier the FW enforces (ota.c:223)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, f); err != nil {
		log.Printf("ota blob stream %q: %v", version, err) // client/transport drop; headers already sent
	}

	// masterplan K12: a log ride-along on the OTA download is the brownout-during-OTA diagnostic window.
	// Persist it as ONE logs.source='ota-snapshot' row (seq/offset NULL, no reassembly) — the statement
	// lives in A21's logingest, this handler only invokes it. Best-effort: the binary already streamed
	// (headers sent), so a snapshot-write failure is logged, never surfaced. Serial comes from the
	// VERIFIED ticket (sn), not a spoofable query param.
	if body := r.Header.Get("X-Picpak-Log"); body != "" {
		if err := insertSnapshot(r.Context(), s.pool, sn, ni(r.URL.Query().Get("bc")), body); err != nil {
			log.Printf("ota log snapshot %s (version %q): %v", sn, version, err)
		}
	}
}
