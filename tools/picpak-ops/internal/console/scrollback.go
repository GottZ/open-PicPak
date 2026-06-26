package console

import "strings"

// frame splits a raw device-output chunk into terminal lines on \r\n, \r, or \n.
//
// The firmware emits CRLF (console.c) and a bare CR on backspace redraw; the sshhost
// reader already splits on \n (stripping a trailing \r), so this primarily re-splits
// a chunk on interior \r and normalizes the three endings to discrete lines. A
// trailing fragment WITHOUT a terminator (e.g. the firmware's "> " prompt) is kept
// as its own last line so it is not lost; a trailing terminator does not synthesize a
// spurious empty line. Nothing is stripped from the content — the operator wants the
// raw truth — only the line endings are consumed.
//
// Banner matching runs per framed line (see consolePane.matchBanner), never anchored
// against the raw byte stream, because the firmware wraps the banner in CRLF.
func frame(s string) []string {
	if s == "" {
		return nil
	}
	// Normalize CRLF and bare CR to LF, then split: this folds all three endings.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	parts := strings.Split(s, "\n")
	// A trailing terminator yields a final "" — drop exactly one so a complete,
	// newline-terminated chunk does not append a blank line. Interior blank lines
	// (\n\n) are preserved.
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	return parts
}
