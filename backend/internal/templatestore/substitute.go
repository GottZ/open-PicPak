package templatestore

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// SubstituteError is a client-facing substitution failure. Code is the wire error code the apply
// handler maps to a 422 (unknown_param / missing_param / unresolved_placeholder /
// egress_host_mismatch / invalid_param); Msg is the human string. A *SubstituteError is a defined 4xx, anything else out of
// Substitute (a malformed stored schema) is a 500 config defect.
type SubstituteError struct {
	Code string
	Msg  string
}

func (e *SubstituteError) Error() string { return e.Msg }

// paramSpec is one entry of a template's params JSONB schema
// ([{name,label,type,default?,required,options?}], §4.4). Default rides as raw JSON so a number
// default stays a number; Options backs the enum membership check.
type paramSpec struct {
	Name     string          `json:"name"`
	Label    string          `json:"label"`
	Type     string          `json:"type"`
	Required bool            `json:"required"`
	Default  json.RawMessage `json:"default,omitempty"`
	Options  []string        `json:"options,omitempty"`
}

var placeholderRe = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// Substitute validates the caller's params against the template's params schema, splices them into
// {{name}} tokens, and returns the substituted source (§4.4). It is the full-validation replacement
// for the apply handler's earlier local splice helper (W4/W5 seam).
//
// Contract (all fail-closed 422 via *SubstituteError):
//   - a provided param key the schema does not declare → unknown_param (§4.4 "unbekannter Param-Key
//     → 422"; fail-closed: an undeclared key that covers a {{token}} would otherwise splice with NO
//     type/host check — an undeclared url placeholder would take SSRF-capable values straight past
//     the K9 gate. The schema is the only door into the source.)
//   - a schema param that is required and neither provided nor defaulted → missing_param
//   - a provided/defaulted value for a schema param of type url whose host is NOT in the template's
//     egress_allow → egress_host_mismatch (K9 SSRF gate: url params must not widen the pinned host)
//   - a schema param of type number/enum whose value fails the type/format check → invalid_param
//   - any {{token}} still present after substitution → unresolved_placeholder
func Substitute(t *Template, params map[string]any) (string, error) {
	specs, err := parseParamSchema(t.Params)
	if err != nil {
		return "", fmt.Errorf("templatestore: invalid params schema: %w", err)
	}

	// Unknown-key reject FIRST: only declared params may be provided at all. Everything after this
	// line can trust that every value it splices went through the schema's type/host discipline.
	declared := make(map[string]bool, len(specs))
	for _, s := range specs {
		declared[s.Name] = true
	}
	for k := range params {
		if !declared[k] {
			return "", &SubstituteError{Code: "unknown_param",
				Msg: "parameter is not declared in the template's params schema: " + k}
		}
	}

	// Effective values = caller-provided ∪ schema defaults (a default fills an omitted param).
	values := make(map[string]any, len(params)+len(specs))
	for k, v := range params {
		values[k] = v
	}
	for _, s := range specs {
		if _, provided := values[s.Name]; !provided {
			if len(s.Default) > 0 {
				var dv any
				if err := json.Unmarshal(s.Default, &dv); err != nil {
					return "", fmt.Errorf("templatestore: param %q default: %w", s.Name, err)
				}
				values[s.Name] = dv
			} else if s.Required {
				return "", &SubstituteError{Code: "missing_param", Msg: "required parameter is missing: " + s.Name}
			} else {
				// Optional, no default, not provided: nothing to substitute — if the source references
				// {{name}} it surfaces below as unresolved_placeholder (fail-closed).
				continue
			}
		}
		if err := checkParamType(s, values[s.Name], t.EgressAllow); err != nil {
			return "", err
		}
	}

	out := spliceTokens(t.Source, values)
	if tok := placeholderRe.FindString(out); tok != "" {
		return "", &SubstituteError{Code: "unresolved_placeholder", Msg: "unresolved placeholder after substitution: " + tok}
	}
	return out, nil
}

// parseParamSchema decodes the params JSONB. An empty/absent schema is the unparametrised default ([]).
func parseParamSchema(raw json.RawMessage) ([]paramSpec, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var specs []paramSpec
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// checkParamType enforces the schema type/format for a DECLARED param value. url is the K9 SSRF gate:
// the value's host must be in the template's fixed egress_allow (egress_allow is not parametrisable,
// §3.1/§4.4). number/enum are format checks. string and unknown types pass through.
func checkParamType(s paramSpec, val any, egressAllow []string) error {
	switch s.Type {
	case "url":
		str, ok := val.(string)
		if !ok {
			return &SubstituteError{Code: "invalid_param", Msg: "url parameter must be a string: " + s.Name}
		}
		host, ok := urlHost(str)
		if !ok {
			return &SubstituteError{Code: "invalid_param", Msg: "url parameter is not a valid URL: " + s.Name}
		}
		if !hostInAllowlist(host, egressAllow) {
			return &SubstituteError{Code: "egress_host_mismatch",
				Msg: "url host " + host + " is not in the template's egress allow-list"}
		}
	case "number":
		if !isNumeric(val) {
			return &SubstituteError{Code: "invalid_param", Msg: "parameter must be a number: " + s.Name}
		}
	case "enum":
		if !containsString(s.Options, fmt.Sprint(val)) {
			return &SubstituteError{Code: "invalid_param", Msg: "parameter " + s.Name + " is not one of its allowed options"}
		}
	}
	return nil
}

// spliceTokens replaces every {{name}} with its value in one pass (no re-substitution of a value that
// happens to look like a token — such a leftover is caught as unresolved_placeholder).
func spliceTokens(source string, values map[string]any) string {
	if len(values) == 0 {
		return source
	}
	pairs := make([]string, 0, len(values)*2)
	for name, val := range values {
		pairs = append(pairs, "{{"+name+"}}", fmt.Sprint(val))
	}
	return strings.NewReplacer(pairs...).Replace(source)
}

// urlHost extracts the hostname (no port) from a url param value that may or may not carry a scheme
// ("example.com/a", "images.example.com:443/x", "https://host/p" all resolve to their host).
func urlHost(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if u.Host == "" {
		// No authority component (scheme-less "host/path" or an opaque "host:port/path"): reparse as
		// authority so url.Parse populates Host.
		u, err = url.Parse("//" + raw)
		if err != nil {
			return "", false
		}
	}
	h := u.Hostname()
	return h, h != ""
}

// hostInAllowlist reports whether host matches any egress_allow entry at the HOST level (the design
// gate is "gegen den Host", §4.4). Entries may carry a port (images.example.com:443, ha.local:8123)
// or be bare (example.com); the port is stripped before comparison. An empty allow-list matches
// nothing → fail-closed 422.
func hostInAllowlist(host string, allow []string) bool {
	for _, entry := range allow {
		if hostOf(entry) == host {
			return true
		}
	}
	return false
}

// hostOf strips a :port suffix from an egress_allow entry; a bare host is returned unchanged.
func hostOf(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func isNumeric(val any) bool {
	switch v := val.(type) {
	case float64, int, int64:
		return true
	case json.Number:
		return true
	case string:
		_, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil
	}
	return false
}

func containsString(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}
