package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes content to a temp file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "picpak-ops.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

// loadString loads a config from raw TOML with a controlled (empty) environment
// so env overlay never interferes with a negative test.
func loadString(content string) (*Config, error) {
	p := mustTemp(content)
	defer os.Remove(p)
	return Load(Opts{Path: p, Getenv: func(string) string { return "" }})
}

func mustTemp(content string) string {
	f, err := os.CreateTemp("", "picpak-ops-*.toml")
	if err != nil {
		panic(err)
	}
	_, _ = f.WriteString(content)
	_ = f.Close()
	return f.Name()
}

// A minimal valid base config the negative tests mutate one rule at a time.
const baseValid = `
schema = 1
[database]
dsn = "postgres://user:pass@db.example:5432/picpak?sslmode=require"
`

// TestValidate_Negative drives each validation rule by a config that violates it
// (NEGATIVE-first per methodology). Each case asserts Load returns an error whose
// message names the offending rule.
func TestValidate_Negative(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		wantSub string // substring expected in the aggregated error
	}{
		{
			name:    "rule1_forbid_erase_false",
			toml:    baseValid + "\n[flash]\nforbid_erase = false\n",
			wantSub: "flash.forbid_erase = false is not allowed",
		},
		{
			name: "rule2_artifact_overlaps_nvs",
			// app artifact moved onto 0x9000 (the protected NVS region).
			toml: baseValid + `
[[build.artifacts]]
name = "bootloader"
file = "bootloader/bootloader.bin"
offset = "0x0"
[[build.artifacts]]
name = "app"
file = "picpak_fw.bin"
offset = "0x9000"
`,
			wantSub: "protected NVS region",
		},
		{
			name:    "rule3_direct_write_read_only",
			toml:    baseValid + "\n[ota]\nallow_direct_write = true\n",
			wantSub: "requires database.read_only = false",
		},
		{
			name: "rule3_direct_write_empty_dsn",
			toml: `
schema = 1
[database]
read_only = false
[ota]
allow_direct_write = true
`,
			wantSub: "requires a non-empty database.dsn",
		},
		{
			name: "rule4_device_unknown_host",
			toml: baseValid + `
[[devices]]
serial = "PLACEHLD"
host = "nope"
`,
			wantSub: "references an unknown hosts[].name",
		},
		{
			name:    "rule6_vendor_id_bad",
			toml:    baseValid + "\n[poll]\nvendor_id = \"ZZZZ\"\n",
			wantSub: "poll.vendor_id",
		},
		{
			name:    "rule6_product_id_bad",
			toml:    baseValid + "\n[poll]\nproduct_id = \"10\"\n",
			wantSub: "poll.product_id",
		},
		{
			name:    "rule7_double_bind_global",
			toml:    baseValid + "\n[keys]\nquit = [\"x\"]\nhelp = [\"x\"]\n",
			wantSub: "bound to both",
		},
		{
			name:    "rule9_unknown_key",
			toml:    baseValid + "\n[flash]\nnot_a_real_key = 1\n",
			wantSub: "unknown key",
		},
		{
			name:    "structural_bad_offset",
			toml:    baseValid + "\n[[build.artifacts]]\nname=\"app\"\nfile=\"x.bin\"\noffset=\"nothex\"\n",
			wantSub: "not a valid hex offset",
		},
		{
			name: "structural_duplicate_host",
			toml: baseValid + `
[[hosts]]
name = "host-a"
[[hosts]]
name = "host-a"
`,
			wantSub: "duplicated",
		},
		{
			name: "structural_duplicate_serial",
			toml: baseValid + `
[[devices]]
serial = "PLACEHLD"
[[devices]]
serial = "PLACEHLD"
`,
			wantSub: "duplicated",
		},
		{
			name: "structural_bad_serial_width",
			toml: baseValid + `
[[devices]]
serial = "TOOLONG12"
`,
			wantSub: "D8 8-char identifier",
		},
		{
			name:    "structural_bad_mouse_enum",
			toml:    baseValid + "\n[ui]\nmouse = \"wiggle\"\n",
			wantSub: "ui.mouse",
		},
		{
			name:    "structural_bad_stage_mode",
			toml:    baseValid + "\n[flash]\nstage_mode = \"telepathy\"\n",
			wantSub: "flash.stage_mode",
		},
		{
			name:    "structural_absolute_session_log",
			toml:    baseValid + "\n[console]\nsession_log_dir = \"/var/log/picpak\"\n",
			wantSub: "never an absolute path",
		},
		{
			name:    "structural_unknown_startup_pane_kind",
			toml:    baseValid + "\n[[ui.startup_panes]]\nkind = \"frobnicate\"\n",
			wantSub: "not a known pane kind",
		},
		{
			name:    "schema_version_mismatch",
			toml:    "schema = 999\n" + "[database]\ndsn=\"x\"\n",
			wantSub: "unsupported schema version",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadString(tc.toml)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestValidate_AggregatesAllErrors proves validate reports every problem at once,
// not the first one only.
func TestValidate_AggregatesAllErrors(t *testing.T) {
	toml := `
schema = 1
[database]
dsn = "x"
[flash]
forbid_erase = false
[poll]
vendor_id = "ZZZZ"
product_id = "10"
`
	_, err := loadString(toml)
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if len(ve.Errs) < 3 {
		t.Fatalf("expected at least 3 aggregated errors, got %d: %v", len(ve.Errs), ve)
	}
}

// TestValidate_PositiveBase confirms the minimal base config loads clean.
func TestValidate_PositiveBase(t *testing.T) {
	cfg, err := loadString(baseValid)
	if err != nil {
		t.Fatalf("base config should load: %v", err)
	}
	if cfg.Flash.ForbidErase != true {
		t.Fatal("forbid_erase default should be true")
	}
}

// TestLoad_DefaultsApply confirms an absent key keeps its compiled default and
// the env overlay wins over the file.
func TestLoad_DefaultsApply(t *testing.T) {
	p := writeConfig(t, baseValid)
	cfg, err := Load(Opts{Path: p, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Poll.VendorID != "303a" || cfg.Poll.ProductID != "1001" {
		t.Fatalf("VID:PID defaults not applied: %q:%q", cfg.Poll.VendorID, cfg.Poll.ProductID)
	}
	if cfg.UI.TeardownDeadline.String() != "5s" {
		t.Fatalf("teardown_deadline default not applied: %s", cfg.UI.TeardownDeadline)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	p := writeConfig(t, baseValid)
	env := map[string]string{
		"PICPAK_OPS_DB_DSN":      "postgres://e:e@env.example:5432/db",
		"PICPAK_OPS_ADMIN_URL":   "https://env-admin.example/admin",
		"PICPAK_OPS_ADMIN_TOKEN": "ENV_TOKEN",
	}
	cfg, err := Load(Opts{Path: p, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(cfg.Database.DSN, "env.example") {
		t.Fatalf("env DSN override not applied: %q", cfg.Database.DSN)
	}
	if cfg.OTA.AdminAPIToken != "ENV_TOKEN" {
		t.Fatalf("env token override not applied: %q", cfg.OTA.AdminAPIToken)
	}
}

// TestSecretIndirection covers $ENV: / $FILE: resolution and the fail-closed
// behavior when an indirection resolves to nothing.
func TestSecretIndirection(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		toml := "schema=1\n[database]\ndsn=\"$ENV:MY_DSN\"\n"
		cfg, err := loadStringEnv(toml, map[string]string{"MY_DSN": "postgres://r:r@host.example/d"})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !strings.Contains(cfg.Database.DSN, "host.example") {
			t.Fatalf("env indirection not resolved: %q", cfg.Database.DSN)
		}
	})

	t.Run("env_missing_fails_closed", func(t *testing.T) {
		toml := "schema=1\n[database]\ndsn=\"$ENV:NOPE\"\n"
		_, err := loadStringEnv(toml, map[string]string{})
		if err == nil || !strings.Contains(err.Error(), "is empty or unset") {
			t.Fatalf("expected fail-closed missing-env error, got: %v", err)
		}
	})

	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		secret := filepath.Join(dir, "token")
		if err := os.WriteFile(secret, []byte("  FILE_TOKEN\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		toml := "schema=1\n[database]\ndsn=\"x\"\n[ota]\nadmin_api_token=\"$FILE:" + secret + "\"\n"
		cfg, err := loadStringEnv(toml, map[string]string{})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.OTA.AdminAPIToken != "FILE_TOKEN" {
			t.Fatalf("file indirection not resolved/trimmed: %q", cfg.OTA.AdminAPIToken)
		}
	})
}

func loadStringEnv(content string, env map[string]string) (*Config, error) {
	p := mustTemp(content)
	defer os.Remove(p)
	return Load(Opts{Path: p, Getenv: func(k string) string { return env[k] }})
}

// TestRedaction proves secrets/hosts never render their raw value.
func TestRedaction(t *testing.T) {
	cfg := defaults()
	cfg.Database.DSN = "postgres://secret:pw@private.host/db"
	cfg.OTA.AdminAPIURL = "https://private-admin.host/admin"
	cfg.OTA.AdminAPIToken = "supersecrettoken"

	s := cfg.String()
	for _, leak := range []string{"private.host", "private-admin.host", "supersecrettoken", "secret:pw"} {
		if strings.Contains(s, leak) {
			t.Fatalf("redacted String() leaked %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, "«secret:database.dsn»") {
		t.Fatalf("expected secret key-name tag, got: %s", s)
	}

	// Same host hashes stably; different hosts differ.
	a := RedactHost("h", "host.example")
	b := RedactHost("h", "host.example")
	c := RedactHost("h", "other.example")
	if a != b {
		t.Fatalf("redaction not stable: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("redaction collided across distinct hosts: %q", a)
	}
	if strings.Contains(a, "host.example") {
		t.Fatalf("redaction leaked host: %q", a)
	}
}

// TestLookups proves the O(1) accessors built at freeze.
func TestLookups(t *testing.T) {
	toml := `
schema = 1
[database]
dsn = "x"
[[hosts]]
name = "host-a"
ssh_target = "user@host.example"
[[devices]]
serial = "AAAA0000"
host = "host-a"
[[devices]]
serial = "BBBB1111"
host = "host-a"
`
	cfg, err := loadString(toml)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if h, ok := cfg.HostByName("host-a"); !ok || h.SSHTarget != "user@host.example" {
		t.Fatalf("HostByName failed: %+v ok=%v", h, ok)
	}
	if _, ok := cfg.HostByName("missing"); ok {
		t.Fatal("HostByName found a missing host")
	}
	if d, ok := cfg.DeviceBySerial("BBBB1111"); !ok || d.Host != "host-a" {
		t.Fatalf("DeviceBySerial failed: %+v ok=%v", d, ok)
	}
	if got := cfg.DevicesByHost("host-a"); len(got) != 2 {
		t.Fatalf("DevicesByHost expected 2, got %d", len(got))
	}
}

// TestKeymap covers single-key vs chord parsing and the per-scope no-double-bind.
func TestKeymap(t *testing.T) {
	km, err := BuildKeymap(Keys{
		Quit:    []string{"ctrl+c", "q"},
		Help:    []string{"?"},
		NextTab: []string{"ctrl+b z"}, // a chord
	})
	if err != nil {
		t.Fatalf("BuildKeymap: %v", err)
	}
	if _, ok := km.Bindings["quit"]; !ok {
		t.Fatal("expected single-key binding for quit")
	}
	chords := km.Chords["next_tab"]
	if len(chords) != 1 || chords[0].Prefix != "ctrl+b" || chords[0].Key != "z" {
		t.Fatalf("chord not parsed: %+v", chords)
	}

	// A binding may repeat across DIFFERENT scopes legally.
	if err := ValidateKeyScope("ota", map[string][]string{"register": {"r"}}); err != nil {
		t.Fatalf("pane scope reuse should be legal: %v", err)
	}
	// But not twice in the SAME scope.
	err = ValidateKeyScope("global", map[string][]string{"a": {"x"}, "b": {"x"}})
	if err == nil || !strings.Contains(err.Error(), "bound to both") {
		t.Fatalf("expected same-scope double-bind error, got: %v", err)
	}

	// 3+ key chord is rejected.
	if _, err := BuildKeymap(Keys{Quit: []string{"a b c"}}); err == nil {
		t.Fatal("expected error for 3-key chord")
	}
}

// TestLoad_NoFileUsesDefaults confirms a read-less run works without RequireFile.
func TestLoad_NoFileUsesDefaults(t *testing.T) {
	cfg, err := Load(Opts{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("no-file load should succeed: %v", err)
	}
	if cfg.Poll.VendorID != "303a" {
		t.Fatalf("defaults not applied on no-file load: %q", cfg.Poll.VendorID)
	}
}

// TestLoad_RequireFileErrors confirms RequireFile fails closed with no candidate.
func TestLoad_RequireFileErrors(t *testing.T) {
	_, err := Load(Opts{RequireFile: true, Getenv: func(string) string { return "" }})
	if err == nil {
		t.Fatal("RequireFile with no candidate should error")
	}
}
