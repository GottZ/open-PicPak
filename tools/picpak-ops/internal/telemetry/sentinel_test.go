package telemetry

import "testing"

func i16(v int16) *int16 { return &v }
func i32(v int32) *int32 { return &v }
func i64(v int64) *int64 { return &v }

// TestSentinel_SmallintDiagSurvives is the gate assertion that a real -32768 in a
// SMALLINT diag_* column is NOT masked (NULL-only sentinel, applyGeneric=false).
func TestSentinel_SmallintDiagSurvives(t *testing.T) {
	got := nullI16(i16(-32768), false) // diag_runstate/diag_o0/diag_o1 mapping
	v, ok := got.Get()
	if !ok || v != -32768 {
		t.Fatalf("real -32768 in a SMALLINT diag column was masked: got (%d, %v)", v, ok)
	}
	// The same -32768 is never an rssi/temp sentinel here — those live only in extra.
}

func TestSentinel_GenericColumns(t *testing.T) {
	// batt_pct / batt_mv / uptime_ms: -1 + NULL → n/a.
	if _, ok := nullI16(i16(-1), true).Get(); ok {
		t.Fatal("batt_pct -1 sentinel should be n/a")
	}
	if v, ok := nullI16(i16(82), true).Get(); !ok || v != 82 {
		t.Fatalf("batt_pct 82 should survive: (%d,%v)", v, ok)
	}
	if _, ok := nullI32(i32(-1), true).Get(); ok {
		t.Fatal("batt_mv -1 sentinel should be n/a")
	}
	if v, ok := nullI32(i32(3950), true).Get(); !ok || v != 3950 {
		t.Fatalf("batt_mv 3950 should survive: (%d,%v)", v, ok)
	}
	if _, ok := nullI64(i64(-1), true).Get(); ok {
		t.Fatal("uptime_ms -1 sentinel should be n/a")
	}
	if v, ok := nullI64(i64(123456), true).Get(); !ok || v != 123456 {
		t.Fatalf("uptime_ms 123456 should survive: (%d,%v)", v, ok)
	}
}

func TestSentinel_NoSentinelColumns(t *testing.T) {
	// bad_boots / boot_count: NULL only (no numeric sentinel) — a literal -1 is NOT
	// treated as n/a (proves the per-column, not global, sentinel set).
	if v, ok := nullI32(i32(-1), false).Get(); !ok || v != -1 {
		t.Fatalf("bad_boots -1 must survive (no numeric sentinel): (%d,%v)", v, ok)
	}
	if _, ok := nullI32(nil, false).Get(); ok {
		t.Fatal("NULL bad_boots should be n/a")
	}
}

func TestDecodeExtra(t *testing.T) {
	// rssi -127 sentinel → n/a; a real rssi survives.
	if _, ok := DecodeExtra([]byte(`{"rssi":-127}`)).RSSI.Get(); ok {
		t.Fatal("rssi -127 sentinel must be n/a")
	}
	if v, ok := DecodeExtra([]byte(`{"rssi":-60}`)).RSSI.Get(); !ok || v != -60 {
		t.Fatalf("rssi -60 should survive: (%d,%v)", v, ok)
	}
	// temp -32768 sentinel → n/a (only in extra; never against a SMALLINT column).
	if _, ok := DecodeExtra([]byte(`{"temp":-32768}`)).Temp.Get(); ok {
		t.Fatal("temp -32768 sentinel must be n/a")
	}
	if v, ok := DecodeExtra([]byte(`{"temp":21}`)).Temp.Get(); !ok || v != 21 {
		t.Fatalf("temp 21 should survive: (%d,%v)", v, ok)
	}
	// heap/tx have no sentinel.
	if v, ok := DecodeExtra([]byte(`{"heap":40000,"tx":11}`)).Heap.Get(); !ok || v != 40000 {
		t.Fatalf("heap 40000 should survive: (%d,%v)", v, ok)
	}
	// absent keys → n/a; present-but-empty object → Present true.
	e := DecodeExtra([]byte(`{}`))
	if !e.Present {
		t.Fatal("empty object should still mark Present")
	}
	if _, ok := e.RSSI.Get(); ok {
		t.Fatal("absent rssi should be n/a")
	}
	// NULL / empty / malformed → Present false, all n/a, no panic.
	if DecodeExtra(nil).Present {
		t.Fatal("nil extra should not be Present")
	}
	if DecodeExtra([]byte(`{bad`)).Present {
		t.Fatal("malformed extra should not be Present")
	}
}
