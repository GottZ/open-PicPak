package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/otaticket"
)

// A21 W1 reassembly properties as negatively-probed DB tests (skipped unless TEST_DATABASE_URL is set;
// run in the e2e gate). Each test states the bug its red would be. dbPool / mustExec / seedServeable /
// fwReq / strongKey are shared with ota_test.go (same package).

func logServer(pool *pgxpool.Pool, maxFrame int64) *server {
	if maxFrame == 0 {
		maxFrame = 65536
	}
	return &server{pool: pool, logFrameMax: maxFrame}
}

func seedDevice(t *testing.T, pool *pgxpool.Pool, serial string) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO devices (serial) VALUES ($1) ON CONFLICT DO NOTHING`, serial)
}

// pushLogErr runs one reassembly push in its own committed tx — the real /pp log step. Returns the error
// so it is safe to call from goroutines (T8); pushLog is the t.Fatal-on-error convenience wrapper.
func pushLogErr(s *server, serial string, bc, off int64, payload string) (int64, bool, error) {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	ackOff, acked, err := reassembleLog(ctx, tx, serial, logFrame{epoch: bc, end: off}, payload, s.logFrameMax)
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return ackOff, acked, nil
}

func pushLog(t *testing.T, s *server, serial string, bc, off int64, payload string) (int64, bool) {
	t.Helper()
	ackOff, acked, err := pushLogErr(s, serial, bc, off, payload)
	if err != nil {
		t.Fatalf("push (bc=%d off=%d): %v", bc, off, err)
	}
	return ackOff, acked
}

func readCursor(t *testing.T, pool *pgxpool.Pool, serial string) (epoch, ackOff, lastSeq int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT epoch, ack_off, last_seq FROM device_log_cursor WHERE serial=$1`, serial).
		Scan(&epoch, &ackOff, &lastSeq); err != nil {
		t.Fatalf("read cursor %s: %v", serial, err)
	}
	return
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// ppReq builds a /pp telemetry GET carrying a log ride-along (sn + an expected key + bc/off + the header).
func ppReq(serial string, bc, off int64, logPayload string) *http.Request {
	u := fmt.Sprintf("/T/pp?sn=%s&v=3900&p=80&bc=%d&off=%d", serial, bc, off)
	r := httptest.NewRequest(http.MethodGet, u, nil)
	if logPayload != "" {
		r.Header.Set("X-Picpak-Log", logPayload)
	}
	return r
}

// T1 — no loss on a failed ack: a push whose tx ROLLS BACK (server 500 / drop) does NOT advance the
// durable cursor, and the same delta re-pushed lands in full. Red: the cursor advanced before COMMIT.
func TestReassemble_NoLossOnFailedAck_T1(t *testing.T) {
	pool := dbPool(t)
	s := logServer(pool, 0)
	seedDevice(t, pool, "d1")

	pushLog(t, s, "d1", 1, 100, "a|b|")
	if e, a, _ := readCursor(t, pool, "d1"); e != 1 || a != 100 {
		t.Fatalf("after push1: epoch=%d ack=%d, want 1/100", e, a)
	}

	// reassemble a further delta but ROLL BACK instead of commit (the 500 / drop path)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reassembleLog(ctx, tx, "d1", logFrame{epoch: 1, end: 250}, "c|d|", s.logFrameMax); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)

	if e, a, _ := readCursor(t, pool, "d1"); e != 1 || a != 100 {
		t.Fatalf("after rollback: epoch=%d ack=%d, want UNCHANGED 1/100 (no loss)", e, a)
	}
	// the same delta committed now lands in full
	ack, acked := pushLog(t, s, "d1", 1, 250, "c|d|")
	if !acked || ack != 250 {
		t.Fatalf("re-push: ack=%d acked=%v, want 250/true", ack, acked)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM device_log_fragment WHERE serial='d1' AND end_off=250`); n != 1 {
		t.Fatalf("re-pushed fragment count=%d, want 1", n)
	}
}

// T2 — durable-before-ack: a committed push echoes X-Log-Ack-Offset; a push whose tx fails (cancelled
// ctx) returns 500 and sets NO ack header (the value is never echoed pre-commit). Red: header set from
// the pre-commit value → the FW believes persisted, data gone.
func TestHandleIngest_DurableBeforeAck_T2(t *testing.T) {
	pool := dbPool(t)
	s := logServer(pool, 0)

	w := httptest.NewRecorder()
	s.handleIngest(w, ppReq("d2", 1, 100, "a|b|"))
	if w.Code != http.StatusOK {
		t.Fatalf("happy code=%d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Log-Ack-Offset"); got != "100" {
		t.Fatalf("happy ack header=%q, want 100", got)
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel() // a cancelled ctx fails the tx Begin/Exec → ingest errors → 500
	w2 := httptest.NewRecorder()
	s.handleIngest(w2, ppReq("d2", 1, 200, "c|d|").WithContext(cctx))
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("failed-push code=%d, want 500", w2.Code)
	}
	if got := w2.Header().Get("X-Log-Ack-Offset"); got != "" {
		t.Fatalf("ack header set on a failed push: %q", got)
	}
}

// T3 — gap = bc increment, cursor RESETS. A second push with bc > cursor.epoch flags gap=true on the
// logs row + fragment, advances the epoch, and RESETS ack_off to the new push's end — NOT
// GREATEST(old_high_water, end). Red: epoch stuck / gap=false, OR ack_off keeps the stale high-water.
func TestReassemble_GapResetsCursor_T3(t *testing.T) {
	pool := dbPool(t)
	s := logServer(pool, 0)
	seedDevice(t, pool, "d3")

	pushLog(t, s, "d3", 7, 5000, "boot7|")
	ack, acked := pushLog(t, s, "d3", 8, 300, "boot8|")
	if !acked || ack != 300 {
		t.Fatalf("gap push ack=%d acked=%v, want 300/true (RESET, not GREATEST=5000)", ack, acked)
	}
	if e, a, _ := readCursor(t, pool, "d3"); e != 8 || a != 300 {
		t.Fatalf("cursor epoch=%d ack=%d, want 8/300", e, a)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM logs WHERE serial='d3' AND boot_count=8 AND gap=true`); n != 1 {
		t.Fatalf("gap logs row count=%d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM device_log_fragment WHERE serial='d3' AND epoch=8 AND gap=true`); n != 1 {
		t.Fatalf("gap fragment count=%d, want 1", n)
	}
}

// T4 — suspect = rollback without reboot. Same epoch, off < cursor.ack_off → suspect=true AND the cursor
// does NOT regress. Red: the cursor takes the lower off → a permanent backward jump / replays.
func TestReassemble_SuspectRollback_T4(t *testing.T) {
	pool := dbPool(t)
	s := logServer(pool, 0)
	seedDevice(t, pool, "d4")

	pushLog(t, s, "d4", 5, 1000, "x|")
	ack, acked := pushLog(t, s, "d4", 5, 400, "y|") // off 400 < ack 1000, same epoch
	if !acked {
		t.Fatal("a suspect push still echoes the true high-water")
	}
	if ack != 1000 {
		t.Fatalf("suspect ack=%d, want UNCHANGED 1000 (no regress)", ack)
	}
	if e, a, _ := readCursor(t, pool, "d4"); e != 5 || a != 1000 {
		t.Fatalf("cursor epoch=%d ack=%d, want unchanged 5/1000", e, a)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM logs WHERE serial='d4' AND suspect=true`); n != 1 {
		t.Fatalf("suspect logs row=%d, want 1", n)
	}
}

// T5 — idempotent re-push, even with a slid base. The same end_off re-pushed (incl. an explicit frame
// whose base slid on an overrun) is exactly ONE fragment + ONE logs row — the PK is (serial, epoch,
// end_off). Red: PK on start_off, or an unconditional logs insert → duplicate/overlapping rows.
func TestReassemble_IdempotentRepush_T5(t *testing.T) {
	pool := dbPool(t)
	s := logServer(pool, 0)
	seedDevice(t, pool, "d5")

	pushLog(t, s, "d5", 2, 100, "a|b|") // epoch 2 (gap from 0), end_off 100
	pushLog(t, s, "d5", 2, 100, "a|b|") // exact re-push → ON CONFLICT no-op

	// an explicit frame re-pushing end=100 under a WILDLY different base must still collide on end_off
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	slid := int64(37)
	if _, _, err := reassembleLog(ctx, tx, "d5", logFrame{epoch: 2, end: 100, base: &slid, explicit: true}, "a|b|", s.logFrameMax); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)

	if n := countRows(t, pool, `SELECT count(*) FROM device_log_fragment WHERE serial='d5' AND epoch=2 AND end_off=100`); n != 1 {
		t.Fatalf("fragment count=%d, want 1 (idempotent on end_off, not start_off)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM logs WHERE serial='d5'`); n != 1 {
		t.Fatalf("logs row count=%d, want 1 (D21.8 one row per delta)", n)
	}
}

// T6 — same-epoch ack is the max, never lowered. Ascending pushes raise ack_off; a later lower off does
// NOT pull it back. Red: read-modify-write lost update, OR an unconditional GREATEST across an advance.
func TestReassemble_GreatestSameEpoch_T6(t *testing.T) {
	pool := dbPool(t)
	s := logServer(pool, 0)
	seedDevice(t, pool, "d6")

	pushLog(t, s, "d6", 3, 200, "a|") // epoch 3 (gap), ack 200
	if ack, _ := pushLog(t, s, "d6", 3, 500, "b|"); ack != 500 {
		t.Fatalf("ascending ack=%d, want 500", ack)
	}
	if ack, _ := pushLog(t, s, "d6", 3, 350, "c|"); ack != 500 { // 350 < 500 → suspect, ack holds
		t.Fatalf("lower-off ack=%d, want held at 500 (GREATEST, no lowering)", ack)
	}
	if _, a, _ := readCursor(t, pool, "d6"); a != 500 {
		t.Fatalf("cursor ack=%d, want 500", a)
	}
}

// T7 — bad frame skipped, telemetry survives. An implausible frame (delta > LOG_FRAME_MAX, or explicit
// base > end) writes NO fragment/logs row and sets NO ack header, but the telemetry row commits and the
// response is 200. Red: 4xx the whole push → telemetry lost; or insert the corrupt fragment.
func TestReassemble_BadFrameSkipped_T7(t *testing.T) {
	pool := dbPool(t)
	s := logServer(pool, 50) // tiny cap to make a normal-looking delta implausible

	// push1 establishes the epoch + a small ack
	w1 := httptest.NewRecorder()
	s.handleIngest(w1, ppReq("d7", 1, 10, "ok|"))
	if w1.Code != 200 || w1.Header().Get("X-Log-Ack-Offset") != "10" {
		t.Fatalf("push1 code=%d ack=%q", w1.Code, w1.Header().Get("X-Log-Ack-Offset"))
	}
	// push2: a same-epoch delta of 990 bytes (10→1000) blows the 50-byte cap → log skipped, telemetry kept
	w2 := httptest.NewRecorder()
	s.handleIngest(w2, ppReq("d7", 1, 1000, "way-too-big|"))
	if w2.Code != http.StatusOK {
		t.Fatalf("bad-frame code=%d, want 200 (telemetry must survive)", w2.Code)
	}
	if got := w2.Header().Get("X-Log-Ack-Offset"); got != "" {
		t.Fatalf("ack header set on a skipped frame: %q", got)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM logs WHERE serial='d7'`); n != 1 {
		t.Fatalf("logs rows=%d, want 1 (only push1; the bad frame inserted nothing)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM telemetry WHERE serial='d7'`); n != 2 {
		t.Fatalf("telemetry rows=%d, want 2 (both pushes' telemetry committed)", n)
	}

	// explicit base > end is skipped too (no ack, no row), proven directly
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	hi := int64(900)
	_, acked, err := reassembleLog(ctx, tx, "d7", logFrame{epoch: 1, end: 100, base: &hi, explicit: true}, "bad|", s.logFrameMax)
	_ = tx.Rollback(ctx)
	if err != nil || acked {
		t.Fatalf("base>end frame: acked=%v err=%v, want acked=false err=nil (skip)", acked, err)
	}
}

// T8 — seq monotone per device, surviving the FOR UPDATE lock. N concurrent same-epoch pushes produce
// strictly increasing seq with no gaps/dupes. Red: MAX(logs.seq) without the lock → racey duplicate seq.
func TestReassemble_SeqMonotoneConcurrent_T8(t *testing.T) {
	pool := dbPool(t)
	s := logServer(pool, 0)
	seedDevice(t, pool, "d8")

	pushLog(t, s, "d8", 1, 1000, "seed|") // epoch 1 (gap), ack 1000, seq 1
	const N = 8
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := pushLogErr(s, "d8", 1, int64(1000+(i+1)*10), fmt.Sprintf("line-%d|", i)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent push: %v", err)
	}

	rows, err := pool.Query(context.Background(), `SELECT seq FROM logs WHERE serial='d8' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var sq int64
		if err := rows.Scan(&sq); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, sq)
	}
	if len(seqs) != N+1 {
		t.Fatalf("logs rows=%d, want %d", len(seqs), N+1)
	}
	sort.Slice(seqs, func(a, b int) bool { return seqs[a] < seqs[b] })
	for i, sq := range seqs { // want exactly 1,2,...,N+1 — contiguous, distinct
		if sq != int64(i+1) {
			t.Fatalf("seq[%d]=%d, want %d (no gaps/dupes under the lock)", i, sq, i+1)
		}
	}
}

// T9 — cursor RESETS on epoch advance, so no re-push loop and no mis-suspect. After a high ack at epoch
// e1, a post-reboot push with a SMALL end echoes that small end (not the stale high-water), and a
// following same-epoch larger push classifies NORMAL (not suspect). Red: unconditional GREATEST keeps
// the stale 5000 → the FW rejects the echoed ack (re-push forever) and the 600 push reads off<ack → suspect.
func TestReassemble_EpochResetNoLoop_T9(t *testing.T) {
	pool := dbPool(t)
	s := logServer(pool, 0)
	seedDevice(t, pool, "d9")

	pushLog(t, s, "d9", 4, 5000, "e4|") // epoch 4, ack 5000
	ack, _ := pushLog(t, s, "d9", 5, 300, "e5|")
	if ack != 300 {
		t.Fatalf("post-reboot ack=%d, want 300 (the new-epoch end, not the stale 5000)", ack)
	}
	pushLog(t, s, "d9", 5, 600, "e5b|") // off 600 > 300 → must be NORMAL, not suspect
	if e, a, _ := readCursor(t, pool, "d9"); e != 5 || a != 600 {
		t.Fatalf("cursor epoch=%d ack=%d, want 5/600", e, a)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM logs WHERE serial='d9' AND boot_count=5 AND suspect=true`); n != 0 {
		t.Fatalf("epoch-5 suspect rows=%d, want 0 (the GREATEST bug would mis-flag the 600 push)", n)
	}
}

// T-merge (masterplan K11) — the log ack rides the post-commit /pp path BEFORE the OTA signal, so it is
// emitted regardless of the OTA outcome. With OTA disarmed (injectOTASignal a no-op) the ack still lands;
// with OTA armed+resolving, BOTH headers land. Red: a reorder putting injectOTASignal's early-return
// before the ack → the disarmed push loses its ack (committed delta never acked → ring overrun, O5).
func TestHandleIngest_K11_AckBeforeOTA_Tmerge(t *testing.T) {
	pool := dbPool(t)

	// OTA disarmed → no OTA headers, but the ack survives
	s := logServer(pool, 0)
	w := httptest.NewRecorder()
	s.handleIngest(w, ppReq("dm", 2, 400, "a|"))
	if got := w.Header().Get("X-Log-Ack-Offset"); got != "400" {
		t.Fatalf("disarmed ack header=%q, want 400 (must not depend on OTA)", got)
	}
	if got := w.Header().Get("X-Firmware-Version"); got != "" {
		t.Fatalf("X-Firmware-Version=%q with OTA disarmed", got)
	}

	// OTA armed + resolvable → both headers present
	s2, version, _, _ := seedServeable(t, pool) // arms OTA, seeds dev1 + stable default
	s2.logFrameMax = 65536
	w2 := httptest.NewRecorder()
	s2.handleIngest(w2, ppReq("dev1", 3, 700, "b|"))
	if got := w2.Header().Get("X-Log-Ack-Offset"); got != "700" {
		t.Fatalf("armed ack header=%q, want 700", got)
	}
	if got := w2.Header().Get("X-Firmware-Version"); got != version {
		t.Fatalf("X-Firmware-Version=%q, want %q (OTA signal must still fire)", got, version)
	}
}

// T-snap (masterplan K12) — a firmware.bin GET carrying X-Picpak-Log persists EXACTLY ONE
// source='ota-snapshot' logs row (seq NULL, boot_count from ?bc); a download without the header writes
// none. Red: the viewer's ota-snapshot filter is always empty (no one writes it), or the reassembly path
// double-writes it.
func TestHandleFirmware_LogSnapshot_Tsnap(t *testing.T) {
	pool := dbPool(t)
	s, version, _, _ := seedServeable(t, pool)

	r := fwReq(otaticket.Mint(s.otaKey, "dev1", version, time.Minute))
	r.URL.RawQuery = "bc=9"
	r.Header.Set("X-Picpak-Log", "panic|brownout|")
	w := httptest.NewRecorder()
	s.handleFirmware(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("download code=%d, want 200", w.Code)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM logs WHERE serial='dev1' AND source='ota-snapshot'`); n != 1 {
		t.Fatalf("ota-snapshot rows=%d, want 1", n)
	}
	var seqNull bool
	var bc *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT seq IS NULL, boot_count FROM logs WHERE serial='dev1' AND source='ota-snapshot'`).
		Scan(&seqNull, &bc); err != nil {
		t.Fatal(err)
	}
	if !seqNull {
		t.Fatal("ota-snapshot seq should be NULL (no reassembly)")
	}
	if bc == nil || *bc != 9 {
		t.Fatalf("ota-snapshot boot_count=%v, want 9", bc)
	}

	// a download WITHOUT the log header writes no snapshot row
	w2 := httptest.NewRecorder()
	s.handleFirmware(w2, fwReq(otaticket.Mint(s.otaKey, "dev1", version, time.Minute)))
	if n := countRows(t, pool, `SELECT count(*) FROM logs WHERE serial='dev1' AND source='ota-snapshot'`); n != 1 {
		t.Fatalf("ota-snapshot rows after a header-less download=%d, want still 1", n)
	}
}
