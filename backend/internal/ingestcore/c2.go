package ingestcore

// C2 command channel (Doc 13 Wave 3c): the device polls GET /<token>/c2?sn=<serial>&ack=<seq>; the
// backend advances the device's cursor (the ack proves it applied up to <seq>) and serves the next
// pending Berry command for that device or the whole fleet ('*'), or 204 when in sync.
//
// The exchange is an ack-cursor change feed: the device's cursor (device_c2_cursor.applied_seq) is the
// only per-device state, so a fleet command is served to every device independently and acked per
// device. The cursor only moves forward (GREATEST) -> a replayed low ack can never roll it back.
//
// Auth (Wave 3d, c2auth.go): per-device HOTP gates the route BEFORE the cursor is touched, so a forged
// request cannot advance the cursor. The shared path token remains the endpoint locator (scanners get
// 404). HTTPS-only enforcement is the firmware side of 3d (the device refuses a non-https c2_url).

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-picpak/backend/internal/commandstore"
)

// c2MaxWaitS caps the long-poll hold budget a device may request via ?wait=<secs> (Design 16).
const c2MaxWaitS = 25

func (s *Server) handleC2(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serial := q.Get("sn")
	if serial == "" {
		http.NotFound(w, r) // no identity = noise
		return
	}
	// ack = highest seq the device claims to have applied; absent/garbage -> 0 (cold device).
	ack, _ := strconv.ParseInt(q.Get("ack"), 10, 64)
	if ack < 0 {
		ack = 0
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError) // device retries next poll
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// Authenticate BEFORE touching the cursor: a forged request must not be able to advance the
	// cursor (which would make the device skip real commands). Constant 401 on any failure.
	if !s.authDevice(ctx, tx, serial, q) {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	// Advance the cursor (GREATEST = forward-only) and stamp the poll. The FK on
	// device_c2_cursor.serial -> devices(serial) rejects an unknown device (must be registered via
	// ingest/provisioning first) -> 404 noise.
	var applied int64
	err = tx.QueryRow(ctx,
		`INSERT INTO device_c2_cursor (serial, applied_seq, last_poll_at, updated_at)
		 VALUES ($1, $2, now(), now())
		 ON CONFLICT (serial) DO UPDATE SET
		   applied_seq  = GREATEST(device_c2_cursor.applied_seq, EXCLUDED.applied_seq),
		   last_poll_at = now(),
		   updated_at   = now()
		 RETURNING applied_seq`,
		serial, ack).Scan(&applied)
	if err != nil {
		http.NotFound(w, r) // FK violation (unknown device) or transient -> noise; device retries
		return
	}

	// The advanced cursor is this device's ack: clear the enqueue-dedup flag on its now-applied keyed
	// commands so an identical command may be re-enqueued (K4 re-entry). '*' rows are untouched (applied
	// per device) -> the fleet dedup persists while any device lags. In-tx with the cursor advance.
	if err := commandstore.MarkApplied(ctx, tx, serial, applied); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Serve the next pending command for this device or the fleet ('*'), in seq order.
	var seq int64
	var script string
	err = tx.QueryRow(ctx,
		`SELECT seq, script FROM command_queue
		 WHERE serial IN ($1, '*') AND seq > $2
		 ORDER BY seq
		 LIMIT 1`,
		serial, applied).Scan(&seq, &script)
	if errors.Is(err, pgx.ErrNoRows) {
		if cErr := tx.Commit(ctx); cErr != nil { // persist the cursor advance even when idle
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		// Long-poll opt-in (?wait=<secs>, Design 16): hold the request until a command for this device
		// lands or the budget elapses -> near-instant delivery on USB power. Absent/<=0 -> immediate 204
		// (the battery field poll wants a fast 204, no connection hold). The cursor is already committed;
		// the hold below only READs the queue (the device acks the served seq on its next poll).
		waitS, _ := strconv.Atoi(q.Get("wait"))
		if waitS <= 0 {
			w.WriteHeader(http.StatusNoContent) // 204: in sync, nothing to do
			return
		}
		if waitS > c2MaxWaitS {
			waitS = c2MaxWaitS
		}
		s.c2LongPoll(w, r, serial, applied, time.Duration(waitS)*time.Second)
		return
	}
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// 200 + the Berry script body; X-C2-Seq is what the device acks once it has applied the script.
	w.Header().Set("X-C2-Seq", strconv.FormatInt(seq, 10))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(script))
}

// c2LongPoll holds the request until a command for serial (or the fleet '*') with seq > applied
// appears, or the budget elapses. It writes the response (200 + Berry script, or 204). The caller has
// already committed the cursor advance; this path only READs the queue. It waits on the in-process
// notifier (fed by one LISTEN connection), so a held poll costs a goroutine, not a DB connection.
func (s *Server) c2LongPoll(w http.ResponseWriter, r *http.Request, serial string, applied int64, budget time.Duration) {
	ctx := r.Context()
	deadline := time.After(budget)
	for {
		// Grab the wake channel BEFORE the query: an insert racing between the query and the select
		// still closes this channel -> we re-query (no lost wakeup).
		wake := s.notifier.wait()

		var seq int64
		var script string
		err := s.pool.QueryRow(ctx,
			`SELECT seq, script FROM command_queue
			 WHERE serial IN ($1, '*') AND seq > $2
			 ORDER BY seq
			 LIMIT 1`,
			serial, applied).Scan(&seq, &script)
		if err == nil {
			w.Header().Set("X-C2-Seq", strconv.FormatInt(seq, 10))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(script))
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		select {
		case <-ctx.Done():
			return // client disconnected -> nothing to write
		case <-deadline:
			w.WriteHeader(http.StatusNoContent) // budget elapsed: in sync
			return
		case <-wake:
			// a command landed somewhere -> loop and re-query
		}
	}
}
