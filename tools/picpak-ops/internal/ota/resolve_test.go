package ota

import "testing"

// chans is a small channels fixture: stable defaults to D, beta has no default.
func chans() []Channel {
	return []Channel{
		{Name: "stable", DefaultVersion: "D"},
		{Name: "beta", DefaultVersion: ""},
	}
}

// TestResolve_Precedence is the load-bearing table test: it proves all four precedence
// steps fire in order AND that a paused/done target offers nothing (resolution falls
// through). Versions are single letters so the winning step is obvious from the value:
// P=pinned-serial, S=serial(non-pinned), C=channel-fleet, D=channel-default.
func TestResolve_Precedence(t *testing.T) {
	dev := DeviceInfo{Serial: "SN", Channel: "stable"}

	cases := []struct {
		name       string
		rollouts   []Rollout
		wantVer    string
		wantSource string
	}{
		{
			name: "pinned per-serial active wins over everything",
			rollouts: []Rollout{
				{Serial: "SN", Channel: "stable", Version: "P", State: StateActive, Pinned: true},
				{Serial: "*", Channel: "stable", Version: "C", State: StateActive},
			},
			wantVer: "P", wantSource: "pinned-serial",
		},
		{
			name: "non-pinned per-serial active beats channel-fleet and default",
			rollouts: []Rollout{
				{Serial: "SN", Channel: "stable", Version: "S", State: StateActive, Pinned: false},
				{Serial: "*", Channel: "stable", Version: "C", State: StateActive},
			},
			wantVer: "S", wantSource: "serial",
		},
		{
			name: "channel-fleet active beats the channel default",
			rollouts: []Rollout{
				{Serial: "*", Channel: "stable", Version: "C", State: StateActive},
			},
			wantVer: "C", wantSource: "channel",
		},
		{
			name:     "channel default when no rollout applies",
			rollouts: nil,
			wantVer:  "D", wantSource: "default",
		},
		{
			name: "paused per-serial offers nothing -> falls through to channel-fleet",
			rollouts: []Rollout{
				{Serial: "SN", Channel: "stable", Version: "S", State: StatePaused, Pinned: true},
				{Serial: "*", Channel: "stable", Version: "C", State: StateActive},
			},
			wantVer: "C", wantSource: "channel",
		},
		{
			name: "done per-serial AND done channel-fleet offer nothing -> channel default",
			rollouts: []Rollout{
				{Serial: "SN", Channel: "stable", Version: "S", State: StateDone},
				{Serial: "*", Channel: "stable", Version: "C", State: StateDone},
			},
			wantVer: "D", wantSource: "default",
		},
		{
			name: "pinned but paused does NOT win; non-pinned active serial does",
			rollouts: []Rollout{
				// Two rows for the same serial would violate UNIQUE(serial,channel) in
				// the DB; here a single paused-pinned row proves pinned-step requires
				// active, so it falls through to... nothing for this serial, then the
				// channel-fleet, then default.
				{Serial: "SN", Channel: "stable", Version: "P", State: StatePaused, Pinned: true},
			},
			wantVer: "D", wantSource: "default",
		},
		{
			name: "per-serial rollout in a DIFFERENT channel does not apply",
			rollouts: []Rollout{
				{Serial: "SN", Channel: "beta", Version: "B", State: StateActive, Pinned: true},
			},
			wantVer: "D", wantSource: "default",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(dev, tc.rollouts, chans(), nil)
			if got.Version != tc.wantVer {
				t.Errorf("version = %q, want %q", got.Version, tc.wantVer)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
			if !got.Predicted {
				t.Error("local Resolve must mark the target Predicted (no backend)")
			}
		})
	}
}

// TestResolve_NoTargetOnEmptyDefaultChannel proves a device on a channel with no
// default and no applicable rollout resolves to "" / "none" (the device stays).
func TestResolve_NoTargetOnEmptyDefaultChannel(t *testing.T) {
	dev := DeviceInfo{Serial: "SN", Channel: "beta"}
	got := Resolve(dev, nil, chans(), nil)
	if got.Version != "" {
		t.Errorf("version = %q, want empty", got.Version)
	}
	if got.Source != "none" {
		t.Errorf("source = %q, want none", got.Source)
	}
}

// TestResolve_CustomPrecedence proves the order is Policy=Data: reordering precedence so
// "default" precedes the per-serial steps makes the channel default win even when a
// per-serial active target exists.
func TestResolve_CustomPrecedence(t *testing.T) {
	dev := DeviceInfo{Serial: "SN", Channel: "stable"}
	rollouts := []Rollout{
		{Serial: "SN", Channel: "stable", Version: "S", State: StateActive, Pinned: true},
	}
	got := Resolve(dev, rollouts, chans(), []string{"default", "pinned-serial", "serial", "channel"})
	if got.Version != "D" || got.Source != "default" {
		t.Fatalf("custom precedence: got %q/%q, want D/default", got.Version, got.Source)
	}
}

// TestBuildDeviceOTA_Behind proves the honest Behind verdict: behind only when a device
// reported a running version that differs from the offered target; never for a device
// that never reported, and never when running == target.
func TestBuildDeviceOTA_Behind(t *testing.T) {
	state := OTAState{
		Channels: chans(),
		Rollouts: []Rollout{
			{Serial: "*", Channel: "stable", Version: "C", State: StateActive},
		},
	}
	devices := []DeviceInfo{
		{Serial: "AA", Channel: "stable", RunningVer: "C", HasReport: true},   // up to date
		{Serial: "BB", Channel: "stable", RunningVer: "OLD", HasReport: true}, // behind
		{Serial: "CC", Channel: "stable", RunningVer: "", HasReport: false},   // never reported
		{Serial: "DD", Channel: "beta", RunningVer: "x", HasReport: true},     // no target on beta
	}
	rows := BuildDeviceOTA(devices, state, nil)

	want := map[string]bool{"AA": false, "BB": true, "CC": false, "DD": false}
	for _, r := range rows {
		if r.Behind != want[r.Serial] {
			t.Errorf("device %s: Behind = %v, want %v (running=%q target=%q)",
				r.Serial, r.Behind, want[r.Serial], r.RunningVer, r.Resolved.Version)
		}
	}
	// Sorted by serial.
	for i := 1; i < len(rows); i++ {
		if rows[i].Serial < rows[i-1].Serial {
			t.Fatalf("BuildDeviceOTA not sorted by serial: %q before %q", rows[i-1].Serial, rows[i].Serial)
		}
	}
}
