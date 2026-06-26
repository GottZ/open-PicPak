package flash

import (
	"regexp"
	"strconv"
	"strings"
)

// writingRe matches esptool's per-file progress line, e.g.
//
//	Writing at 0x00020000... (37 %)
//
// capturing the percentage. The address is the live write position (not the file
// base), so it is not used to attribute the line to a file — sequencing by verified
// count is the robust attribution (see Progress).
var writingRe = regexp.MustCompile(`Writing at 0x[0-9a-fA-F]+\.\.\.\s*\((\d+)\s*%\)`)

// hashVerifiedRe matches the per-file integrity line esptool prints once a file's
// flashed contents read back with a matching hash:
//
//	Hash of data verified.
var hashVerifiedRe = regexp.MustCompile(`Hash of data verified\.?`)

// hashMismatchRe matches a failed verify (esptool wording varies across versions).
var hashMismatchRe = regexp.MustCompile(`(?i)hash of data (does not match|verification failed|mismatch)`)

// leavingRe matches the terminal lines esptool prints after the last file.
var leavingRe = regexp.MustCompile(`(?i)^\s*(Leaving\.\.\.|Hard resetting|Staying in bootloader)`)

// Progress is the per-device parse state. It is fed esptool output line-by-line
// (live in the pane for the matrix, and once over the full ring in the controller for
// the authoritative terminal verdict). A device is SUCCESS only when every file in the
// write set reported a verified hash AND esptool exited 0 (Success).
type Progress struct {
	NumFiles    int  // files in the write set (the verified-all denominator)
	Verified    int  // count of "Hash of data verified." lines
	CurrentFile int  // index of the file currently being written (== Verified until done)
	CurrentPct  int  // last "Writing at … (NN %)" percentage
	Mismatch    bool // a hash-mismatch line was seen (hard fail signal)
	Leaving     bool // esptool reached its terminal/reset phase
}

// NewProgress builds a parser for a write set of numFiles files.
func NewProgress(numFiles int) *Progress { return &Progress{NumFiles: numFiles} }

// Ingest folds one output line into the parse state. It is cheap and allocation-free
// on the common (non-matching) line, so it is safe to call per RunOutputMsg.
func (p *Progress) Ingest(line string) {
	line = strings.TrimRight(line, "\r")
	if m := writingRe.FindStringSubmatch(line); m != nil {
		if pct, err := strconv.Atoi(m[1]); err == nil {
			p.CurrentPct = pct
		}
		// The file being written is the next not-yet-verified one.
		p.CurrentFile = p.Verified
		if p.CurrentFile >= p.NumFiles && p.NumFiles > 0 {
			p.CurrentFile = p.NumFiles - 1
		}
		return
	}
	if hashMismatchRe.MatchString(line) {
		p.Mismatch = true
		return
	}
	if hashVerifiedRe.MatchString(line) {
		p.Verified++
		p.CurrentPct = 0
		return
	}
	if leavingRe.MatchString(line) {
		p.Leaving = true
	}
}

// AllVerified reports whether every file in the write set verified its hash.
func (p *Progress) AllVerified() bool { return p.NumFiles > 0 && p.Verified >= p.NumFiles }

// Success is the authoritative per-device verdict: every file hash-verified AND a
// clean (exit 0) esptool exit AND no observed mismatch. Anything else is a failure.
func (p *Progress) Success(exitCode int) bool {
	return exitCode == 0 && !p.Mismatch && p.AllVerified()
}

// IngestAll folds an entire output ring in order (the controller's terminal parse).
func (p *Progress) IngestAll(lines []string) {
	for _, l := range lines {
		p.Ingest(l)
	}
}
