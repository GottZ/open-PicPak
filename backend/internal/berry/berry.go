// Package berry serves the C2 Berry command capability manifest (Design 23, W1): the safe-subset
// surface the operator editor autocompletes and lints against.
//
// The manifest is declared DATA (manifest.json, go:embed'd), never a hand-typed const
// (Mechanism=Code/Policy=Data). A parity test (parity_test.go, T6) binds its non-forbidden surface
// BIDIRECTIONALLY to the firmware's be_regfunc registration sites, so the catalog can never silently
// diverge from what the device VM actually exposes (D23.2). Go cannot compile Berry, so the test
// regexes the registration sites rather than linking the surface — the same drift-guard discipline as
// the Go-linked guards elsewhere, a deliberately different mechanism.
package berry

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed manifest.json
var manifestJSON []byte

// ClassForbidden marks a render/policy-phase name that is NOT in the C2 VM (§2.4): carried in the
// manifest so the linter can name its real home and warn that calling it faults at runtime while the
// cursor still advances — but excluded from the parity surface (it has no be_regfunc in the C2 files).
const ClassForbidden = "forbidden"

// Param is one positional argument of a capability. Opt marks a trailing optional argument
// (e.g. device_sleep([s])), so the editor can render the bracketed signature honestly.
type Param struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Opt  bool   `json:"opt,omitempty"`
}

// Capability is one Berry function the C2 executor exposes (or, for class "forbidden", a render/policy
// surface absent from the C2 VM).
type Capability struct {
	Name   string  `json:"name"`
	Params []Param `json:"params"`
	Ret    string  `json:"ret"`
	Class  string  `json:"class"`            // command|query|intent|store|rtc|dev|forbidden
	Risk   string  `json:"risk,omitempty"`   // "severing": can cut the device's own C2 path (USB recovery only)
	MinFW  string  `json:"min_fw,omitempty"` // advisory; enforced only once running_ver is known (§4.2)
	Doc    string  `json:"doc"`
}

// Manifest is the full served catalog: the capability list plus the enabled Berry builtins/keywords
// (so the linter does not false-positive on print/json/if).
type Manifest struct {
	Capabilities []Capability `json:"capabilities"`
	Builtins     []string     `json:"builtins"`
}

var parsed Manifest

func init() {
	if err := json.Unmarshal(manifestJSON, &parsed); err != nil {
		panic(fmt.Sprintf("berry: embedded manifest.json is invalid: %v", err))
	}
}

// Get returns the parsed manifest (the editor catalog).
func Get() Manifest { return parsed }

// NonForbiddenNames is the set of capability names that MUST have a real be_regfunc on the device —
// the parity-tested surface (command+query+intent+store+rtc+dev), excluding the forbidden class.
func (m Manifest) NonForbiddenNames() map[string]bool {
	out := make(map[string]bool, len(m.Capabilities))
	for _, c := range m.Capabilities {
		if c.Class != ClassForbidden {
			out[c.Name] = true
		}
	}
	return out
}
