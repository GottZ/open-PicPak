package flash

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// WriteEntry is one (offset, file) pair in the flash write set. File is the
// build-dir-relative path (e.g. "bootloader/bootloader.bin") used both to reproduce
// the staging subdirs and to anchor the on-host path; LocalPath is the source on the
// build machine (sha256 + copy); Size is the local file size in bytes, used for the
// [offset, offset+size) overlap check against the protected NVS region.
type WriteEntry struct {
	Name      string
	Offset    uint64
	Size      int64  // local file size; 0 = unknown (overlap check then degrades to a point check)
	File      string // relative to the build/artifact dir (subdir-preserving)
	LocalPath string // source on the build machine
}

// WriteSet is the ordered flash recipe. Entries are SORTED ASCENDING BY OFFSET so the
// app (0x20000) is written LAST — the abort-safety property (a killed write leaves the
// bootloader/parttable/otadata intact and only the app partial, which is re-flashable;
// this is a deliberate divergence from the IDF flasher_args.json key order, which puts
// the app second).
type WriteSet struct {
	Entries []WriteEntry
}

// NVSGuard is the protected factory-NVS region [Start, Start+Len): RF-cal / base-MAC
// / serial / netcache. It is the SINGLE source of truth from config
// (flash.nvs_preserve_offset + the fixed region width), never re-defined here.
type NVSGuard struct {
	Start uint64
	Len   uint64
}

// nvsRegionLen is the partitions.csv nvs region size (0x6000). It mirrors the config
// validator's nvsLen so flash enforces the same width the config-time overlap check
// uses; both derive from partitions.csv, one fact in two enforcement sites.
const nvsRegionLen = 0x6000

// GuardFromConfig resolves the protected region from the config-owned
// flash.nvs_preserve_offset (the single source of truth). A malformed offset is a
// config-load error upstream; defensively it yields a zero-width guard at 0 here,
// which still rejects an injected 0x0-region overlap.
func GuardFromConfig(fc config.Flash) NVSGuard {
	start, err := parseOffset(fc.NVSPreserveOffset)
	if err != nil {
		return NVSGuard{Start: 0, Len: 0}
	}
	return NVSGuard{Start: start, Len: nvsRegionLen}
}

// BuildWriteSet resolves the write set from the config-owned build.artifacts (the ONE
// offset→file source, already cross-checked against flasher_args.json by axis 04). It
// parses each offset, anchors the local source path under artifactDir, stats the file
// best-effort for the overlap size, and SORTS ASCENDING BY OFFSET (app last). A
// malformed offset is a hard error (fail-closed: no partial write set is returned).
func BuildWriteSet(artifactDir string, artifacts []config.BuildArtifact) (*WriteSet, error) {
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("flash: empty build.artifacts (nothing to flash)")
	}
	ws := &WriteSet{Entries: make([]WriteEntry, 0, len(artifacts))}
	for _, a := range artifacts {
		off, err := parseOffset(a.Offset)
		if err != nil {
			return nil, fmt.Errorf("flash: artifact %q has an invalid offset %q: %w", a.Name, a.Offset, err)
		}
		local := resolveUnder(artifactDir, a.File)
		var size int64
		if fi, statErr := os.Stat(local); statErr == nil {
			size = fi.Size()
		}
		ws.Entries = append(ws.Entries, WriteEntry{
			Name:      a.Name,
			Offset:    off,
			Size:      size,
			File:      a.File,
			LocalPath: local,
		})
	}
	// Ascending by offset → the app at 0x20000 is written LAST (abort-safety).
	sort.SliceStable(ws.Entries, func(i, j int) bool { return ws.Entries[i].Offset < ws.Entries[j].Offset })
	return ws, nil
}

// eraseVerbs are the esptool subcommands that destroy NVS — rejected in BOTH the
// dashed (esptool 9.x) and underscored (older) spellings, defensively, wherever they
// appear in the resolved argv (an operator could smuggle one via flash.extra_args).
var eraseVerbs = map[string]struct{}{
	"erase-flash":  {},
	"erase_flash":  {},
	"erase-region": {},
	"erase_region": {},
	"erase-all":    {},
	"erase_all":    {},
}

// ValidateWriteSet is THE enforcement site of the inviolable NVS-preserve rule (the
// methodology's NEGATIVE-tested guard: red→green). It is fail-closed — any violation
// returns an error and the controller performs NO HostRegistry.Run:
//
//  1. it scans the resolved argv for any erase verb (dashed AND underscored) — so an
//     erase smuggled into extra_args or the verb position is refused; and
//  2. it rejects any write-set entry whose [offset, offset+size) interval overlaps the
//     protected NVS region [guard.Start, guard.Start+guard.Len). With an unknown size
//     (stat failed) it degrades to a point check (offset inside the region → reject),
//     which still catches the catastrophic offset==0x9000 case.
//
// It consumes the config-owned guard; it introduces NO second preserve-region surface.
func ValidateWriteSet(ws *WriteSet, argv []string, guard NVSGuard) error {
	for _, tok := range argv {
		if _, bad := eraseVerbs[strings.ToLower(strings.TrimSpace(tok))]; bad {
			return fmt.Errorf("flash: refusing to run — argv carries the erase verb %q, which would destroy factory NVS (fail-closed)", tok)
		}
	}
	if ws == nil {
		return fmt.Errorf("flash: nil write set")
	}
	gStart, gEnd := guard.Start, guard.Start+guard.Len
	for _, e := range ws.Entries {
		start := e.Offset
		end := e.Offset + 1 // point check fallback when size is unknown
		if e.Size > 0 {
			end = e.Offset + uint64(e.Size)
		}
		// Overlap of [start,end) with [gStart,gEnd): start < gEnd && gStart < end.
		if guard.Len > 0 && start < gEnd && gStart < end {
			return fmt.Errorf("flash: refusing to run — artifact %q [%#x,%#x) overlaps the protected NVS region [%#x,%#x); writing it would clobber factory RF-cal/MAC/serial (fail-closed)",
				e.Name, start, end, gStart, gEnd)
		}
	}
	return nil
}

// parseOffset parses a hex (0x-prefixed) flash offset, mirroring the config loader's
// strconv.ParseUint(…,0,64) so "0x20000" decodes identically on both sides.
func parseOffset(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty offset")
	}
	return strconv.ParseUint(s, 0, 64)
}

// resolveUnder joins p onto base, leaving an absolute p as-is and an empty p as base
// (the same rule the build axis uses, so paths resolve consistently across axes).
func resolveUnder(base, p string) string {
	if p == "" {
		return base
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
