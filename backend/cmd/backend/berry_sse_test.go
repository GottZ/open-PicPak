package main

import (
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/commandstore"
)

// T9 — the c2cursor producer diffs on applied_seq ONLY. A tick where only last_poll_at advanced emits
// NOTHING; a tick where applied_seq moved emits one event per advanced device. Pure (diffCursors), like
// diffRoster — the broadcast wiring is the shared hub path already covered by the roster/telemetry tests.
func TestDiffCursors(t *testing.T) {
	t1 := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	t3 := t2.Add(time.Minute)
	at := func(tm time.Time) *time.Time { return &tm }

	// first tick (last empty) re-emits every current cursor — like the roster producer, so a cursor that
	// advanced between the editor's rehydration read and this tick is delivered, not swallowed.
	ev, state := diffCursors(map[string]int64{}, []commandstore.Cursor{
		{Serial: "A", AppliedSeq: 5, LastPollAt: at(t1)},
		{Serial: "B", AppliedSeq: 3, LastPollAt: at(t1)},
	})
	if len(ev) != 2 {
		t.Fatalf("first tick must emit all current cursors; got %d, want 2", len(ev))
	}
	if state["A"] != 5 || state["B"] != 3 {
		t.Fatalf("state not tracked: %+v", state)
	}

	// only last_poll_at advanced (applied_seq unchanged) → NOTHING (the T9 property; the last_seen hazard).
	ev, state = diffCursors(state, []commandstore.Cursor{
		{Serial: "A", AppliedSeq: 5, LastPollAt: at(t2)},
		{Serial: "B", AppliedSeq: 3, LastPollAt: at(t2)},
	})
	if len(ev) != 0 {
		t.Fatalf("a last_poll_at-only advance must emit nothing; got %d events", len(ev))
	}

	// applied_seq advanced for A only → exactly one event for A.
	ev, state = diffCursors(state, []commandstore.Cursor{
		{Serial: "A", AppliedSeq: 7, LastPollAt: at(t3)},
		{Serial: "B", AppliedSeq: 3, LastPollAt: at(t3)},
	})
	if len(ev) != 1 || ev[0].Serial != "A" || ev[0].AppliedSeq != 7 {
		t.Fatalf("only A's applied_seq advance should emit; got %+v", ev)
	}

	// a newly-seen device emits its current cursor.
	ev, _ = diffCursors(state, []commandstore.Cursor{
		{Serial: "A", AppliedSeq: 7, LastPollAt: at(t3)},
		{Serial: "C", AppliedSeq: 2, LastPollAt: at(t3)},
	})
	if len(ev) != 1 || ev[0].Serial != "C" {
		t.Fatalf("a new device must emit its cursor; got %+v", ev)
	}
}
