package ota

// resolve.go holds the LOAD-BEARING pure functions: the precedence resolver and the
// per-device assembly. They do no I/O and import nothing but the package's own types,
// so they are exhaustively table-testable. The resolver is labeled "predicted (no
// backend)" — the future GET /admin/resolve/{serial} is the authoritative resolver and
// must implement the SAME precedence; resolver drift is the documented risk (design 07
// §Risks).

// DefaultPrecedence is the fixed contract order when ota.resolve_precedence is unset:
// a pinned per-serial active target beats a non-pinned per-serial active target beats a
// channel-fleet ('*') active target beats the channel default. A paused/done target
// offers nothing (it is invisible to every step), so resolution falls through to the
// next step.
var DefaultPrecedence = []string{"pinned-serial", "serial", "channel", "default"}

// Resolve computes the predicted target version for one device by walking the
// configured precedence (default DefaultPrecedence) and returning the FIRST step that
// yields a candidate. All per-serial and channel-fleet steps consider only ACTIVE
// targets in the DEVICE'S authoritative channel (rollout_targets is UNIQUE(serial,
// channel), and the device has exactly one channel, so the lookup key is unambiguous);
// a paused/done target matches no step. When no step yields a version the target is
// empty with Source "none" (the device stays).
//
// Resolve is PURE: identical inputs always yield the identical ResolvedTarget. It is
// the predicted resolver — Predicted is always true here.
func Resolve(dev DeviceInfo, rollouts []Rollout, channels []Channel, precedence []string) ResolvedTarget {
	if len(precedence) == 0 {
		precedence = DefaultPrecedence
	}
	for _, step := range precedence {
		if v, ok := resolveStep(step, dev, rollouts, channels); ok && v != "" {
			return ResolvedTarget{Version: v, Source: step, Predicted: true}
		}
	}
	return ResolvedTarget{Version: "", Source: "none", Predicted: true}
}

// resolveStep evaluates ONE named precedence step. An unknown step name yields no
// candidate (defensive: a typo in ota.resolve_precedence is skipped, not fatal).
func resolveStep(step string, dev DeviceInfo, rollouts []Rollout, channels []Channel) (string, bool) {
	switch step {
	case "pinned-serial":
		if r := findActiveRollout(rollouts, dev.Serial, dev.Channel, true); r != nil {
			return r.Version, true
		}
	case "serial":
		if r := findActiveRollout(rollouts, dev.Serial, dev.Channel, false); r != nil {
			return r.Version, true
		}
	case "channel":
		if r := findActiveRollout(rollouts, "*", dev.Channel, false); r != nil {
			return r.Version, true
		}
	case "default":
		for _, c := range channels {
			if c.Name == dev.Channel && c.DefaultVersion != "" {
				return c.DefaultVersion, true
			}
		}
	}
	return "", false
}

// findActiveRollout returns the active rollout matching (serial, channel), optionally
// requiring pinned=true. Only state=="active" qualifies — a paused/done target is
// skipped — so it offers nothing and resolution falls through.
func findActiveRollout(rollouts []Rollout, serial, channel string, requirePinned bool) *Rollout {
	for i := range rollouts {
		r := &rollouts[i]
		if r.Serial != serial || r.Channel != channel {
			continue
		}
		if r.State != StateActive {
			continue
		}
		if requirePinned && !r.Pinned {
			continue
		}
		return r
	}
	return nil
}

// matchRollout returns the per-serial rollout (any state) for a device's channel, so
// the pane can act on it (toggle_state / mark_done / unpin) and show its state. It is
// state-agnostic on purpose: an operator must see and resume a PAUSED per-serial
// target, which Resolve (active-only) deliberately ignores.
func matchRollout(rollouts []Rollout, serial, channel string) *Rollout {
	for i := range rollouts {
		r := &rollouts[i]
		if r.Serial == serial && r.Channel == channel {
			return r
		}
	}
	return nil
}

// BuildDeviceOTA assembles the per-device target-vs-running rows from the device set
// (fleet cache ∪ telemetry running rows, prepared by the pane) and the OTA state. It is
// pure: it runs Resolve per device, attaches the matched per-serial rollout, and
// computes the honest Behind verdict. The result is sorted by serial for a stable view.
func BuildDeviceOTA(devices []DeviceInfo, state OTAState, precedence []string) []DeviceOTA {
	out := make([]DeviceOTA, 0, len(devices))
	for _, d := range devices {
		resolved := Resolve(d, state.Rollouts, state.Channels, precedence)
		row := DeviceOTA{
			DeviceInfo: d,
			Resolved:   resolved,
			Rollout:    matchRollout(state.Rollouts, d.Serial, d.Channel),
		}
		// Behind only when the device reported a running version AND a target is
		// offered AND they differ (the firmware's strncmp(running, server, 32) == 0
		// means "no update", so inequality = behind). Never inferred for a device that
		// never reported (HasReport == false / RunningVer == "").
		if d.HasReport && d.RunningVer != "" && resolved.Version != "" && d.RunningVer != resolved.Version {
			row.Behind = true
		}
		out = append(out, row)
	}
	sortDevicesBySerial(out)
	return out
}

// sortDevicesBySerial is a small insertion sort (device counts are small; avoids a
// sort import for one call) giving a stable serial-ordered view.
func sortDevicesBySerial(rows []DeviceOTA) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Serial < rows[j-1].Serial; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
