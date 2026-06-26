package logs

import (
	"hash/fnv"
	"strconv"
	"strings"
)

// splitPayload splits one row's `payload` back into its constituent device lines on the
// configured separator (the firmware maps '\n'→sep before exfil, so a row is several
// lines joined by sep). It drops a single trailing empty token, because a ring tail
// commonly ends at/just after a separator; interior empty tokens are kept (a genuine
// blank device line). An empty payload yields no lines. A separator that does not occur
// yields the whole payload as one line.
//
// This is the single load-bearing parse of the axis. A literal sep inside a device
// message is NOT escaped by the firmware, so it becomes a false break — a documented
// fidelity limit; making the separator config means a future firmware separator change
// needs no code change.
func splitPayload(payload, sep string) []string {
	if sep == "" {
		// A misconfigured empty separator must not explode the payload into runes; treat
		// the whole payload as one line (and let validation/config carry the real sep).
		if payload == "" {
			return nil
		}
		return []string{payload}
	}
	parts := strings.Split(payload, sep)
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	return parts
}

// dedupKey is the interim de-dup key for a row while seq is NULL (design Q3): the
// (offset_start, payload-hash) pair. Combined with the store's high-water second it
// drops exact repeats a re-fetching forward poll re-reads at the same timestamp without
// dropping genuinely distinct same-second rows.
func dedupKey(r logRow) string {
	off := int64(-1)
	if r.OffsetStart != nil {
		off = *r.OffsetStart
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(r.Payload))
	return strconv.FormatInt(off, 10) + ":" + strconv.FormatUint(h.Sum64(), 16)
}

// bootTracker remembers the last boot_count seen per serial so a boot-epoch change can be
// detected and a synthetic gap divider injected between epochs. The store holds one
// persistent tracker for the forward tail; backfill uses a fresh per-batch tracker so an
// older page's within-batch epoch changes still divide, without faking a seam to the
// live tail (documented limit).
type bootTracker struct {
	last map[string]int64
	seen map[string]bool
}

func newBootTracker() *bootTracker {
	return &bootTracker{last: map[string]int64{}, seen: map[string]bool{}}
}

// step records a row's boot_count and reports whether it changed from the previous one
// for that serial (and the previous value). The first observation of a serial never
// reports a change (there is no gap before the first observed epoch).
func (bt *bootTracker) step(serial string, bc *int64) (changed bool, from int64) {
	if bc == nil {
		return false, 0
	}
	prev, had := bt.last[serial]
	bt.last[serial] = *bc
	bt.seen[serial] = true
	if !had {
		return false, 0
	}
	return prev != *bc, prev
}

// parseRow turns one row into its ordered Lines: an optional gap marker, an optional
// suspect marker, then the content lines split from the payload. The marker text comes
// entirely from the config templates (gapMarker / cfg.SuspectMarker) — there is no
// literal marker string in this package (Policy=Data).
func parseRow(cfg Config, r logRow, bt *bootTracker) []Line {
	var out []Line

	changed, from := bt.step(r.Serial, r.BootCount)
	// A boot-epoch change OR an explicit gap=true row divides epochs.
	if cfg.ShowGapMarkers && (changed || r.Gap) {
		to := from
		if r.BootCount != nil {
			to = *r.BootCount
		}
		fromShown := from
		if !changed && r.Gap {
			// gap=true without a tracked predecessor: show the epoch on both sides.
			fromShown = to
		}
		out = append(out, syntheticLine(r, SynthGap, gapMarker(cfg.GapMarkerFormat, fromShown, to)))
	}

	if cfg.ShowSuspect && r.Suspect {
		out = append(out, syntheticLine(r, SynthSuspect, cfg.SuspectMarker))
	}

	for _, txt := range splitPayload(r.Payload, cfg.LineSeparator) {
		out = append(out, contentLine(r, txt))
	}
	return out
}

// gapMarker fills the {from}/{to} placeholders in the configured template with the two
// boot epochs. The template is config; this only substitutes.
func gapMarker(format string, from, to int64) string {
	s := strings.ReplaceAll(format, "{from}", strconv.FormatInt(from, 10))
	s = strings.ReplaceAll(s, "{to}", strconv.FormatInt(to, 10))
	return s
}

// contentLine wraps one split device line, carrying the row context the view colorizes
// and labels by.
func contentLine(r logRow, text string) Line {
	l := Line{
		Time:      r.Time,
		Serial:    r.Serial,
		Source:    r.Source,
		Text:      text,
		Synthetic: SynthNone,
	}
	if r.BootCount != nil {
		l.BootCount = *r.BootCount
		l.HasBoot = true
	}
	return l
}

// syntheticLine wraps an injected marker line (gap / suspect), sharing the row's context
// so the view can place and color it next to the device lines of the same serial.
func syntheticLine(r logRow, kind SyntheticKind, text string) Line {
	l := Line{
		Time:      r.Time,
		Serial:    r.Serial,
		Source:    r.Source,
		Text:      text,
		Synthetic: kind,
	}
	if r.BootCount != nil {
		l.BootCount = *r.BootCount
		l.HasBoot = true
	}
	return l
}
