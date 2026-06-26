package sshhost

import (
	"path"
	"regexp"
	"strings"
)

// discoverer parses the output of poll.command (default `ls -l /dev/serial/by-id/`)
// into []DiscoveredDevice, applying the match_regex gate on the by-id DESCRIPTOR
// name (not the tty) so an unrelated ttyUSB Zigbee dongle is dropped, and
// extracting the real tty path plus a best-effort MAC. It holds the compiled
// regexes so a poll does not recompile per call. All patterns come from config
// (Policy=Data); none is a code literal.
type discoverer struct {
	match *regexp.Regexp // gate on the by-id descriptor name (poll.match_regex)
	mac   *regexp.Regexp // best-effort MAC extraction (poll.mac_regex); may be nil
	host  string
}

// newDiscoverer compiles the gate + MAC regexes for a host. A bad match_regex is a
// hard error (the gate is load-bearing — a wrong gate could false-accept a Zigbee
// dongle); a bad mac_regex degrades to no-MAC (enrichment is best-effort).
func newDiscoverer(host, matchRegex, macRegex string) (*discoverer, error) {
	m, err := regexp.Compile(matchRegex)
	if err != nil {
		return nil, err
	}
	d := &discoverer{match: m, host: host}
	if macRegex != "" {
		if mr, err := regexp.Compile(macRegex); err == nil {
			d.mac = mr
		}
		// A bad mac_regex is non-fatal: MAC stays empty (best-effort, §1.1).
	}
	return d, nil
}

// parse turns the raw `ls -l /dev/serial/by-id/` output into the gated device set.
//
// Each `ls -l` symlink line looks like:
//
//	lrwxrwxrwx 1 root root 13 Jun 26 12:00 <descriptor> -> ../../ttyACM0
//
// We key the gate on the <descriptor> (the by-id name) and resolve the tty from the
// "-> ../../ttyACMx" target. A line without a symlink arrow, or whose descriptor
// fails the gate, is dropped. The robust identity is (host, tty); MAC is enrichment.
func (d *discoverer) parse(raw string) []DiscoveredDevice {
	var out []DiscoveredDevice
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		desc, target, ok := splitSymlinkLine(line)
		if !ok {
			continue
		}
		// THE GATE: only a descriptor matching match_regex is accepted. A Zigbee
		// ttyUSB dongle has a different descriptor and is dropped here (§2.2, §9).
		if !d.match.MatchString(desc) {
			continue
		}
		tty := resolveTTY(target)
		if tty == "" {
			continue
		}
		dev := DiscoveredDevice{
			Host:       d.host,
			TTY:        tty,
			Descriptor: desc,
		}
		if d.mac != nil {
			if m := d.mac.FindString(desc); m != "" {
				dev.MAC = normalizeMAC(m)
			}
		}
		out = append(out, dev)
	}
	return out
}

// splitSymlinkLine extracts (descriptor, linkTarget) from one `ls -l` symlink line.
// It splits on the " -> " arrow: the token immediately before the arrow is the
// descriptor (the by-id basename), the token after is the link target. Returns
// ok=false for any line that is not a symlink (no arrow).
func splitSymlinkLine(line string) (descriptor, target string, ok bool) {
	const arrow = " -> "
	i := strings.Index(line, arrow)
	if i < 0 {
		return "", "", false
	}
	left := line[:i]
	target = strings.TrimSpace(line[i+len(arrow):])
	// The descriptor is the last whitespace-separated field on the left side (the
	// ls -l columns precede the filename; the by-id name is the final field).
	fields := strings.Fields(left)
	if len(fields) == 0 {
		return "", "", false
	}
	descriptor = fields[len(fields)-1]
	if descriptor == "" || target == "" {
		return "", "", false
	}
	return descriptor, target, true
}

// resolveTTY normalizes a symlink target like "../../ttyACM0" to "/dev/ttyACM0".
// A target that is already absolute is returned as-is. The by-id symlink farm lives
// under /dev/serial/by-id/, so a relative "../../ttyACMx" resolves to /dev/ttyACMx.
func resolveTTY(target string) string {
	if target == "" {
		return ""
	}
	if strings.HasPrefix(target, "/") {
		return path.Clean(target)
	}
	// Relative to /dev/serial/by-id/ — join and clean.
	return path.Clean(path.Join("/dev/serial/by-id", target))
}

// normalizeMAC lowercases and colon-separates a matched MAC tail to the canonical
// form (OQ 5 pins lowercase, colon-separated). It accepts the `:`/`_`/`-`
// separators mac_regex allows (and the no-separator form) and re-emits colons.
func normalizeMAC(m string) string {
	// Strip the allowed separators, then re-group into pairs with colons.
	var hex strings.Builder
	for _, r := range m {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			hex.WriteRune(r)
		}
	}
	h := strings.ToLower(hex.String())
	if len(h) != 12 {
		return strings.ToLower(m) // not a clean 6-octet MAC; return lowercased input
	}
	var b strings.Builder
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}
