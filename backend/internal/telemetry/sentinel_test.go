package telemetry

import "testing"

// T2 — the sentinel is PER-COLUMN, never a blanket integer list (D22.5). The single distinction that
// matters: -127/-32768 are bound ONLY to the JSONB rssi/temp metrics; a genuine -32768 in a SMALLINT
// diag_* column is a real reading and must survive, while -1 in a generic numeric column is n/a.

func i16(v int16) *int16 { return &v }
func i32(v int32) *int32 { return &v }
func i64p(v int64) *int64 { return &v }

func TestSentinel_PerColumn_T2(t *testing.T) {
	// a real -32768 in a SMALLINT diag column (NULL-only sentinel) survives — NOT masked to n/a.
	if v, ok := nullI16(i16(-32768), false).Get(); !ok || v != -32768 {
		t.Fatalf("diag SMALLINT -32768 must survive; got (%d, %v)", v, ok)
	}
	// the generic -1 sentinel on batt_pct (SMALLINT, applyGeneric=true) → n/a.
	if _, ok := nullI16(i16(-1), true).Get(); ok {
		t.Fatal("batt_pct -1 generic sentinel must be n/a")
	}
	// the same -1 on a no-sentinel column (bad_boots INTEGER, applyGeneric=false) is a real value.
	if v, ok := nullI32(i32(-1), false).Get(); !ok || v != -1 {
		t.Fatalf("a no-sentinel column must keep -1; got (%d, %v)", v, ok)
	}
	// batt_mv -1 generic sentinel → n/a; a real value passes through.
	if _, ok := nullI32(i32(-1), true).Get(); ok {
		t.Fatal("batt_mv -1 generic sentinel must be n/a")
	}
	if v, ok := nullI32(i32(3990), true).Get(); !ok || v != 3990 {
		t.Fatalf("batt_mv real value must pass; got (%d, %v)", v, ok)
	}
	// uptime_ms BIGINT -1 sentinel → n/a (the rollback-poison guard at scan time).
	if _, ok := nullI64(i64p(-1), true).Get(); ok {
		t.Fatal("uptime_ms -1 sentinel must be n/a")
	}
	// a nil pointer (SQL NULL) is always n/a.
	if _, ok := nullI16(nil, false).Get(); ok {
		t.Fatal("SQL NULL must be n/a")
	}
}

func TestDecodeExtra_Sentinels_T2(t *testing.T) {
	// rssi -127 and temp -32768 are the JSONB metric sentinels → n/a; the SAME -32768 in a SMALLINT
	// column survived above. This is the crux: the value is identical, the column binding differs.
	e := DecodeExtra([]byte(`{"rssi":-127,"temp":-32768,"heap":54000,"tx":11}`))
	if !e.Present {
		t.Fatal("extra with keys must be Present")
	}
	if _, ok := e.RSSI.Get(); ok {
		t.Fatal("rssi -127 sentinel must be n/a")
	}
	if _, ok := e.Temp.Get(); ok {
		t.Fatal("temp -32768 sentinel must be n/a")
	}
	if v, ok := e.Heap.Get(); !ok || v != 54000 {
		t.Fatalf("heap (no sentinel) must pass; got (%d, %v)", v, ok)
	}
	if v, ok := e.TX.Get(); !ok || v != 11 {
		t.Fatalf("tx (no sentinel) must pass; got (%d, %v)", v, ok)
	}

	// a real rssi reading passes through.
	if v, ok := DecodeExtra([]byte(`{"rssi":-50}`)).RSSI.Get(); !ok || v != -50 {
		t.Fatalf("real rssi must pass; got (%d, %v)", v, ok)
	}
	// an absent key, an empty payload, and malformed JSON all yield n/a / not-Present, never an error.
	if _, ok := DecodeExtra([]byte(`{"heap":1}`)).RSSI.Get(); ok {
		t.Fatal("absent rssi key must be n/a")
	}
	if e := DecodeExtra(nil); e.Present {
		t.Fatal("nil extra must be not-Present")
	}
	if e := DecodeExtra([]byte(`not json`)); e.Present {
		t.Fatal("malformed extra must be not-Present (no error, no panic)")
	}
}
