// Command picpak-ops is the operator cockpit TUI: a single Bubble Tea program
// (the wm-shell root) that hosts feature panes (build, hosts, console, flash,
// telemetry, logs, ota). This file is the bootstrap only: it loads config, builds
// the program, injects the Sender after construction (the §4.5 pointer invariant),
// registers the pane factories, runs, and propagates the exit code.
//
// Air-gap (§6.1): no transport endpoint, DSN, host, /dev path, offset, or
// credential has an in-code fallback here. Absent transport config is a hard error
// at startup, surfaced by the config loader — never a silent baked value. The only
// defaults the tool ships are the cosmetic UI/keymap/theme constants in the config
// defaults table.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/buildpane"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/console"
	"github.com/open-picpak/picpak-ops/internal/db"
	"github.com/open-picpak/picpak-ops/internal/flashpane"
	"github.com/open-picpak/picpak-ops/internal/fleet"
	"github.com/open-picpak/picpak-ops/internal/logs"
	"github.com/open-picpak/picpak-ops/internal/ota"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
	"github.com/open-picpak/picpak-ops/internal/telemetry"
)

func main() {
	os.Exit(run())
}

// run is the testable entry point: it returns the process exit code instead of
// calling os.Exit, so a wiring test can drive it without killing the test binary.
func run() int {
	var (
		configPath  = flag.String("config", "", "path to the picpak-ops config file")
		requireFile = flag.Bool("require-config", false, "fail if no config file is found")
	)
	flag.Parse()

	opts := config.Opts{
		Path:        *configPath,
		RequireFile: *requireFile,
	}

	p, cancel, err := build(context.Background(), opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cancel()

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// build performs the full bootstrap wiring (the §4.5 ordering) and returns a ready
// tea.Program plus the root cancel func. It is the single place Load→New→register→
// NewProgram→SetSender happens, so a test can prove the wiring without running the
// interactive loop. parent is the application's parent context (cancelled-on-quit
// child is derived here).
func build(parent context.Context, opts config.Opts) (*tea.Program, context.CancelFunc, error) {
	loaded, err := config.LoadWithPath(opts)
	if err != nil {
		return nil, nil, err
	}

	// rootCtx is cancelled on quit; every per-pane context is a child of it, and
	// tea.WithContext ties the program lifetime to it (R2 resource-leak guard).
	rootCtx, baseCancel := context.WithCancel(parent)

	m := app.New(loaded.Config, rootCtx, baseCancel) // *app.Model — POINTER (§4.5)
	m.SetReload(loaded.Path, opts)

	p := tea.NewProgram(m, tea.WithContext(rootCtx))
	// Inject the Sender AFTER NewProgram captured m: SetSender mutates the SAME
	// instance the program holds (pointer invariant). Done before Run, so panes
	// spawned in Init (inside Run) see a live Sender.
	send := func(msg tea.Msg) { p.Send(msg) }
	m.SetSender(send)

	// Build the ONE shared ssh-host transport at startup (the §4.5 / W7 promotion):
	// constructed once here — after SetSender, so its workers stream through the live
	// Sender — and shared by BOTH the hosts inventory pane and the flash pane (K9), so
	// flash always has a transport (Snapshot/Confirm/Run) even when no hosts pane is
	// open. The registry is built with an empty inventory target; the first hosts pane
	// adopts the live poll-push by re-targeting it to its own id (SetInventoryPane).
	// Started here so the flash target picker is populated without a hosts pane.
	reg := sshhost.NewHostRegistry(loaded.Config, send, "")
	reg.Start()

	// Build the ONE shared read pool + fleet cache here (this is the first DB-backed
	// wave). Telemetry is the first consumer (logs/ota follow). The pool is OPTIONAL: an
	// empty DSN or an unreachable DB does NOT abort startup — the bench cockpit
	// (build/hosts/flash/console) runs without a DB and the telemetry pane renders a
	// "no database configured" state from the resulting nil pool. The DSN/host never
	// leaks: db.NewPool sanitizes its errors (redaction contract).
	var (
		pool       *pgxpool.Pool
		fleetCache *fleet.Cache
	)
	if loaded.Config.Database.DSN != "" {
		if pl, perr := db.NewPool(rootCtx, loaded.Config); perr == nil {
			pool = pl
			fleetCache = fleet.New(pool)
			loadCtx, loadCancel := context.WithTimeout(rootCtx, loaded.Config.Database.ConnectTimeout.D())
			_ = fleetCache.Load(loadCtx) // best-effort; a load error leaves an empty cache, not a crash
			loadCancel()
		}
		// pool error (DSN set but DB unreachable) → run DB-less; pool stays nil.
	}

	// The OTA pane consumes the telemetry latest-row read for its target-vs-running
	// view (LatestFleet), so it gets a telemetry Repo over the SAME shared pool rather
	// than duplicating the latest-row query. Nil when there is no pool (read features
	// disabled). The pool also backs the interim DirectPGXWriter when
	// ota.allow_direct_write is set (config rule 3 guarantees it is then writable).
	var otaRepo *telemetry.Repo
	if pool != nil {
		otaRepo = telemetry.NewRepo(pool, loaded.Config.Database.StatementTimeout.D())
	}

	registerFactories(m, reg, pool, fleetCache, otaRepo)

	// Augment cancel so app teardown also stops the shared ssh-host transport (workers
	// + ssh -O exit per host) and closes the shared read pool (single teardown point).
	cancel := func() {
		_ = reg.Stop()
		if pool != nil {
			pool.Close()
		}
		baseCancel()
	}

	return p, cancel, nil
}

// registerFactories binds every pane-kind constructor. W3 shipped only the launcher;
// W4 adds the hosts inventory pane (axis 03 ssh-host). Each later wave
// (build/console/flash/telemetry/logs/ota) adds its line here. The shared HostRegistry
// is handed to both the hosts pane factory (SharedFactory: it adopts the poll-push on
// spawn) and the flash pane factory (Snapshot/Confirm/Run) so both ride the ONE
// transport (K9).
func registerFactories(m *app.Model, reg *sshhost.HostRegistry, pool *pgxpool.Pool, fleetCache *fleet.Cache, otaRepo *telemetry.Repo) {
	m.RegisterFactory(pane.KindLauncher, app.NewLauncher)

	// W4 hosts inventory pane bound to the app-level shared registry (no longer the
	// lazy per-pane build; the registry is owned by build() now).
	m.RegisterFactory(pane.KindHosts, sshhost.SharedFactory(reg))

	// W5 build pane (axis 04): consumes the single sshhost Runner (K9) for its
	// build host; opens no transport of its own.
	m.RegisterFactory(pane.KindBuild, buildpane.New)

	// W6 console pane (axis 06): multi-instance device console. Consumes the single
	// sshhost transport via an interactive run (K9); honors ReleaseDevicePortMsg (K6);
	// connect = reset, so it opens nothing on spawn/layout-restore.
	m.RegisterFactory(pane.KindConsole, console.New)

	// W7 flash pane (axis 05): the esptool rollout. Shares the SAME registry as hosts
	// for the VID-gated picker (Snapshot), the Confirm gate, and the esptool Run (K9);
	// emits ReleaseDevicePortMsg before each device's write (K6).
	m.RegisterFactory(pane.KindFlash, flashpane.New(reg))

	// W9 telemetry pane (axis 08): the first DB-backed pane. Consumes the shared
	// (possibly nil) read pool + fleet cache (K9); read-only. A nil pool renders the
	// "no database configured" state — it never crashes the bench cockpit.
	m.RegisterFactory(pane.KindTelemetry, telemetry.New(pool, fleetCache))

	// W10 logs pane (axis 09): the background log viewer. Consumes the SAME shared
	// (possibly nil) read pool + fleet cache (K9); read-only. Its store goroutine
	// keeps tailing while the pane is hidden (K2/K3). A nil pool ⇒ "no database
	// configured" — it never crashes the bench cockpit.
	m.RegisterFactory(pane.KindLogs, logs.New(pool, fleetCache))

	// W11 ota pane (axis 07): OTA management — read Store + Resolve precedence over the
	// shared (possibly nil) read pool; the per-device target-vs-running view consumes the
	// telemetry Repo's LatestFleet (no duplicated latest-row query) + the fleet cache.
	// The Writer backend is chosen by ota.allow_direct_write (AdminAPIWriter default; the
	// interim DirectPGXWriter rides the same — then writable — pool). A nil pool ⇒ "no
	// database configured" — it never crashes the bench cockpit.
	m.RegisterFactory(pane.KindOTA, ota.New(pool, otaRepo, fleetCache))
}
