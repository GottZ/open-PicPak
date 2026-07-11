package ingestcore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Log reassembly (A21 / Design 21, part a — device-facing, untrusted parser, M5 public zone).
//
// Replaces the snapshot-only log insert: per /pp push, derive the device's stream position from
// (boot_count, offset), persist a byte-exact idempotent FRAGMENT, a seq-stamped viewer LOGS row, advance
// the durable CURSOR, and echo the durably-committed high-water as X-Log-Ack-Offset — but only AFTER the
// telemetry tx COMMITs (durable-before-ack, Doc 04 §5.4). The FW owns the offset; the Server echoes it and
// never computes end = off + len (D21.2) — a '|'-mapped payload's transport length is not the raw-byte
// count, so deriving end from it would corrupt the cursor.
//
// The three tables already exist (migrations/0001:59-131); this wires them. The cursor is a SINGLE row per
// device (PK = serial, 0001:60) carrying the current epoch + high-water — an epoch advance UPDATES the row
// (D21.4), it does not key a new (sn,epoch) row.

// logFrame is the Server's view of one log push's metadata. The richer X-Picpak-Log-Frame is preferred
// when present (explicit base/gap); else the canonical (?bc, ?off) fallback is used and the base is the
// prior cursor ack (D21.3). Which the firmware actually emits is a HARD on-device dependency (Doc 04 /
// F4-W3, masterplan FW-2) — this Server tolerantly consumes both and does not fabricate the FW emission.
type logFrame struct {
	epoch    int64  // ep / ?bc — epoch discriminator, == NVS otadiag/boots (D21.4)
	end      int64  // end / ?off — device post-delta high-water, absolute bytes, FW-owned (D21.2)
	base     *int64 // explicit frame base, or nil (fallback derives it from the cursor ack)
	gapClaim int64  // explicit frame gap hint, or 0 — validated, NOT used to classify (Server decides)
	explicit bool   // came from X-Picpak-Log-Frame
}

// parseLogFrame extracts the wire metadata, preferring X-Picpak-Log-Frame ("ep=.. base=.. end=.. gap=..")
// and falling back to (?bc, ?off). Returns ok=false when there is no position to reassemble (no ?off and
// no frame) — the caller then has nothing to ack and skips the log step (D21.6). Payload is carried
// separately (the X-Picpak-Log header today; the body per Doc 04 §3.5).
func parseLogFrame(q map[string][]string, frameHdr string) (logFrame, bool) {
	if frameHdr != "" {
		m := parseDiag(frameHdr) // reuse the space-separated "k=v k=v" parser (main.go)
		ep, okE := pInt64(m["ep"])
		end, okN := pInt64(m["end"])
		if okE && okN {
			lf := logFrame{epoch: ep, end: end, explicit: true}
			if b, ok := pInt64(m["base"]); ok {
				lf.base = &b
			}
			if g, ok := pInt64(m["gap"]); ok {
				lf.gapClaim = g
			}
			return lf, true
		}
		// malformed frame header → fall through to the (?bc, ?off) interpretation
	}
	qget := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	end, ok := pInt64(qget("off"))
	if !ok {
		return logFrame{}, false // no offset → no stream position → nothing to reassemble
	}
	epoch, _ := pInt64(qget("bc")) // a missing bc defaults to 0 (the cursor's default epoch, 0001:61)
	return logFrame{epoch: epoch, end: end}, true
}

// reassembleLog runs inside the telemetry tx, AFTER the telemetry insert. It returns the durable ack
// high-water and whether the push was acked; the caller sets X-Log-Ack-Offset iff (acked && err == nil)
// AND the tx committed. A malformed/implausible frame is SKIPPED (no fragment/logs/ack) and err is nil so
// the telemetry still commits (D21.6) — the device re-pushes the unacked delta next wake (idempotent).
func reassembleLog(ctx context.Context, tx pgx.Tx, serial string, lf logFrame, payload string, maxFrame int64) (ackOff int64, acked bool, err error) {
	if payload == "" || lf.end < 0 {
		return 0, false, nil // nothing to persist / negative offset is garbage
	}
	// Cursor-INDEPENDENT frame validation (D21.6). A bad frame is skipped, not 4xx'd — the log
	// piggybacks the telemetry push on one tx, so rejecting it would drop the telemetry too.
	if lf.explicit {
		if lf.base != nil && *lf.base > lf.end {
			return 0, false, nil // base > end (T7)
		}
		if lf.gapClaim < 0 {
			return 0, false, nil
		}
		if lf.base != nil && lf.end-*lf.base > maxFrame {
			return 0, false, nil // explicit oversize delta (T7)
		}
	}

	// Race-safe lazy cursor create, then lock it (D21.9). The idempotent insert waits out a concurrent
	// uncommitted first-contact insert, so the row is present + visible before the locking SELECT — a
	// naive SELECT-then-INSERT races two first pushes into a PK violation or a missed lock.
	if _, err := tx.Exec(ctx,
		`INSERT INTO device_log_cursor (serial) VALUES ($1) ON CONFLICT (serial) DO NOTHING`, serial); err != nil {
		return 0, false, fmt.Errorf("cursor lazy create: %w", err)
	}
	var curEpoch, curAck, curSeq int64
	if err := tx.QueryRow(ctx,
		`SELECT epoch, ack_off, last_seq FROM device_log_cursor WHERE serial=$1 FOR UPDATE`, serial).
		Scan(&curEpoch, &curAck, &curSeq); err != nil {
		return 0, false, fmt.Errorf("cursor lock: %w", err)
	}

	// Fallback delta-cap (D21.6): with no explicit base, a same-epoch contiguous push's base is the
	// prior ack; an implausibly large delta (end - prevAck) is a corrupt frame → skip. Gap/stale have no
	// Server-known base, so no size bound applies. (Done post-lock because the base IS the cursor ack.)
	if !lf.explicit && lf.epoch == curEpoch && lf.end > curAck && lf.end-curAck > maxFrame {
		return 0, false, nil
	}

	// Classify against the durable cursor (D21.4); epoch == boot_count (NVS boots). The Server decides
	// gap/suspect from (bc, off) vs the cursor — the frame's own gap claim is a hint, never authoritative.
	gapFlag := lf.epoch > curEpoch // reboot → new stream segment
	suspectFlag := (lf.epoch == curEpoch && lf.end < curAck) || // offset rollback WITHOUT a reboot
		(lf.epoch < curEpoch) // a stale, already-superseded epoch (a live device cannot push this → replay)
	var startOff any // a contiguous same-epoch push knows its base (the prior ack); gap/suspect/stale do not
	if lf.epoch == curEpoch && lf.end > curAck {
		startOff = curAck
	}

	// Idempotent fragment insert (D21.5): the FW-owned, ack-gated (serial, epoch, end_off) PK makes a
	// re-pushed delta a no-op even if a ring overrun slid its start_off. Keyed by the INCOMING epoch, so
	// old-epoch fragments survive under their own key across a reboot.
	var inserted bool
	switch err := tx.QueryRow(ctx,
		`INSERT INTO device_log_fragment (serial, epoch, start_off, end_off, payload, gap)
		 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (serial, epoch, end_off) DO NOTHING RETURNING true`,
		serial, lf.epoch, startOff, lf.end, payload, gapFlag).Scan(&inserted); {
	case errors.Is(err, pgx.ErrNoRows):
		inserted = false // duplicate delta — the PK collided; no second viewer row (D21.8)
	case err != nil:
		return 0, false, fmt.Errorf("fragment insert: %w", err)
	}

	// One push → at most one logs row, only when the fragment actually inserted (D21.8). seq is assigned
	// from the locked cursor (D21.9) — monotone per device, no MAX(logs.seq) race, and it outlives the
	// 90d logs retention because it lives on the cursor (it is the keyset tiebreak, D21.11).
	newSeq := curSeq
	if inserted {
		newSeq = curSeq + 1
		if _, err := tx.Exec(ctx,
			`INSERT INTO logs (time, serial, boot_count, seq, offset_start, payload, source, gap, suspect)
			 VALUES (now(), $1, $2, $3, $4, $5, 'telemetry', $6, $7)`,
			serial, lf.epoch, newSeq, startOff, payload, gapFlag, suspectFlag); err != nil {
			return 0, false, fmt.Errorf("logs insert: %w", err)
		}
	}

	// Advance the cursor (D21.4/D21.5). The row is locked, so curEpoch/curAck are a stable pre-write
	// read: an epoch advance RESETS ack_off to the new ring's end (the dead ring's high-water would
	// exceed the device's new s_total and the FW would reject the echoed ack, Doc 04 §3.4 — the re-push
	// loop + mis-suspect bug T9 isolates); a same-epoch push serializes via the max (a frame-pull + an
	// OTA-pull in one boot must not lose-update, Doc 04 §5.11); a stale epoch leaves the cursor untouched.
	newEpoch, newAck := curEpoch, curAck
	switch {
	case lf.epoch > curEpoch: // gap / epoch advance → RESET (NOT GREATEST — T3/T9)
		newEpoch, newAck = lf.epoch, lf.end
	case lf.epoch == curEpoch: // same epoch → serialize; duplicate/suspect keep the high-water
		if lf.end > curAck {
			newAck = lf.end
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE device_log_cursor SET epoch=$2, ack_off=$3, last_seq=$4, updated_at=now() WHERE serial=$1`,
		serial, newEpoch, newAck, newSeq); err != nil {
		return 0, false, fmt.Errorf("cursor advance: %w", err)
	}

	// The ack is the read-your-write durable high-water from THIS tx, echoed only after COMMIT by the
	// caller (Doc 04 §5.4). Any valid frame is acked — even a suspect/stale push echoes the true high-
	// water, telling a replaying/confused device where the Server actually is.
	return newAck, true, nil
}

// insertSnapshot persists the device's log ride-along on a firmware.bin OTA download as ONE row with
// source='ota-snapshot', seq=NULL, no offset/cursor/reassembly (masterplan K12). A20's handleFirmware
// invokes it; the logs-write contract stays here in A21's module (the same handler-vs-statement split as
// the device-delete tx). It runs on its OWN connection — the download is not the /pp telemetry tx.
func insertSnapshot(ctx context.Context, pool *pgxpool.Pool, serial string, bootCount any, payload string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO logs (time, serial, boot_count, seq, offset_start, payload, source, gap, suspect)
		 VALUES (now(), $1, $2, NULL, NULL, $3, 'ota-snapshot', false, false)`,
		serial, bootCount, payload)
	return err
}

// pInt64 parses a base-10 int64, reporting ok=false on empty/garbage (so an absent field is not a 0).
func pInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// logFrameMaxFromEnv reads LOG_FRAME_MAX (the frame plausibility cap, D21.6 — policy as data, never a
// code constant). Default 65536: a single push delta cannot exceed the device's ~2 KB ring, so 64 KiB is
// a generous ceiling that rejects corrupt frames without ever false-positiving legitimate traffic.
func logFrameMaxFromEnv() int64 {
	if v := os.Getenv("LOG_FRAME_MAX"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 65536
}
