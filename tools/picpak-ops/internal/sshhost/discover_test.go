package sshhost

import "testing"

// goldenByID is a synthetic `ls -l /dev/serial/by-id/` capture. It is plausible,
// not on-device-real: it includes
//   - a PicPak (ESP32-C3 native USB-Serial-JTAG, public/generic descriptor) → ACCEPT
//   - a second PicPak on another tty (two devices, one host) → ACCEPT, distinct by tty
//   - a Sonoff-style Zigbee dongle on a ttyUSB with a CP210x descriptor → DROP
//   - the leading `total` line ls -l prints → ignored
//
// TODO(on-device): confirm the by-id descriptor format the ESP32-C3 ROM
// USB-Serial-JTAG actually presents (does the symlink name carry the WIFI_STA MAC,
// and in what case/separator?) and REPLACE this golden with a real PicPak capture
// before match_regex / mac_regex are treated as settled (Masterplan K11 / OQ 6).
const goldenByID = `total 0
lrwxrwxrwx 1 root root 13 Jun 26 12:00 usb-Espressif_USB_JTAG_serial_debug_unit_AA-BB-CC-DD-EE-FF-if00 -> ../../ttyACM0
lrwxrwxrwx 1 root root 13 Jun 26 12:00 usb-Espressif_USB_JTAG_serial_debug_unit_11-22-33-44-55-66-if00 -> ../../ttyACM1
lrwxrwxrwx 1 root root 13 Jun 26 12:01 usb-Silicon_Labs_Sonoff_Zigbee_3.0_USB_Dongle_Plus_0001-if00-port0 -> ../../ttyUSB0
`

func newTestDiscoverer(t *testing.T) *discoverer {
	t.Helper()
	// Use the config defaults' patterns (the same values the shipped tool runs with).
	d, err := newDiscoverer("host-a",
		"usb-Espressif_USB_JTAG.*-if00",
		"([0-9A-Fa-f]{2}([:_-]?[0-9A-Fa-f]{2}){5})")
	if err != nil {
		t.Fatalf("newDiscoverer: %v", err)
	}
	return d
}

// TestDiscoverGoldenGate is the load-bearing air-gap-of-hardware test: the Zigbee
// ttyUSB line MUST be dropped by the descriptor gate, and the PicPak lines MUST be
// accepted with their resolved tty paths.
func TestDiscoverGoldenGate(t *testing.T) {
	d := newTestDiscoverer(t)
	devs := d.parse(goldenByID)

	if len(devs) != 2 {
		t.Fatalf("expected 2 accepted PicPak devices, got %d: %+v", len(devs), devs)
	}

	// Both must be on the host, with the two distinct ttyACM paths (two PicPaks,
	// one host, distinguished by tty per §9).
	gotTTY := map[string]bool{}
	for _, dev := range devs {
		if dev.Host != "host-a" {
			t.Errorf("device host = %q, want host-a", dev.Host)
		}
		gotTTY[dev.TTY] = true
	}
	for _, want := range []string{"/dev/ttyACM0", "/dev/ttyACM1"} {
		if !gotTTY[want] {
			t.Errorf("expected an accepted device on %s; got ttys %v", want, gotTTY)
		}
	}

	// The Zigbee ttyUSB0 must NEVER appear (the whole point of the gate).
	for _, dev := range devs {
		if dev.TTY == "/dev/ttyUSB0" {
			t.Fatalf("Zigbee ttyUSB0 was accepted — the VID gate failed: %+v", dev)
		}
	}
}

// TestDiscoverMACBestEffort proves the best-effort MAC extraction normalizes to the
// canonical lowercase colon form when the descriptor carries one — while remaining
// best-effort (a descriptor without a MAC yields an empty MAC, not an error).
func TestDiscoverMACBestEffort(t *testing.T) {
	d := newTestDiscoverer(t)
	devs := d.parse(goldenByID)

	byTTY := map[string]DiscoveredDevice{}
	for _, dev := range devs {
		byTTY[dev.TTY] = dev
	}
	if got := byTTY["/dev/ttyACM0"].MAC; got != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("ttyACM0 MAC = %q, want aa:bb:cc:dd:ee:ff (normalized)", got)
	}
	if got := byTTY["/dev/ttyACM1"].MAC; got != "11:22:33:44:55:66" {
		t.Errorf("ttyACM1 MAC = %q, want 11:22:33:44:55:66 (normalized)", got)
	}

	// A device must be usable (distinct by tty) even when no MAC is present — the
	// MAC is enrichment, not a precondition (§1.1).
	noMAC, err := newDiscoverer("host-b", "usb-Espressif_USB_JTAG.*-if00", "")
	if err != nil {
		t.Fatalf("newDiscoverer: %v", err)
	}
	out := noMAC.parse("lrwxrwxrwx 1 root root 13 Jun 26 12:00 usb-Espressif_USB_JTAG_serial_debug_unit_x-if00 -> ../../ttyACM3\n")
	if len(out) != 1 {
		t.Fatalf("expected 1 device with no mac_regex, got %d", len(out))
	}
	if out[0].MAC != "" {
		t.Errorf("expected empty MAC with no mac_regex, got %q", out[0].MAC)
	}
	if out[0].TTY != "/dev/ttyACM3" {
		t.Errorf("tty = %q, want /dev/ttyACM3", out[0].TTY)
	}
}

// TestDiscoverIgnoresNonSymlinkAndEmpty proves the parser ignores the `total` line,
// blank lines, and any non-symlink line without panicking.
func TestDiscoverIgnoresNonSymlinkAndEmpty(t *testing.T) {
	d := newTestDiscoverer(t)
	raw := "total 0\n\n" +
		"drwxr-xr-x 2 root root 60 Jun 26 12:00 .\n" + // a dir line (no arrow) → ignored
		"garbage without an arrow\n"
	if devs := d.parse(raw); len(devs) != 0 {
		t.Fatalf("expected 0 devices from non-symlink input, got %d: %+v", len(devs), devs)
	}
}

// TestResolveTTY checks relative and absolute symlink-target resolution.
func TestResolveTTY(t *testing.T) {
	cases := map[string]string{
		"../../ttyACM0":    "/dev/ttyACM0",
		"/dev/ttyACM2":     "/dev/ttyACM2",
		"../../../ttyUSB0": "/ttyUSB0", // over-relative still cleans deterministically
		"":                 "",
	}
	for in, want := range cases {
		if got := resolveTTY(in); got != want {
			t.Errorf("resolveTTY(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormalizeMAC checks the canonical-form normalization across the separator
// variants mac_regex permits.
func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"AA:BB:CC:DD:EE:FF": "aa:bb:cc:dd:ee:ff",
		"aa-bb-cc-dd-ee-ff": "aa:bb:cc:dd:ee:ff",
		"AABBCCDDEEFF":      "aa:bb:cc:dd:ee:ff",
		"aa_bb_cc_dd_ee_ff": "aa:bb:cc:dd:ee:ff",
	}
	for in, want := range cases {
		if got := normalizeMAC(in); got != want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}
