package flash

import "testing"

// TestProgress_SuccessOnlyWhenAllVerifiedAndExit0 fixes the integrity contract: a
// device is SUCCESS only when every file hash-verified AND esptool exited 0.
func TestProgress_SuccessOnlyWhenAllVerifiedAndExit0(t *testing.T) {
	lines := []string{
		"esptool v9.0.0",
		"Writing at 0x00000000... (100 %)",
		"Hash of data verified.",
		"Writing at 0x00008000... (100 %)",
		"Hash of data verified.",
		"Writing at 0x00010000... (100 %)",
		"Hash of data verified.",
		"Writing at 0x00020000... (37 %)",
		"Writing at 0x00020000... (100 %)",
		"Hash of data verified.",
		"Leaving...",
		"Hard resetting via RTS pin...",
	}
	p := NewProgress(4)
	p.IngestAll(lines)

	if p.Verified != 4 {
		t.Fatalf("verified = %d, want 4", p.Verified)
	}
	if !p.AllVerified() {
		t.Fatal("AllVerified should be true after 4 verifies")
	}
	if !p.Leaving {
		t.Fatal("Leaving should be detected")
	}
	if !p.Success(0) {
		t.Fatal("all-verified + exit 0 must be success")
	}
	if p.Success(1) {
		t.Fatal("a non-zero exit must NOT be success even when all verified")
	}
}

// TestProgress_PartialVerifyIsFailure proves a clean exit with fewer verifies than
// files is NOT a success (a half-written app must never read as done).
func TestProgress_PartialVerifyIsFailure(t *testing.T) {
	p := NewProgress(4)
	p.IngestAll([]string{
		"Writing at 0x00000000... (100 %)",
		"Hash of data verified.",
		"Hash of data verified.",
		"Hash of data verified.", // only 3 of 4
		"Leaving...",
	})
	if p.AllVerified() {
		t.Fatal("3 of 4 must not be all-verified")
	}
	if p.Success(0) {
		t.Fatal("a partial verify with exit 0 must be a failure")
	}
}

// TestProgress_ParsesPercent proves the write-progress percentage is captured.
func TestProgress_ParsesPercent(t *testing.T) {
	p := NewProgress(1)
	p.Ingest("Writing at 0x00020000... (42 %)")
	if p.CurrentPct != 42 {
		t.Fatalf("CurrentPct = %d, want 42", p.CurrentPct)
	}
}

// TestProgress_HashMismatchFailsClosed proves an explicit hash mismatch is a failure
// regardless of exit code.
func TestProgress_HashMismatchFailsClosed(t *testing.T) {
	p := NewProgress(1)
	p.Ingest("A fatal error occurred: Hash of data does not match!")
	if !p.Mismatch {
		t.Fatal("hash-mismatch line should set Mismatch")
	}
	if p.Success(0) {
		t.Fatal("a hash mismatch must never be a success")
	}
}
