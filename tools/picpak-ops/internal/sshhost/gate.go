package sshhost

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// gate is the strict VID:PID confirmation layer (defense-in-depth layer 2, §2.2).
// Polling (layer 1) accepts a device on descriptor-match alone and never opens the
// port; before any open/flash a consumer calls HostRegistry.Confirm, which runs
// poll.confirm_command (default udevadm — no port open) and asserts that
// ID_VENDOR_ID / ID_MODEL_ID equal the CONFIGURED poll.vendor_id / poll.product_id.
//
// The expected VID:PID is read from config — there is NO VID:PID literal in this
// code (single source of truth = the config keys; an operator override cannot drift
// out of sync). 303a:1001 lives only in the config defaults table.
type gate struct {
	expectVID string   // configured poll.vendor_id, lowercased
	expectPID string   // configured poll.product_id, lowercased
	command   []string // confirm_command template; %TTY% substituted per device
	required  bool     // fail-closed: an unconfirmable device is never acted on
	cacheTTL  time.Duration

	mu    sync.Mutex
	cache map[string]gateEntry // (host,tty,descriptor) → cached confirm
}

type gateEntry struct {
	ok        bool
	at        time.Time
	expectVID string // the VID:PID this entry was confirmed against; invalidates on config change
	expectPID string
}

// newGate builds the gate from config values. The expected pair is normalized to
// lowercase so the comparison is case-insensitive against udevadm output.
func newGate(vendorID, productID string, command []string, required bool, cacheTTL time.Duration) *gate {
	return &gate{
		expectVID: strings.ToLower(vendorID),
		expectPID: strings.ToLower(productID),
		command:   append([]string(nil), command...),
		required:  required,
		cacheTTL:  cacheTTL,
		cache:     map[string]gateEntry{},
	}
}

// confirmKey is the cache key per §2.2: (host, tty, descriptor). A positive
// confirm is cached until the descriptor disappears or the TTL expires.
func confirmKey(dev DiscoveredDevice) string {
	return dev.Host + "\x00" + dev.TTY + "\x00" + dev.Descriptor
}

// confirm runs the hard VID:PID gate for a device using the host's runner. A cached
// positive confirm (within TTL and against the current expected VID:PID) short-
// circuits. On a fresh confirm it runs confirm_command (with %TTY% substituted),
// parses ID_VENDOR_ID / ID_MODEL_ID from the udevadm property output, and asserts
// both equal the configured pair. Fail-closed: any error or mismatch under
// confirm_required returns an error so the device is never acted on.
func (g *gate) confirm(ctx context.Context, r Runner, dev DiscoveredDevice) error {
	key := confirmKey(dev)

	g.mu.Lock()
	if e, ok := g.cache[key]; ok && e.ok &&
		e.expectVID == g.expectVID && e.expectPID == g.expectPID &&
		(g.cacheTTL <= 0 || time.Since(e.at) < g.cacheTTL) {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	argv := substituteTTY(g.command, dev.TTY)
	out, err := r.CaptureOutput(ctx, argv)
	if err != nil {
		if g.required {
			return fmt.Errorf("confirm %s on %s: %w", dev.TTY, dev.Host, err)
		}
		return nil // not required → a probe failure does not block (descriptor-match stands)
	}

	vid, pid := parseUdevVIDPID(out)
	if vid == g.expectVID && pid == g.expectPID {
		g.mu.Lock()
		g.cache[key] = gateEntry{ok: true, at: time.Now(), expectVID: g.expectVID, expectPID: g.expectPID}
		g.mu.Unlock()
		return nil
	}

	if g.required {
		return fmt.Errorf("confirm %s on %s: VID:PID %s:%s != expected %s:%s (fail-closed)",
			dev.TTY, dev.Host, emptyDash(vid), emptyDash(pid), g.expectVID, g.expectPID)
	}
	return nil
}

// invalidate drops the cache entry for a device whose descriptor disappeared, so a
// re-plugged device is re-confirmed rather than trusting a stale positive (§2.2:
// cached "until the descriptor disappears").
func (g *gate) invalidate(dev DiscoveredDevice) {
	g.mu.Lock()
	delete(g.cache, confirmKey(dev))
	g.mu.Unlock()
}

// substituteTTY replaces the %TTY% token in the confirm_command argv with the
// device's tty path. Every occurrence is replaced so the template can place it in
// any position.
func substituteTTY(template []string, tty string) []string {
	out := make([]string, len(template))
	for i, a := range template {
		out[i] = strings.ReplaceAll(a, "%TTY%", tty)
	}
	return out
}

// parseUdevVIDPID extracts the lowercased ID_VENDOR_ID / ID_MODEL_ID from the
// `udevadm info -q property` output (KEY=VALUE per line). udevadm prints these as
// 4-hex lowercase; lsusb-style output is not parsed here (the default command is
// udevadm; an lsusb alt confirm_command would need its own parser — left for the
// operator who sets that alt, since udevadm is the wear-free default, §2.2).
func parseUdevVIDPID(out string) (vid, pid string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "ID_VENDOR_ID="); ok {
			vid = strings.ToLower(strings.TrimSpace(v))
		} else if v, ok := strings.CutPrefix(line, "ID_MODEL_ID="); ok {
			pid = strings.ToLower(strings.TrimSpace(v))
		}
	}
	return vid, pid
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
