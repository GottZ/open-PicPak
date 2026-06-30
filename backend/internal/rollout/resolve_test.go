package rollout

import "testing"

// T4 — Resolve precedence (pure, table-driven, 3-tier): per-serial ▶ fleet '*' ▶ channel default.
// A paused/done row offers nothing and is SKIPPED to the next tier (D20.3). There is NO pinned tier —
// rollout_targets.pinned is vestigial and not an input to Resolve. Red cases each describe the wrong
// firmware a device would pull if a tier mis-fired.
func TestResolve(t *testing.T) {
	const ch = "stable"
	serial := func(v, state string) Target { return Target{Serial: "dev1", Channel: ch, Version: v, State: state} }
	fleet := func(v, state string) Target { return Target{Serial: "*", Channel: ch, Version: v, State: state} }

	tests := []struct {
		name      string
		rs        []Target
		chDefault string
		want      Resolved
	}{
		{
			// red: a paused per-serial row resolves, or wrong order → the device gets the fleet version.
			name: "per-serial active beats fleet and default",
			rs:   []Target{serial("v-sn", "active"), fleet("v-fleet", "active")},
			chDefault: "v-def", want: Resolved{Version: "v-sn", Source: SourceSerial},
		},
		{
			// red: no per-serial pin → fleet must win over the channel default.
			name: "fleet active beats default when no per-serial",
			rs:   []Target{fleet("v-fleet", "active")},
			chDefault: "v-def", want: Resolved{Version: "v-fleet", Source: SourceFleet},
		},
		{
			name: "channel default when no active targets",
			rs:   nil,
			chDefault: "v-def", want: Resolved{Version: "v-def", Source: SourceChannelDefault},
		},
		{
			// red: a paused per-serial row still resolves → device pulls the paused version.
			name: "paused per-serial skipped → falls to fleet",
			rs:   []Target{serial("v-sn", "paused"), fleet("v-fleet", "active")},
			chDefault: "v-def", want: Resolved{Version: "v-fleet", Source: SourceFleet},
		},
		{
			name: "done per-serial + paused fleet → falls to default",
			rs:   []Target{serial("v-sn", "done"), fleet("v-fleet", "paused")},
			chDefault: "v-def", want: Resolved{Version: "v-def", Source: SourceChannelDefault},
		},
		{
			name: "no targets + no default → none",
			rs:   nil,
			chDefault: "", want: Resolved{Source: SourceNone},
		},
		{
			// a per-serial pin paused AND no default → nothing to serve (fail-open at the caller).
			name: "all paused + no default → none",
			rs:   []Target{serial("v-sn", "paused"), fleet("v-fleet", "done")},
			chDefault: "", want: Resolved{Source: SourceNone},
		},
		{
			// a target on a DIFFERENT channel must never leak into this channel's resolution.
			name: "wrong-channel row ignored",
			rs:   []Target{{Serial: "dev1", Channel: "beta", Version: "v-beta", State: "active"}},
			chDefault: "v-def", want: Resolved{Version: "v-def", Source: SourceChannelDefault},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(ch, tc.rs, tc.chDefault, DefaultPrecedence); got != tc.want {
				t.Errorf("Resolve = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Precedence is policy=data (D20.3): reversing the tier order makes the fleet rollout beat the
// per-serial pin — same rows, different answer, no code change. Red: a hardcoded order ignores prec.
func TestResolvePrecedenceIsData(t *testing.T) {
	const ch = "stable"
	rs := []Target{
		{Serial: "dev1", Channel: ch, Version: "v-sn", State: "active"},
		{Serial: "*", Channel: ch, Version: "v-fleet", State: "active"},
	}
	reversed := []Tier{TierFleet, TierSerial}
	if got := Resolve(ch, rs, "v-def", reversed); got.Source != SourceFleet || got.Version != "v-fleet" {
		t.Errorf("reversed precedence: got %+v, want fleet/v-fleet", got)
	}
}
