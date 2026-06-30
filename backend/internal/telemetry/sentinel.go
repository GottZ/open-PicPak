package telemetry

import "encoding/json"

// This file is the SINGLE place a raw scanned column (a nullable pointer) or an extra-JSONB field is
// mapped to a typed Null[T] with its OWN per-column sentinel set. The mapping is deliberately per-column
// (design 08 §5, D22.5): the generic numeric columns use -1 + SQL NULL; the SMALLINT diag_* columns use
// NULL only (no numeric sentinel, so a real -32768 survives); rssi/temp use -127/-32768 but ONLY inside
// the extra JSONB, never against a SMALLINT column.

// nullI32 maps a nullable INTEGER pointer (batt_mv, bad_boots, …) to Null[int]. applyGeneric=true treats
// SentinelGeneric (-1) as n/a (batt_mv); false uses NULL only (bad_boots has no numeric sentinel).
func nullI32(p *int32, applyGeneric bool) Null[int] {
	if p == nil {
		return Null[int]{}
	}
	if applyGeneric && int64(*p) == SentinelGeneric {
		return Null[int]{}
	}
	return some(int(*p))
}

// nullI16 maps a nullable SMALLINT pointer (batt_pct, diag_runstate, diag_o0/o1, …) to Null[int].
// applyGeneric=true treats -1 as n/a (batt_pct); false uses NULL only, so a legitimate SMALLINT diag
// value (incl. -32768) is preserved.
func nullI16(p *int16, applyGeneric bool) Null[int] {
	if p == nil {
		return Null[int]{}
	}
	if applyGeneric && int64(*p) == SentinelGeneric {
		return Null[int]{}
	}
	return some(int(*p))
}

// nullI64 maps a nullable BIGINT pointer (uptime_ms, boot_count, diag_*) to Null[int64].
// applyGeneric=true treats -1 as n/a (uptime_ms); false uses NULL only.
func nullI64(p *int64, applyGeneric bool) Null[int64] {
	if p == nil {
		return Null[int64]{}
	}
	if applyGeneric && *p == SentinelGeneric {
		return Null[int64]{}
	}
	return some(*p)
}

// nullStr maps a nullable TEXT pointer to Null[string] (NULL → n/a).
func nullStr(p *string) Null[string] {
	if p == nil {
		return Null[string]{}
	}
	return some(*p)
}

// nullBool maps a nullable BOOLEAN pointer to Null[bool] (NULL → n/a).
func nullBool(p *bool) Null[bool] {
	if p == nil {
		return Null[bool]{}
	}
	return some(*p)
}

// derefStr returns "" for a NULL TEXT pointer (used for running_ver/channel where an empty string and
// NULL render identically as "unknown").
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// DecodeExtra opportunistically decodes the telemetry.extra JSONB (design 08 §1.1). rssi/heap/temp/tx
// live ONLY here — there is no column — so the reader must tolerate absence (→ n/a) and any future
// additive key without breaking. rssi applies its -127 sentinel and temp its -32768 sentinel; heap/tx
// have no sentinel. A malformed or NULL extra yields an all-n/a Extra (Present=false), never an error.
func DecodeExtra(raw []byte) Extra {
	var e Extra
	if len(raw) == 0 {
		return e
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return e
	}
	e.Present = true
	e.RSSI = extraInt(m, "rssi", int64(SentinelRSSI), true)
	e.Temp = extraInt(m, "temp", int64(SentinelTemp), true)
	e.Heap = extraInt(m, "heap", 0, false)
	e.TX = extraInt(m, "tx", 0, false)
	return e
}

// extraInt reads one numeric extra-JSONB key. A missing key, a non-numeric value, or (when
// applySentinel) the metric's own sentinel maps to n/a. JSON numbers decode as float64 then truncate to
// int — telemetry metrics are small integers.
func extraInt(m map[string]json.RawMessage, key string, sentinel int64, applySentinel bool) Null[int] {
	raw, ok := m[key]
	if !ok {
		return Null[int]{}
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return Null[int]{}
	}
	v := int(f)
	if applySentinel && int64(v) == sentinel {
		return Null[int]{}
	}
	return some(v)
}
