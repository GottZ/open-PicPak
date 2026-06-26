package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ValidationError aggregates every problem found in one validate() pass so the
// operator sees all of them at once, not one-at-a-time. validate() never panics.
type ValidationError struct {
	Errs []error
}

func (e *ValidationError) Error() string {
	if len(e.Errs) == 1 {
		return "config: " + e.Errs[0].Error()
	}
	parts := make([]string, len(e.Errs))
	for i, err := range e.Errs {
		parts[i] = "  - " + err.Error()
	}
	return fmt.Sprintf("config: %d validation error(s):\n%s", len(e.Errs), strings.Join(parts, "\n"))
}

// Unwrap exposes the aggregated errors to errors.Is/As over the slice.
func (e *ValidationError) Unwrap() []error { return e.Errs }

var (
	hex4Re   = regexp.MustCompile(`^[0-9a-f]{4}$`)
	serialRe = regexp.MustCompile(`^[A-Za-z0-9]{8}$`) // D8 width the backend devices PK uses
)

// validate runs structural + cross-field rules and aggregates ALL errors. The
// fail-closed rules are numbered to match 02-config.md §Validation.
func (c *Config) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	c.validateStructural(add)
	c.validateKeymap(add)
	c.validateCrossField(add)

	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errs: errs}
}

// validateStructural checks required fields, uniqueness, and parseable values.
func (c *Config) validateStructural(add func(string, ...any)) {
	// schema version (a deprecation allow-list can key off a future bump).
	if c.Schema != 0 && c.Schema != SchemaVersion {
		add("schema = %d: unsupported schema version (this build supports %d)", c.Schema, SchemaVersion)
	}

	// hosts: name required + unique.
	hostNames := map[string]bool{}
	for i, h := range c.Hosts {
		if h.Name == "" {
			add("hosts[%d].name is required", i)
			continue
		}
		if hostNames[h.Name] {
			add("hosts[%d].name %q is duplicated", i, h.Name)
		}
		hostNames[h.Name] = true
	}

	// devices: serial required, unique, D8-width.
	seenSerial := map[string]bool{}
	for i, d := range c.Devices {
		if d.Serial == "" {
			add("devices[%d].serial is required", i)
		} else {
			if seenSerial[d.Serial] {
				add("devices[%d].serial %q is duplicated", i, d.Serial)
			}
			seenSerial[d.Serial] = true
			if !serialRe.MatchString(d.Serial) {
				add("devices[%d].serial %q must be the D8 8-char identifier (^[A-Za-z0-9]{8}$)", i, d.Serial)
			}
		}
	}

	// build artifact offsets must parse as hex.
	for i, a := range c.Build.Artifacts {
		if _, err := parseOffset(a.Offset); err != nil {
			add("build.artifacts[%d] (%s) offset %q is not a valid hex offset: %v", i, a.Name, a.Offset, err)
		}
	}

	// nvs preserve offset must parse as hex.
	if _, err := parseOffset(c.Flash.NVSPreserveOffset); err != nil {
		add("flash.nvs_preserve_offset %q is not a valid hex offset: %v", c.Flash.NVSPreserveOffset, err)
	}

	// enumerated string fields.
	if !oneOf(c.UI.Mouse, "none", "cell", "all") {
		add("ui.mouse %q must be one of none|cell|all", c.UI.Mouse)
	}
	if !oneOf(c.UI.TabBar, "top", "bottom", "off") {
		add("ui.tab_bar %q must be one of top|bottom|off", c.UI.TabBar)
	}
	if !oneOf(c.Flash.StageMode, "scp", "build_on_host", "local") {
		add("flash.stage_mode %q must be one of scp|build_on_host|local", c.Flash.StageMode)
	}
	if !oneOf(c.Console.LineEndings, "lf", "cr", "crlf") {
		add("console.line_endings %q must be one of lf|cr|crlf", c.Console.LineEndings)
	}
	if !oneOf(c.Logs.ColorizeBy, "serial", "source", "none") {
		add("logs.colorize_by %q must be one of serial|source|none", c.Logs.ColorizeBy)
	}
	if !oneOf(c.Telemetry.AgeFormat, "relative", "absolute") {
		add("telemetry.age_format %q must be one of relative|absolute", c.Telemetry.AgeFormat)
	}
	if !oneOf(c.Telemetry.BattUnit, "V", "mV") {
		add("telemetry.batt_unit %q must be one of V|mV", c.Telemetry.BattUnit)
	}

	// startup panes must name a known pane kind.
	for i, sp := range c.UI.StartupPanes {
		if !knownPaneKind(string(sp.Kind)) {
			add("ui.startup_panes[%d].kind %q is not a known pane kind", i, sp.Kind)
		}
	}

	// session log dir must not be an absolute literal in the example/shipped path
	// posture — an absolute path points at a real machine. Empty or XDG-relative
	// only.
	if strings.HasPrefix(c.Console.SessionLogDir, "/") {
		add("console.session_log_dir %q must be empty or an XDG-relative template, never an absolute path", c.Console.SessionLogDir)
	}
}

// validateCrossField runs the fail-closed cross-section rules (02-config.md).
func (c *Config) validateCrossField(add func(string, ...any)) {
	// Rule 1: flash.forbid_erase == false → error (inviolable; cannot be disabled).
	if !c.Flash.ForbidErase {
		add("flash.forbid_erase = false is not allowed: the NVS/erase guard is inviolable and cannot be disabled")
	}

	// Rule 2: any build.artifacts[].offset == or overlaps flash.nvs_preserve_offset → error.
	// We have no file size at config-time, so an offset that equals (or, for a
	// known fixed-size NVS region, falls inside) the preserve region is rejected.
	nvsStart, nvsErr := parseOffset(c.Flash.NVSPreserveOffset)
	if nvsErr == nil {
		const nvsLen = 0x6000 // partitions.csv nvs region size (factory RF-cal/MAC/serial)
		nvsEnd := nvsStart + nvsLen
		for i, a := range c.Build.Artifacts {
			off, err := parseOffset(a.Offset)
			if err != nil {
				continue // already reported structurally
			}
			if off >= nvsStart && off < nvsEnd {
				add("build.artifacts[%d] (%s) offset %q falls inside the protected NVS region [%#x,%#x) — would clobber factory NVS",
					i, a.Name, a.Offset, nvsStart, nvsEnd)
			}
		}
	}

	// Rule 6: poll.vendor_id / poll.product_id must each be ^[0-9a-f]{4}$.
	if !hex4Re.MatchString(c.Poll.VendorID) {
		add("poll.vendor_id %q must match ^[0-9a-f]{4}$ (a wrong/empty gate risks flashing a non-PicPak)", c.Poll.VendorID)
	}
	if !hex4Re.MatchString(c.Poll.ProductID) {
		add("poll.product_id %q must match ^[0-9a-f]{4}$ (a wrong/empty gate risks flashing a non-PicPak)", c.Poll.ProductID)
	}

	// Rule 3: ota.allow_direct_write == true requires a writable session + DSN.
	if c.OTA.AllowDirectWrite {
		if c.Database.ReadOnly {
			add("ota.allow_direct_write = true requires database.read_only = false (the direct-pgx-write path needs a writable session)")
		}
		if strings.TrimSpace(c.Database.DSN) == "" {
			add("ota.allow_direct_write = true requires a non-empty database.dsn (the direct-pgx-write path needs a write DSN)")
		}
	}

	// Rule 4: devices[].host must reference a known hosts[].name.
	hostNames := map[string]bool{}
	for _, h := range c.Hosts {
		if h.Name != "" {
			hostNames[h.Name] = true
		}
	}
	for i, d := range c.Devices {
		if d.Host != "" && !hostNames[d.Host] {
			add("devices[%d].host %q references an unknown hosts[].name", i, d.Host)
		}
	}
}

// parseOffset parses a hex (or 0x-prefixed) flash offset.
func parseOffset(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty offset")
	}
	return strconv.ParseUint(s, 0, 64)
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func knownPaneKind(kind string) bool {
	switch kind {
	case "launcher", "hosts", "build", "console", "flash", "ota", "telemetry", "logs":
		return true
	default:
		return false
	}
}
