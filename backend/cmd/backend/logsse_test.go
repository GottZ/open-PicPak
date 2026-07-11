package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/logquery"
)

// Q6 — SSE frame integrity. A log payload whose lines contain "\n\nevent:" must round-trip as exactly ONE
// `event: log` frame: the hub frame-builder json.Marshal's the whole value (escaping the newlines) and
// the writer emits a single data: line. Red: a hand-concatenated data: field → the embedded "\n\n" ends
// the frame early / forges a second `event:` line. No DB needed — this probes the marshal + writer only.
func TestSSE_LogFrameIntegrity_Q6(t *testing.T) {
	forged := "\n\nevent: log\ndata: {\"forged\":true}"
	ev := logquery.TailEvent{
		Serial: "stub-serial", Time: time.Unix(0, 0).UTC(), Source: "telemetry",
		Lines: []string{"a normal line", forged},
	}

	// exactly what the hub broadcastJSON does, then the real SSE writer frames it
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec, time.Minute)
	if err := sw.event("log", "", data); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()

	// one frame only: splitting on the SSE frame terminator yields a single non-empty frame
	frames := strings.Split(strings.TrimRight(out, "\n"), "\n\n")
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 — the forged newline split the frame:\n%q", len(frames), out)
	}
	// exactly one PHYSICAL line is an event line: the escaped "event: log" text lives mid-data-line, not
	// at a line start (json.Marshal turned its newlines into \n). A hand-concatenated data field would
	// surface it as a real second event: line here.
	eventLines := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "event: ") {
			eventLines++
		}
	}
	if eventLines != 1 {
		t.Fatalf("event: lines=%d, want 1 (a forged event line leaked as a real frame):\n%q", eventLines, out)
	}

	// the data: line is intact valid JSON and the malicious line survived escaped→unescaped
	var payload string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: ") {
			payload = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	var got logquery.TailEvent
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("data line is not valid JSON — frame corrupted: %v\n%q", err, out)
	}
	if len(got.Lines) != 2 || got.Lines[1] != forged {
		t.Fatalf("malicious line not preserved through the frame: %q", got.Lines)
	}

	// Negative control: the SAME forged bytes hand-concatenated (NOT json.Marshal'd) DO split the frame —
	// proving the marshal is the load-bearing protection, not an incidental pass.
	rec2 := httptest.NewRecorder()
	sw2 := newSSEWriter(rec2, time.Minute)
	if err := sw2.event("log", "", []byte(forged)); err != nil {
		t.Fatal(err)
	}
	naive := strings.Split(strings.TrimRight(rec2.Body.String(), "\n"), "\n\n")
	if len(naive) < 2 {
		t.Fatalf("naive raw concat produced %d frames — expected it to split (the probe would be toothless)", len(naive))
	}
}
