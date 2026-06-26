package flash

import "strings"

// Classify maps an esptool run's exit code + output tail + transport error into a
// FailClass and an operator recovery hint. It is heuristic over esptool's wording
// (which varies across 9.x point releases), ordered most-specific first. A clean exit
// with all-verified never reaches here — the controller calls Classify only on a
// non-success run.
//
// runErr is the transport-level error (nil for a clean non-zero exit; non-nil for a
// killed/timed-out/start-failed run). prog carries the parse state so a partial verify
// is distinguished from a sync failure.
func Classify(exitCode int, lines []string, runErr error, prog *Progress) (FailClass, string) {
	blob := strings.ToLower(strings.Join(lines, "\n"))
	if runErr != nil {
		blob += "\n" + strings.ToLower(runErr.Error())
	}

	contains := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(blob, s) {
				return true
			}
		}
		return false
	}

	switch {
	case prog != nil && prog.Mismatch,
		contains("hash of data does not match", "hash verification failed", "hash mismatch",
			"a fatal error occurred: contents differ"):
		return FailHash, "the flashed contents did not verify — re-flash; a bad cable/usb hub is the usual cause"

	case contains("could not open", "permission denied", "resource busy", "device or resource busy",
		"port doesn't exist", "no such file or directory", "could not exclusively lock"):
		return FailPortBusy, "the device port could not be opened — close any console on it (auto-released) and retry"

	case contains("failed to connect", "no serial data received", "invalid head of packet",
		"wrong boot mode detected", "timed out waiting for packet header", "failed to enter download mode"):
		return FailSync, "esptool could not sync with the ROM — replug the device or retry (download mode auto-resyncs over USB-JTAG)"

	case runErr != nil && (contains("context deadline exceeded", "signal: killed", "killed") ||
		strings.Contains(strings.ToLower(runErr.Error()), "deadline")):
		return FailTimeout, "the esptool run exceeded flash.run_timeout — retry; raise the timeout if the app is large"

	case contains("failed to leave", "hard resetting failed", "error while resetting"):
		return FailReset, "the post-write reset failed — the bytes are likely written; power-cycle the device"

	case prog != nil && prog.NumFiles > 0 && !prog.AllVerified():
		return FailHash, "not every file verified before esptool exited — re-flash; treat as a partial write"

	default:
		return FailExit, "esptool exited non-zero — see the output tail; re-flash over USB"
	}
}

// classifyTransport maps an early transport error (the run never started / Run
// returned nil) into a class before any esptool output exists.
func classifyTransport(err error) (FailClass, string) {
	if err == nil {
		return FailExit, "the flash run did not start"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "unknown host"):
		return FailExit, "the device's host is not a configured [[hosts]] entry: " + err.Error()
	case strings.Contains(s, "busy"), strings.Contains(s, "open"):
		return FailPortBusy, err.Error()
	default:
		return FailExit, err.Error()
	}
}
