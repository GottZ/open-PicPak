package templatestore

import (
	"encoding/json"
	"strings"
	"testing"
)

// tmpl builds a Template for the pure-unit substitution tests (no DB): schema + source + egress_allow
// are all Substitute reads.
func tmpl(source, paramsJSON string, egress ...string) *Template {
	return &Template{Source: source, Params: json.RawMessage(paramsJSON), EgressAllow: egress}
}

func subCode(t *testing.T, err error) string {
	t.Helper()
	var se *SubstituteError
	if err == nil {
		t.Fatalf("want a SubstituteError, got nil")
	}
	if !asSubstituteError(err, &se) {
		t.Fatalf("want a *SubstituteError, got %T: %v", err, err)
	}
	return se.Code
}

// asSubstituteError is errors.As without pulling the import into every call site.
func asSubstituteError(err error, target **SubstituteError) bool {
	for err != nil {
		if se, ok := err.(*SubstituteError); ok {
			*target = se
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// T-sub-happy — a declared param substitutes and validates; a schema default fills an omitted param.
func TestSubstituteHappyAndDefault(t *testing.T) {
	schema := `[{"name":"ssid","type":"string","required":true},{"name":"n","type":"number","default":5}]`
	out, err := Substitute(tmpl("wifi({{ssid}},{{n}})", schema), map[string]any{"ssid": "home"})
	if err != nil {
		t.Fatalf("substitute: %v", err)
	}
	if out != "wifi(home,5)" {
		t.Errorf("out = %q, want wifi(home,5) (default filled n)", out)
	}
}

// T-sub-missing — a required schema param that is neither provided nor defaulted → missing_param.
// Red without the required check: it would substitute nothing and fall through as unresolved (wrong
// code) or splice an empty value silently.
func TestSubstituteMissingParam(t *testing.T) {
	schema := `[{"name":"ssid","type":"string","required":true}]`
	_, err := Substitute(tmpl("wifi({{ssid}})", schema), map[string]any{})
	if code := subCode(t, err); code != "missing_param" {
		t.Errorf("code = %q, want missing_param", code)
	}
}

// T-sub-unresolved — a {{token}} left in the source after substitution → unresolved_placeholder. Red
// without the post-substitution scan: the un-substituted token would reach the wire (the W4/W5 local
// helper left it in place).
func TestSubstituteUnresolved(t *testing.T) {
	// b is optional with no default and not provided → its {{b}} stays.
	schema := `[{"name":"a","type":"string","required":true},{"name":"b","type":"string"}]`
	_, err := Substitute(tmpl("x={{a}}{{b}}", schema), map[string]any{"a": "1"})
	if code := subCode(t, err); code != "unresolved_placeholder" {
		t.Errorf("code = %q, want unresolved_placeholder", code)
	}
}

// T-sub-url-offallow — the K9 SSRF gate: a type:url param whose host is NOT in egress_allow →
// egress_host_mismatch. Red without the host check: the off-allowlist host substitutes through and the
// minted function would try to fetch it (defense-in-depth ahead of the egress proxy).
func TestSubstituteURLOffAllowlist(t *testing.T) {
	schema := `[{"name":"url","type":"url","required":true}]`
	_, err := Substitute(tmpl("fetch({{url}})", schema, "images.example.com:443"),
		map[string]any{"url": "https://evil.example.net/x"})
	if code := subCode(t, err); code != "egress_host_mismatch" {
		t.Errorf("code = %q, want egress_host_mismatch", code)
	}
}

// T-sub-url-onallow — a type:url param whose host IS in egress_allow substitutes. Host-level match:
// the allow entry carries a port, the value carries a path — only the host must match.
func TestSubstituteURLOnAllowlist(t *testing.T) {
	schema := `[{"name":"url","type":"url","required":true}]`
	cases := []struct{ allow, val string }{
		{"images.example.com:443", "https://images.example.com:443/a.png"},
		{"images.example.com:443", "images.example.com/a.png"}, // scheme-less, no port
		{"example.com", "example.com/a"},                       // bare host both sides (W5 stamp shape)
	}
	for _, c := range cases {
		out, err := Substitute(tmpl("fetch({{url}})", schema, c.allow), map[string]any{"url": c.val})
		if err != nil {
			t.Errorf("allow=%q val=%q: unexpected err %v", c.allow, c.val, err)
			continue
		}
		if !strings.Contains(out, c.val) {
			t.Errorf("allow=%q val=%q: out=%q missing substituted url", c.allow, c.val, out)
		}
	}
}

// T-sub-enum-number — enum membership and number format are enforced for declared params.
func TestSubstituteEnumAndNumber(t *testing.T) {
	enum := `[{"name":"mode","type":"enum","options":["a","b"],"required":true}]`
	if code := subCode(t, mustErr(Substitute(tmpl("m={{mode}}", enum), map[string]any{"mode": "c"}))); code != "invalid_param" {
		t.Errorf("enum: code = %q, want invalid_param", code)
	}
	num := `[{"name":"n","type":"number","required":true}]`
	if code := subCode(t, mustErr(Substitute(tmpl("n={{n}}", num), map[string]any{"n": "notnum"}))); code != "invalid_param" {
		t.Errorf("number: code = %q, want invalid_param", code)
	}
}

// T-sub-unknown — a provided param the schema does NOT declare → 422 unknown_param (§4.4). The probe
// is the exact K9 bypass the reject closes: the template pins an egress host but its schema does not
// declare the url placeholder — an SSRF-capable value for the undeclared {{url}} must NOT splice
// through untyped (it would skip the host gate entirely). Red without the reject: Substitute returns
// the evil URL substituted into the source with no error.
func TestSubstituteUndeclaredRejected(t *testing.T) {
	// undeclared key alongside an empty schema.
	_, err := Substitute(tmpl("x={{v}}", "[]"), map[string]any{"v": "hi"})
	if code := subCode(t, err); code != "unknown_param" {
		t.Errorf("empty-schema undeclared: code = %q, want unknown_param", code)
	}

	// the K9 bypass shape: pinned egress_allow, url placeholder NOT declared, off-allowlist value.
	out, err := Substitute(tmpl("fetch({{url}})", "[]", "images.example.com:443"),
		map[string]any{"url": "https://evil.example.net/x"})
	if err == nil {
		t.Fatalf("undeclared url param spliced through the K9 gate: out = %q", out)
	}
	if code := subCode(t, err); code != "unknown_param" {
		t.Errorf("undeclared url: code = %q, want unknown_param", code)
	}

	// undeclared key alongside declared ones is rejected too.
	schema := `[{"name":"a","type":"string","required":true}]`
	_, err = Substitute(tmpl("x={{a}}", schema), map[string]any{"a": "1", "extra": "y"})
	if code := subCode(t, err); code != "unknown_param" {
		t.Errorf("extra key: code = %q, want unknown_param", code)
	}

	// empty schema + no params + no placeholder → still verbatim (unparametrised template applies).
	if out, err := Substitute(tmpl("static()", ""), map[string]any{}); err != nil || out != "static()" {
		t.Errorf("verbatim: out=%q err=%v", out, err)
	}
}

func mustErr(_ string, err error) error { return err }
