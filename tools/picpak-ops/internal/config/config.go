// Package config owns the complete, validated, typed runtime configuration of
// picpak-ops. It is the single source of all variability: no other package may
// hardcode a host, device, DSN, URL, token, path, offset, esptool flag, or
// keybinding. Every consuming axis (wm-shell, build, flash, console, ssh-host,
// ota, telemetry, logs, db) receives an immutable, already-validated *Config and
// reads it — it never re-parses a file or falls back to a literal.
//
// Design: one schema (config.go), one loader (load.go), one validator
// (validate.go), one defaults table (defaults.go). The shipped tree carries only
// config.example.toml with pinned public placeholders; real values live in the
// operator's gitignored local config.
//
// No magic values: everything is configurable. (The cross-cutting requirement is
// stated in English here; it is never transcribed from its non-English source.)
package config

import (
	"time"

	"github.com/open-picpak/picpak-ops/internal/pane"
)

// SchemaVersion is the current top-level schema version. A config may set the
// `schema` key; a mismatch is reported by validation so a future deprecation
// allow-list can key off it rather than silently accepting unknown shapes.
const SchemaVersion = 1

// Config is the root document. Every nested section is owned (key names +
// defaults) by the axis named in its comment; config restates only the
// struct/validation shape. The single compiled defaults table lives in
// defaults.go — the only place a literal default may exist.
type Config struct {
	Schema int `toml:"schema"`

	Meta   Meta   `toml:"meta"`
	Reload Reload `toml:"reload"`

	// wm-shell §5 namespace (ui.* / keys.* / theme.*).
	UI    UI                `toml:"ui"`
	Keys  Keys              `toml:"keys"`
	Theme map[string]string `toml:"theme"`

	Database Database `toml:"database"`

	// ssh-host §5 namespace.
	SSH       SSH       `toml:"ssh"`
	Poll      Poll      `toml:"poll"`
	Run       Run       `toml:"run"`
	Reconnect Reconnect `toml:"reconnect"`
	Hosts     []Host    `toml:"hosts"`

	Build     Build     `toml:"build"`
	Flash     Flash     `toml:"flash"`
	Console   Console   `toml:"console"`
	OTA       OTA       `toml:"ota"`
	Telemetry Telemetry `toml:"telemetry"`
	Logs      Logs      `toml:"logs"`

	Devices []Device `toml:"devices"`

	// idx holds the O(1) lookup maps built at freeze (lookup.go). It is not a
	// TOML field and is never decoded; a frozen Config is immutable so the maps
	// are safe to read without locking.
	idx *indexes `toml:"-"`
}

// Meta carries config-owned document metadata.
type Meta struct {
	Name string `toml:"name"` // optional human label for this config; never an identifier
}

// Reload controls the fsnotify hot-reload watch (config-owned).
type Reload struct {
	Watch    bool     `toml:"watch"`    // enable fsnotify watch on the loaded file
	Debounce Duration `toml:"debounce"` // coalesce editor write bursts
}

// UI is the wm-shell §5 ui.* namespace. config parses; wm-shell interprets.
type UI struct {
	Mouse            string          `toml:"mouse"`             // tri-state: none|cell|all (View.MouseMode)
	ReportFocus      bool            `toml:"report_focus"`      // View.ReportFocus
	Altscreen        bool            `toml:"altscreen"`         // alternate screen buffer
	TeardownDeadline Duration        `toml:"teardown_deadline"` // max wait for Pane.Close() on quit
	ScrollbackLines  int             `toml:"scrollback_lines"`  // default stream-pane ring size
	TabBar           string          `toml:"tab_bar"`           // top|bottom|off
	StatusBar        bool            `toml:"status_bar"`        // show the status line
	StartupPanes     []pane.PaneSpec `toml:"startup_panes"`     // panes spawned at launch ({kind,args,focus})
	Theme            string          `toml:"theme"`             // named theme; resolves to a [theme] role table
}

// Keys is the wm-shell §5 keys.* namespace. Values are keyspec strings; a value
// with a space is a chord (see keymap.go). Single keys become key.Binding.
type Keys struct {
	Quit      []string            `toml:"quit"`
	Help      []string            `toml:"help"`
	NextTab   []string            `toml:"next_tab"`
	PrevTab   []string            `toml:"prev_tab"`
	FocusN    string              `toml:"focus_n"` // prefix, e.g. "alt+" → alt+1..9
	Spawn     map[string][]string `toml:"spawn"`   // pane-kind → spawn binding(s)
	ClosePane []string            `toml:"close_pane"`
	Split     []string            `toml:"split"`
}

// Database is the READ side of the hybrid backend (direct pgx/v5). dsn is a
// secret + host-bearing field under the redaction contract.
type Database struct {
	DSN              string   `toml:"dsn"`               // secret; pgx DSN; supports $ENV:/$FILE:
	MaxConns         int      `toml:"max_conns"`         // pgxpool max connections
	ConnectTimeout   Duration `toml:"connect_timeout"`   // pool connect/ping timeout
	StatementTimeout Duration `toml:"statement_timeout"` // per-query timeout for read queries
	ReadOnly         bool     `toml:"read_only"`         // a flag the db axis enforces; config only carries it
}

// SSH is the ssh-host §5 [ssh] section (transport).
type SSH struct {
	Binary         string   `toml:"binary"`          // ssh executable
	BaseArgs       []string `toml:"base_args"`       // args before the target on every exec
	ControlPath    string   `toml:"control_path"`    // master socket per host; leading ~ resolved by the tool
	ConnectTimeout Duration `toml:"connect_timeout"` // hard ceiling on a single exec's dial
	CommandTimeout Duration `toml:"command_timeout"` // 0 = no ceiling (flash/console run long)
}

// Poll is the ssh-host §5 [poll] section (device enumeration + VID:PID gate).
type Poll struct {
	Interval        Duration `toml:"interval"`          // per-host enumeration cadence
	Parallel        int      `toml:"parallel"`          // max concurrent host polls
	Command         []string `toml:"command"`           // remote enumeration probe (no device open)
	MatchRegex      string   `toml:"match_regex"`       // gate on the by-id descriptor name
	MACRegex        string   `toml:"mac_regex"`         // best-effort MAC extraction (on-device-verify)
	TTYGlob         string   `toml:"tty_glob"`          // acceptable real-device targets
	VendorID        string   `toml:"vendor_id"`         // ^[0-9a-f]{4}$ — public Espressif VID
	ProductID       string   `toml:"product_id"`        // ^[0-9a-f]{4}$ — public Espressif PID
	ConfirmCommand  []string `toml:"confirm_command"`   // %TTY% substituted; asserts VID:PID before open/flash
	ConfirmRequired bool     `toml:"confirm_required"`  // fail-closed: unconfirmable device never acted on
	ConfirmCacheTTL Duration `toml:"confirm_cache_ttl"` // cache a positive confirm per (host,tty,descriptor)
}

// Run is the ssh-host §5 [run] section (per-run output buffering).
type Run struct {
	ScrollbackLines int `toml:"scrollback_lines"` // per-run ring-buffer size (survives backgrounding)
	LineMaxBytes    int `toml:"line_max_bytes"`   // truncate a pathological line, never OOM
}

// Reconnect is the ssh-host §5 [reconnect] section (host backoff).
type Reconnect struct {
	BackoffInitial Duration `toml:"backoff_initial"` // first retry delay after a host probe fails
	BackoffMax     Duration `toml:"backoff_max"`     // cap on exponential backoff
	BackoffFactor  float64  `toml:"backoff_factor"`  // multiplier per consecutive failure
	FailThreshold  int      `toml:"fail_threshold"`  // consecutive failures before host shown "down"
}

// Host is one ssh-host §5 [[hosts]] entry. name/ssh_target are host-bearing
// fields under the redaction contract.
type Host struct {
	Name      string `toml:"name"`       // display + map key; NOT a real hostname in the example
	SSHTarget string `toml:"ssh_target"` // ssh alias resolved by ~/.ssh/config
	Local     bool   `toml:"local"`      // true → bypass ssh, run via os/exec on this box
	Enabled   *bool  `toml:"enabled"`    // omitted → enabled; set false to skip without deleting the entry
}

// IsEnabled reports whether the host participates in polling/runs. A [[hosts]]
// entry that is written down is active unless explicitly disabled (enabled =
// false); omitting the key keeps it enabled — the zero value never silently
// disables a configured host.
func (h Host) IsEnabled() bool { return h.Enabled == nil || *h.Enabled }

// Build is the [build] section (build axis 04-build.md §5; reconciled with the
// 02-config.md shape). The richer 04-build keys are added alongside the original
// fields; where an existing key already carried the meaning (docker_cmd≡docker_bin,
// target≡idf_target, build_cmd, docker_run_args, setup≡setup_scripts) it is reused
// rather than duplicated. Remote-host wrapping is NOT configured here: the build
// axis consumes the single sshhost Runner (K9), whose ssh binary/flags live in the
// [ssh] section — so build needs no ssh_bin/ssh_flags/ssh_wrap_tmpl of its own.
type Build struct {
	Host               string          `toml:"host"`                 // build-host id: "local" or a [[hosts]].name (K9 Runner)
	RepoPath           string          `toml:"repo_path"`            // open-picpak checkout root (the firmware/ parent)
	FirmwareDir        string          `toml:"firmware_dir"`         // firmware tree, relative to repo_path (or absolute)
	DockerImage        string          `toml:"docker_image"`         // IDF image (≡ 04-build docker_image)
	DockerCmd          string          `toml:"docker_cmd"`           // container runtime (≡ 04-build docker_bin; podman-overridable)
	Target             string          `toml:"target"`               // idf.py set-target (≡ 04-build idf_target); {target} in build_cmd
	BuildCmd           []string        `toml:"build_cmd"`            // inner command run in the IDF container; {target} substituted
	DockerRunArgs      []string        `toml:"docker_run_args"`      // container args; {repo}→abs repo_path, {name}→container_name_tmpl
	ContainerNameTmpl  string          `toml:"container_name_tmpl"`  // container name; {runid} substituted; the docker-kill cancel fallback matches it
	PythonBin          string          `toml:"python_bin"`           // interpreter for the codegen generators + Pillow probe
	PythonImportProbe  string          `toml:"python_import_probe"`  // preflight Pillow check body: {python_bin} -c {probe}
	StatBin            string          `toml:"stat_bin"`             // remote artifact stat binary (BusyBox/coreutils flavor)
	Sha256Bin          string          `toml:"sha256_bin"`           // remote app-hash binary (e.g. shasum -a 256)
	FontsDir           string          `toml:"fonts_dir"`            // bundled-fonts dir, relative to firmware_dir (preflight check)
	Setup              []BuildStep     `toml:"setup"`                // one-time component setup (≡ 04-build setup_scripts); offered by preflight
	Codegen            []BuildStep     `toml:"codegen"`              // host-side codegen run before the container build (the four generators)
	RequiredComponents []string        `toml:"required_components"`  // components/<c> dirs preflight checks (berry/littlefs/qrcodegen)
	ArtifactDir        string          `toml:"artifact_dir"`         // build output dir, relative to repo_path
	Artifacts          []BuildArtifact `toml:"artifacts"`            // binary → flash-offset map (also consumed by flash)
	FlasherArgsPath    string          `toml:"flasher_args_path"`    // IDF offset→file truth, relative to firmware_dir; cross-checked in verify
	AppArtifact        string          `toml:"app_artifact"`         // the app binary to SHA-256, relative to firmware_dir; lowercase hex
	OTASlotSize        int64           `toml:"ota_slot_size"`        // ota_0 size upper bound for the app (partitions.csv)
	ErrorTailLines     int             `toml:"error_tail_lines"`     // lines kept in the failure tail box
	PaneScrollback     int             `toml:"pane_scrollback"`      // ring-buffer line cap for the build pane
	GateDisclaimer     string          `toml:"gate_disclaimer"`      // the mandatory "green = regression gate" banner text
	CancelKillGraceMS  int             `toml:"cancel_kill_grace_ms"` // SIGTERM→SIGKILL grace + docker-kill fallback window
	Keys               BuildKeys       `toml:"keys"`                 // pane-local bindings (cancel/rerun) + the spawn-class start key
}

// BuildKeys is the [build.keys] pane-local keymap. start is informational (the
// actual spawn binding is keys.spawn.build, resolved by wm-shell); cancel/rerun are
// forwarded to the focused build pane. cancel defaults to "x" — NOT a global chrome
// key (ctrl+c/q quit the TUI before a key reaches the pane).
type BuildKeys struct {
	Start  string `toml:"start"`  // informational mirror of keys.spawn.build
	Cancel string `toml:"cancel"` // cancel a running build (pane-scoped)
	Rerun  string `toml:"rerun"`  // rerun in the build pane
}

// BuildStep is one [[build.setup]] / [[build.codegen]] entry. For a codegen step,
// Cmd is [interpreter, script…] with the script path relative to firmware_dir;
// Output is the generated header (relative to firmware_dir) and Src the optional
// Berry source it reads (preflight file_exists check). Setup steps leave Output/Src
// empty.
type BuildStep struct {
	Name   string   `toml:"name"`
	Cmd    []string `toml:"cmd"`
	Cwd    string   `toml:"cwd"`    // relative to repo_path
	Output string   `toml:"output"` // codegen: generated header, relative to firmware_dir
	Src    string   `toml:"src"`    // codegen: Berry source read by the generator (preflight), relative to firmware_dir
}

// BuildArtifact is one [[build.artifacts]] entry. offset is a hex string. min_size
// is a lower byte bound (0 = unset); exact_size pins a fixed-size data partition
// (0 = unset). Both are verify-time sanity bounds, not equalities for the app.
type BuildArtifact struct {
	Name      string `toml:"name"`
	File      string `toml:"file"`       // relative to artifact_dir
	Offset    string `toml:"offset"`     // hex, e.g. "0x20000"
	MinSize   int64  `toml:"min_size"`   // verify: size must be >= this (0 = unset)
	ExactSize int64  `toml:"exact_size"` // verify: size must equal this (0 = unset)
}

// Flash is the [flash] section (flash; restated in 02-config.md). The NVS
// preserve guard and forbid_erase are the load-bearing safety properties.
type Flash struct {
	EsptoolCmd        []string `toml:"esptool_cmd"`         // base invocation (allows python3 -m esptool)
	Chip              string   `toml:"chip"`                // --chip
	Before            string   `toml:"before"`              // --before
	After             string   `toml:"after"`               // --after
	FlashMode         string   `toml:"flash_mode"`          // --flash-mode
	FlashSize         string   `toml:"flash_size"`          // --flash-size
	FlashFreq         string   `toml:"flash_freq"`          // --flash-freq
	ExtraArgs         []string `toml:"extra_args"`          // spliced into write-flash for odd hosts
	RunOverSSH        bool     `toml:"run_over_ssh"`        // run esptool on the device's host vs locally
	NVSPreserveOffset string   `toml:"nvs_preserve_offset"` // protected NVS region (factory RF-cal/MAC/serial)
	ForbidErase       bool     `toml:"forbid_erase"`        // hard guard; cannot be false (load error)
	// flash-specific additions (single [flash] table, flat keys).
	StageMode          string    `toml:"stage_mode"`           // scp|build_on_host|local
	StageRemoteDir     string    `toml:"stage_remote_dir"`     // host staging dir; empty = derive per host
	StageSkipUnchanged bool      `toml:"stage_skip_unchanged"` // skip copy of artifacts whose remote sha matches
	Concurrency        int       `toml:"concurrency"`          // max devices flashed in parallel
	RunTimeout         Duration  `toml:"run_timeout"`          // per-device esptool timeout; 0 defers to ssh
	RetryAuto          bool      `toml:"retry_auto"`           // auto-retry a transient fail class
	RetryMax           int       `toml:"retry_max"`            // max auto-retries when retry_auto
	TailLines          int       `toml:"tail_lines"`           // esptool lines retained per device for the failure tail
	Keys               FlashKeys `toml:"keys"`                 // pane-local bindings (run/retry/abort/open-console)
}

// FlashKeys is the [flash.keys] pane-local keymap (single keystroke specs), a
// distinct K7 scope so reuse with other panes' chords (build.keys.rerun "r",
// build.keys.cancel "x") is legal. run starts a flash on the selected targets; retry
// re-runs a failed device; abort cancels in-flight; console opens axis-06 on a row.
type FlashKeys struct {
	Run     string `toml:"run"`     // start flash on selected targets
	Retry   string `toml:"retry"`   // retry the selected failed device
	Abort   string `toml:"abort"`   // abort selected/all in-flight
	Console string `toml:"console"` // open a console (axis 06) on the selected device
}

// Console is the [console] section (console; behavioral defaults only — no host,
// port, or path literal lives here).
type Console struct {
	SSHCommand           string      `toml:"ssh_command"`            // base ssh program
	SSHArgs              []string    `toml:"ssh_args"`               // args before the remote command; templated {host}
	RemoteCommand        string      `toml:"remote_command"`         // remote pump; templated {port}/{baud}
	Baud                 int         `toml:"baud"`                   // substituted into {baud}
	AutoConnect          bool        `toml:"auto_connect"`           // connect on Init (default off: connect resets device)
	Reconnect            bool        `toml:"reconnect"`              // auto-restart after a transient drop
	ReconnectBackoffMS   int         `toml:"reconnect_backoff_ms"`   // delay before auto-reconnect
	ReconnectMaxAttempts int         `toml:"reconnect_max_attempts"` // cap on consecutive auto-reconnects
	ScrollbackLines      int         `toml:"scrollback_lines"`       // ring capacity per session
	LineEndings          string      `toml:"line_endings"`           // lf|cr|crlf appended to a submitted line
	LocalEcho            bool        `toml:"local_echo"`             // local echo (firmware echoes)
	QuietTimeoutMS       int         `toml:"quiet_timeout_ms"`       // Attached→Quiet silence threshold (tune on-device)
	BannerMatch          string      `toml:"banner_match"`           // line that flips state to Attached
	ConfirmConnect       bool        `toml:"confirm_connect"`        // require confirm before connect (connect reboots)
	SessionLogDir        string      `toml:"session_log_dir"`        // "" = disabled; never an absolute literal in the example
	Keys                 ConsoleKeys `toml:"keys"`                   // pane-local bindings (single keys)
}

// ConsoleKeys is the console.keys.* pane-local keymap (single keystroke specs).
type ConsoleKeys struct {
	Submit      string `toml:"submit"`
	HistoryPrev string `toml:"history_prev"`
	HistoryNext string `toml:"history_next"`
	ScrollUp    string `toml:"scroll_up"`
	ScrollDown  string `toml:"scroll_down"`
	Search      string `toml:"search"`
	Clear       string `toml:"clear"`
	Reconnect   string `toml:"reconnect"`
	Disconnect  string `toml:"disconnect"`
	Help        string `toml:"help"`
}

// OTA is the [ota] section (owned by ota 07-ota.md; config parses, does not
// re-author). admin_api_url is host-bearing; admin_api_token is a secret.
type OTA struct {
	AdminAPIURL       string              `toml:"admin_api_url"`      // host-bearing; base URL of the Admin-API
	AdminAPIToken     string              `toml:"admin_api_token"`    // secret; bearer token; supports $ENV:/$FILE:
	AdminAPITimeout   Duration            `toml:"admin_api_timeout"`  // HTTP client timeout incl. blob upload
	AllowDirectWrite  bool                `toml:"allow_direct_write"` // interim DirectPGXWriter; default-off, gated by rule 3
	BlobDir           string              `toml:"blob_dir"`           // serving location for the direct-write path
	BlobFilename      string              `toml:"blob_filename"`      // served blob filename (derive_fw_url target)
	ServeURLTemplate  string              `toml:"serve_url_template"` // display-only derived serving URL template
	BuildDir          string              `toml:"build_dir"`          // where built binaries land (paths/flags only)
	VersionFile       string              `toml:"version_file"`       // version.txt path (OTA version key)
	FWArtifactName    string              `toml:"fw_artifact_name"`   // the firmware artifact to hash
	DefaultChannel    string              `toml:"default_channel"`    // form preselect; backend channels stay authoritative
	MaxOTATries       int                 `toml:"max_ota_tries"`      // display-only mirror; not a device authority
	ResolvePrecedence []string            `toml:"resolve_precedence"` // precedence order (Policy=Data)
	BehindIsWarning   bool                `toml:"behind_is_warning"`  // treat "behind for ≥ N reports" as a warning
	RefreshInterval   Duration            `toml:"refresh_interval"`   // fleet re-poll cadence
	Keys              map[string][]string `toml:"keys"`               // pane-scoped action → chord(s)
}

// Telemetry is the [telemetry] section (read-model query shaping; telemetry
// 08-telemetry.md owns the canonical defaults).
type Telemetry struct {
	Window                 Duration            `toml:"window"`                   // default per-device history window
	RefreshInterval        Duration            `toml:"refresh_interval"`         // focused re-query ceiling
	LogTailLines           int                 `toml:"log_tail_lines"`           // initial rows pulled into the log viewer
	LogFollowInterval      Duration            `toml:"log_follow_interval"`      // poll ceiling for the log viewer follow mode
	PollIntervalBackground Duration            `toml:"poll_interval_background"` // throttled cadence while hidden
	StaleAfter             Duration            `toml:"stale_after"`              // now-time beyond this → OFFLINE_STALE
	HistoryMaxRows         int                 `toml:"history_max_rows"`         // cap on history rows fetched
	FleetMaxDevices        int                 `toml:"fleet_max_devices"`        // cap on the fleet snapshot result set
	LowBattPct             int                 `toml:"low_batt_pct"`             // batt_pct <= this (not USB) → LOW_BATT
	BadBootsWarn           int                 `toml:"bad_boots_warn"`           // bad_boots >= this → BAD_BOOTS warning
	BrownoutResetReasons   []string            `toml:"brownout_reset_reasons"`   // reset_reason wire strings counting as brownout
	BrownoutOTARRCodes     []int               `toml:"brownout_ota_rr_codes"`    // diag_ota_rr codes meaning brownout-during-OTA
	LowBattIncludesUSB     bool                `toml:"low_batt_includes_usb"`    // raise LOW_BATT even when usb=true
	ChannelMismatchWarn    bool                `toml:"channel_mismatch_warn"`    // flag device-claimed channel ≠ devices.channel
	AgeFormat              string              `toml:"age_format"`               // relative|absolute
	BattUnit               string              `toml:"batt_unit"`                // V|mV
	SparklineMetrics       []string            `toml:"sparkline_metrics"`        // numeric series that get a trend sparkline
	Keys                   map[string][]string `toml:"keys"`                     // pane-scoped action → chord(s) (refresh/select_next/select_prev/copy_serial)
}

// Logs is the [logs] section (logs 09-logs.md owns the canonical defaults).
type Logs struct {
	LineSeparator        string              `toml:"line_separator"`         // char(s) joining lines in payload; split on this
	PollInterval         Duration            `toml:"poll_interval"`          // live-tail poll cadence
	PollBatch            int                 `toml:"poll_batch"`             // max rows per forward poll query
	BackfillPage         int                 `toml:"backfill_page"`          // rows per scrollback page query
	LiveRingLines        int                 `toml:"live_ring_lines"`        // bounded in-store parsed-line ring
	HistoryMaxRows       int                 `toml:"history_max_rows"`       // cap on total backfilled rows held
	DefaultFilterSerials []string            `toml:"default_filter_serials"` // pre-selected serials on open ([] = all)
	DefaultSources       []string            `toml:"default_sources"`        // source filter default
	ShowGapMarkers       bool                `toml:"show_gap_markers"`       // inject boot-epoch/gap divider lines
	GapMarkerFormat      string              `toml:"gap_marker_format"`      // template for the gap divider
	ShowSuspectMarkers   bool                `toml:"show_suspect_markers"`   // inject suspect markers
	SuspectMarker        string              `toml:"suspect_marker"`         // text/glyph for the suspect marker
	FollowDefault        bool                `toml:"follow_default"`         // start in auto-scroll-to-newest mode
	ErrorBackoff         Duration            `toml:"error_backoff"`          // backoff after a failed poll before retry
	ErrorBackoffMax      Duration            `toml:"error_backoff_max"`      // cap for exponential backoff on repeated errors
	TimestampFormat      string              `toml:"timestamp_format"`       // Go time layout for the per-line time column
	Timezone             string              `toml:"timezone"`               // render zone
	ColorizeBy           string              `toml:"colorize_by"`            // serial|source|none
	Palette              []string            `toml:"palette"`                // color cycle for the colorize key
	QueryTimeout         Duration            `toml:"query_timeout"`          // per-query context timeout
	Keys                 map[string][]string `toml:"keys"`                   // pane-scoped action → chord(s) (follow/device_filter/source_cycle/clear_view/copy/jump_serial/reload)
}

// Device is one [[devices]] entry (label/cross-ref; identity truth is NVS).
// mac is a private identifier under the redaction contract.
type Device struct {
	Serial  string `toml:"serial"`  // required, unique; D8 public identifier
	MAC     string `toml:"mac"`     // cross-ref MAC; host-bearing-adjacent; example uses 00:00:00:00:00:00
	Channel string `toml:"channel"` // display label only; backend channels table is authoritative
	Label   string `toml:"label"`   // human location/name
	Host    string `toml:"host"`    // hosts[].name this device is normally tethered to
}

// Duration is a TOML-decodable time.Duration (Go duration strings like "5s").
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the duration in Go duration syntax.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalText decodes a Go duration string (BurntSushi/toml uses this for
// string-typed values). An empty string decodes to a zero duration.
func (d *Duration) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalText renders the duration so a round-trip through TOML is stable.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}
