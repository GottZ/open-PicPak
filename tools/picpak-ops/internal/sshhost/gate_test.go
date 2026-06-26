package sshhost

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-picpak/picpak-ops/internal/pane"
)

// fakeRunner is a Runner stub for gate/registry tests: CaptureOutput returns a
// canned udevadm body (or error) and counts calls so a test can prove the confirm
// cache short-circuits a second call.
type fakeRunner struct {
	out   string
	err   error
	calls atomic.Int32
}

func (f *fakeRunner) Argv(argv []string) []string { return argv }
func (f *fakeRunner) Run(context.Context, []string, pane.PaneID, pane.Sender) (*Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeRunner) RunInteractive(context.Context, []string, pane.PaneID, pane.Sender) (*Run, io.WriteCloser, error) {
	return nil, nil, errors.New("not used")
}
func (f *fakeRunner) CaptureOutput(context.Context, []string) (string, error) {
	f.calls.Add(1)
	return f.out, f.err
}
func (f *fakeRunner) CloseMaster() error { return nil }

// udevBody builds a realistic `udevadm info -q property` body for a VID:PID.
func udevBody(vid, pid string) string {
	return "DEVNAME=/dev/ttyACM0\n" +
		"ID_BUS=usb\n" +
		"ID_VENDOR_ID=" + vid + "\n" +
		"ID_MODEL_ID=" + pid + "\n" +
		"ID_SERIAL=Espressif_USB_JTAG_serial_debug_unit\n"
}

func picpakDev() DiscoveredDevice {
	return DiscoveredDevice{
		Host:       "host-a",
		TTY:        "/dev/ttyACM0",
		Descriptor: "usb-Espressif_USB_JTAG_serial_debug_unit_x-if00",
	}
}

// TestGateConfirmAcceptsMatchingVIDPID is the GREEN half: a device whose udevadm
// VID:PID equals the configured pair confirms, and a second confirm is served from
// the cache (no second probe).
func TestGateConfirmAcceptsMatchingVIDPID(t *testing.T) {
	g := newGate("303a", "1001",
		[]string{"udevadm", "info", "-q", "property", "-n", "%TTY%"},
		true, 5*time.Minute)
	r := &fakeRunner{out: udevBody("303a", "1001")}

	if err := g.confirm(context.Background(), r, picpakDev()); err != nil {
		t.Fatalf("confirm of a matching device failed: %v", err)
	}
	// Second confirm hits the cache → no extra probe.
	if err := g.confirm(context.Background(), r, picpakDev()); err != nil {
		t.Fatalf("cached confirm failed: %v", err)
	}
	if got := r.calls.Load(); got != 1 {
		t.Fatalf("expected 1 udevadm probe (second served from cache), got %d", got)
	}
}

// TestGateRejectsWrongVIDPID is the RED half (the safety property proved
// negatively, per the methodology): a Zigbee dongle whose VID:PID differs from the
// configured PicPak pair is rejected fail-closed under confirm_required — it is
// NEVER acted on. This is the hard gate that backstops the descriptor regex.
func TestGateRejectsWrongVIDPID(t *testing.T) {
	g := newGate("303a", "1001",
		[]string{"udevadm", "info", "-q", "property", "-n", "%TTY%"},
		true, 5*time.Minute)
	// A Silicon Labs CP210x (Sonoff Zigbee) VID:PID — must be rejected.
	r := &fakeRunner{out: udevBody("10c4", "ea60")}

	err := g.confirm(context.Background(), r, picpakDev())
	if err == nil {
		t.Fatal("a non-PicPak VID:PID was accepted — fail-closed gate broken")
	}
}

// TestGateFailClosedOnProbeError proves an unconfirmable device (probe error) is
// rejected under confirm_required, and accepted (descriptor-match stands) when
// confirm is not required.
func TestGateFailClosedOnProbeError(t *testing.T) {
	probeErr := errors.New("udevadm: device not found")

	strict := newGate("303a", "1001", []string{"udevadm"}, true, time.Minute)
	if err := strict.confirm(context.Background(), &fakeRunner{err: probeErr}, picpakDev()); err == nil {
		t.Fatal("confirm_required=true must reject an unconfirmable device")
	}

	lax := newGate("303a", "1001", []string{"udevadm"}, false, time.Minute)
	if err := lax.confirm(context.Background(), &fakeRunner{err: probeErr}, picpakDev()); err != nil {
		t.Fatalf("confirm_required=false should not block on a probe error: %v", err)
	}
}

// TestGateNoVIDPIDLiteralSource is a guard that the expected pair is config-driven:
// a gate built with a different configured pair confirms a device with THAT pair and
// rejects the 303a:1001 default — proving gate.go embeds no VID:PID literal of its
// own (Policy=Data; an operator override drives the comparison).
func TestGateConfigDrivenPair(t *testing.T) {
	// A gate configured for abcd:1234 confirms a device presenting abcd:1234 ...
	accept := newGate("abcd", "1234", []string{"udevadm"}, true, time.Minute)
	if err := accept.confirm(context.Background(), &fakeRunner{out: udevBody("abcd", "1234")}, picpakDev()); err != nil {
		t.Fatalf("configured pair abcd:1234 should confirm: %v", err)
	}
	// ... and a fresh gate with the same config rejects the 303a:1001 default,
	// proving gate.go embeds no VID:PID literal of its own (Policy=Data). A fresh
	// gate avoids the per-(host,tty,descriptor) confirm cache from the first call.
	reject := newGate("abcd", "1234", []string{"udevadm"}, true, time.Minute)
	if err := reject.confirm(context.Background(), &fakeRunner{out: udevBody("303a", "1001")}, picpakDev()); err == nil {
		t.Fatal("303a:1001 must be rejected when the configured pair is abcd:1234 (config-driven, no literal)")
	}
}

// TestSubstituteTTY checks the %TTY% template substitution.
func TestSubstituteTTY(t *testing.T) {
	got := substituteTTY([]string{"udevadm", "info", "-n", "%TTY%"}, "/dev/ttyACM7")
	want := []string{"udevadm", "info", "-n", "/dev/ttyACM7"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("substituteTTY = %v, want %v", got, want)
		}
	}
}
