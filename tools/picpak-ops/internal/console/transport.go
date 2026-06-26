package console

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// placeholderRe matches any {name} template token, so an UNRESOLVED placeholder left
// after substitution is a hard error — the console must never fall back to a literal
// device node.
var placeholderRe = regexp.MustCompile(`\{[a-zA-Z_][a-zA-Z0-9_]*\}`)

// localShell wraps the remote pump for a LOCAL runner, which execs argv[0] directly
// (there is no remote login shell to interpret the `;` / `&` / `trap` pipeline). For
// an ssh host the single command string is sent verbatim to the remote login shell,
// so no wrap is needed. This is a transport MECHANISM, not an endpoint — no host,
// port, or path literal lives here.
const localShell = "sh"

// transportSubst carries the only values that touch a real environment. They arrive
// from the gated device/host record at runtime (the discovery axis owns the
// 303a:1001 gate), never from a shipped literal.
type transportSubst struct {
	Host string // ssh host alias ({host})
	Port string // resolved, VID-gated remote device node ({port})
	Baud string // cosmetic for native USB-Serial-JTAG, configurable ({baud})
}

// buildPumpArgv builds the runner-level argv for the remote serial pump from the
// console templates, substituting {host}/{port}/{baud}.
//
// Fail-loud contract: it errors on ANY unresolved placeholder — a templated token
// present in the command whose value is empty, OR an unknown {token} left after
// substitution — and NEVER falls back to a literal /dev node. wrapShell=true (a local
// runner that execs argv[0]) wraps the pump in `sh -c` so the pipeline + trap run;
// for an ssh host the verbatim command string is the single remote argv element
// (ssh sends it to the remote login shell, which runs the pipeline).
func buildPumpArgv(cc config.Console, sub transportSubst, wrapShell bool) ([]string, error) {
	cmd, err := substitute(cc.RemoteCommand, sub)
	if err != nil {
		return nil, err
	}
	if wrapShell {
		return []string{localShell, "-c", cmd}, nil
	}
	return []string{cmd}, nil
}

// substitute resolves {host}/{port}/{baud} in tmpl and fails loud on anything
// unresolved: a known placeholder present with an empty value, or any leftover
// {token} after substitution.
func substitute(tmpl string, sub transportSubst) (string, error) {
	known := map[string]string{
		"host": sub.Host,
		"port": sub.Port,
		"baud": sub.Baud,
	}
	for name, val := range known {
		if val == "" && strings.Contains(tmpl, "{"+name+"}") {
			return "", fmt.Errorf("console transport: placeholder {%s} is unresolved (empty); refusing to fall back to a literal device node", name)
		}
	}
	out := tmpl
	for name, val := range known {
		out = strings.ReplaceAll(out, "{"+name+"}", val)
	}
	if m := placeholderRe.FindString(out); m != "" {
		return "", fmt.Errorf("console transport: unresolved placeholder %s in remote_command", m)
	}
	return out, nil
}

// baudString renders the configured baud for {baud} substitution (defaults to a sane
// value if misconfigured to <= 0).
func baudString(cc config.Console) string {
	b := cc.Baud
	if b <= 0 {
		b = 115200
	}
	return strconv.Itoa(b)
}
