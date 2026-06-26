package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Opts controls how Load resolves and reads the config.
type Opts struct {
	// Path is the --config flag value (absolute or relative to CWD). Empty = none.
	Path string

	// RequireFile, when true, makes "no config file found" a hard error. When
	// false (default), Load runs against the compiled defaults + env overlay so a
	// read-less / first-run invocation still works.
	RequireFile bool

	// AllowCWDFile enables the ./picpak-ops.toml resolution candidate. It must
	// only be set by a caller that has confirmed the root .gitignore covers
	// picpak-ops.toml (the air-gap fail-closed posture; see resolve.go).
	AllowCWDFile bool

	// Overrides are CLI-supplied field overrides applied AFTER env, the
	// last-writer-wins layer. Each mutates the merged config in place.
	Overrides []func(*Config)

	// Getenv overrides os.Getenv for testing. nil = os.Getenv.
	Getenv func(string) string

	// ReloadDebounce is the fsnotify coalesce window the watcher uses. The shell
	// sets it from the loaded reload.debounce so the watcher need not re-read the
	// config to know its own debounce; zero falls back to the watcher default.
	ReloadDebounce time.Duration
}

func (o Opts) getenv(name string) string {
	if o.Getenv != nil {
		return o.Getenv(name)
	}
	return os.Getenv(name)
}

// Loaded is the result of a successful Load: the frozen config plus the path it
// came from ("" if it ran on defaults+env with no file). The path is what
// watch.go watches for hot-reload.
type Loaded struct {
	Config *Config
	Path   string
}

// Load resolves the config path, strict-decodes the file (rejecting unknown
// keys), merges enumerated env overrides, applies CLI overrides, resolves secret
// indirection, validates (aggregating all errors), and returns a frozen *Config.
//
// K4: Load returns a *Config (pointer). The shell holds it and the watch path
// emits a new *Config on reload.
func Load(o Opts) (*Config, error) {
	l, err := LoadWithPath(o)
	if err != nil {
		return nil, err
	}
	return l.Config, nil
}

// LoadWithPath is Load but also returns the resolved file path (for the watcher).
func LoadWithPath(o Opts) (*Loaded, error) {
	cfg := defaults()

	path, err := resolvePath(o)
	switch {
	case errors.Is(err, errNoConfig):
		if o.RequireFile {
			return nil, fmt.Errorf("config: %w (set --config, $PICPAK_OPS_CONFIG, or an XDG config)", err)
		}
		path = ""
	case err != nil:
		return nil, fmt.Errorf("config: %w", err)
	}

	if path != "" {
		if err := decodeStrict(path, cfg); err != nil {
			return nil, err
		}
	}

	// Layering: defaults → file → env → CLI overrides (last writer wins per field).
	applyEnv(cfg, o.getenv)
	for _, ov := range o.Overrides {
		if ov != nil {
			ov(cfg)
		}
	}

	if err := resolveSecrets(cfg, o.getenv); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	cfg.freeze()
	return &Loaded{Config: cfg, Path: path}, nil
}

// decodeStrict decodes the TOML file into cfg and rejects unknown keys: a
// misspelled key silently defaulting is exactly the "magic value crept back in"
// failure the strict loader prevents (validation rule 9).
func decodeStrict(path string, cfg *Config) error {
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return fmt.Errorf("config: decoding %q: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, 0, len(undec))
		for _, k := range undec {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		return fmt.Errorf("config: %q: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	return nil
}

// resolveSecrets resolves $ENV:/$FILE: indirection on every secret-bearing field
// in place, after the env/CLI overlay (so an indirection supplied via env is
// resolved too). It fails closed: an indirection that resolves to nothing errors.
func resolveSecrets(cfg *Config, getenv func(string) string) error {
	v, err := resolveSecret("database.dsn", cfg.Database.DSN, getenv)
	if err != nil {
		return err
	}
	cfg.Database.DSN = v

	v, err = resolveSecret("ota.admin_api_token", cfg.OTA.AdminAPIToken, getenv)
	if err != nil {
		return err
	}
	cfg.OTA.AdminAPIToken = v

	return nil
}

// freeze builds the O(1) lookup indexes (lookup.go) so a frozen *Config answers
// HostByName / DeviceBySerial / DevicesByHost without rescanning the slices. A
// Config is never mutated after Load returns; hot-reload produces a new *Config.
func (c *Config) freeze() {
	c.buildIndexes()
}
