package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errNoConfig is returned by resolvePath when no resolution candidate exists.
// Load treats it as "run with defaults" only when Opts.RequireFile is false.
var errNoConfig = fmt.Errorf("no config file found")

// resolvePath returns the path of the config file to load, applying the
// resolution order (first existing wins):
//
//  1. --config <path> (Opts.Path).
//  2. $PICPAK_OPS_CONFIG.
//  3. $XDG_CONFIG_HOME/picpak-ops/config.toml (fallback ~/.config/...).
//  4. ./picpak-ops.toml in CWD — DISABLED unless Opts.AllowCWDFile is true.
//
// Candidate #4 is the air-gap hazard (the build defaults steer the operator to
// run from the repo root, where a ./picpak-ops.toml with real values would be
// git-add-able). It is only a candidate once the root .gitignore covers it; the
// caller signals that by setting Opts.AllowCWDFile.
func resolvePath(o Opts) (string, error) {
	if o.Path != "" {
		p, err := expandPath(o.Path)
		if err != nil {
			return "", err
		}
		if fileExists(p) {
			return p, nil
		}
		// An explicit --config that does not exist is a hard error: the operator
		// asked for a specific file; silently falling through would run against
		// the wrong config.
		return "", fmt.Errorf("config file %q (from --config) does not exist", o.Path)
	}

	if env := o.getenv("PICPAK_OPS_CONFIG"); env != "" {
		p, err := expandPath(env)
		if err != nil {
			return "", err
		}
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("config file %q (from $PICPAK_OPS_CONFIG) does not exist", env)
	}

	if xdg := xdgConfigPath(o); xdg != "" && fileExists(xdg) {
		return xdg, nil
	}

	if o.AllowCWDFile {
		cwdFile := "picpak-ops.toml"
		if fileExists(cwdFile) {
			return cwdFile, nil
		}
	}

	return "", errNoConfig
}

// xdgConfigPath returns $XDG_CONFIG_HOME/picpak-ops/config.toml, falling back to
// ~/.config/picpak-ops/config.toml. Empty if neither base resolves.
func xdgConfigPath(o Opts) string {
	if base := o.getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "picpak-ops", "config.toml")
	}
	home := o.getenv("HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "picpak-ops", "config.toml")
}

// expandPath expands a leading ~ to $HOME and $XDG_* / $VAR references in a path.
// ssh does not ~-expand a ControlPath given via -o on every version, so the tool
// resolves a leading ~ itself for any path-like config value.
func expandPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		p = filepath.Join(home, p[2:])
	}
	return os.ExpandEnv(p), nil
}

// resolveSecret resolves a secret-bearing field's indirection at load time:
//
//   - "$ENV:NAME"  → the value of environment variable NAME.
//   - "$FILE:/path" → the trimmed contents of the file at /path (~/$VAR expanded).
//   - anything else → returned verbatim (a literal value).
//
// An indirection that resolves to nothing is an error, so a misconfigured
// $ENV:/$FILE: fails closed at load rather than silently yielding an empty
// secret that the consuming axis would then treat as "no credential".
func resolveSecret(field, raw string, getenv func(string) string) (string, error) {
	switch {
	case strings.HasPrefix(raw, "$ENV:"):
		name := strings.TrimPrefix(raw, "$ENV:")
		if name == "" {
			return "", fmt.Errorf("%s: $ENV: indirection has no variable name", field)
		}
		v := getenv(name)
		if v == "" {
			return "", fmt.Errorf("%s: environment variable %q (from $ENV:) is empty or unset", field, name)
		}
		return v, nil
	case strings.HasPrefix(raw, "$FILE:"):
		path := strings.TrimPrefix(raw, "$FILE:")
		if path == "" {
			return "", fmt.Errorf("%s: $FILE: indirection has no path", field)
		}
		ep, err := expandPath(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", field, err)
		}
		b, err := os.ReadFile(ep)
		if err != nil {
			return "", fmt.Errorf("%s: reading $FILE %q: %w", field, path, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("%s: file %q (from $FILE:) is empty", field, path)
		}
		return v, nil
	default:
		return raw, nil
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
