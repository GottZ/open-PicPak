# picpak-ops

> A terminal cockpit for operating an open-picpak fleet — build, flash, talk to and
> watch your devices from one window-managed TUI, no cloud.

`picpak-ops` is a [Bubble Tea](https://github.com/charmbracelet/bubbletea) TUI that pulls the
deployment plumbing into the background so you just interact with PicPaks. It builds the firmware,
rolls it out over SSH+esptool, finds which device is on which host, opens a device console, and reads
the fleet's telemetry and logs — each as a pane you can leave running in the background.

> **Status:** full feature surface, built and unit-/integration-tested headless and against a live
> backend DB. **Not yet validated on real hardware or in a live terminal session** — the on-device
> and first-TTY gates are open (see [Validation gaps](#validation-gaps)). Treat it as a working draft,
> not a finished release.

## Features

Each feature is a pane in a tmux-like shell; the log viewer and device console keep running while you
work elsewhere.

| Pane | What it does |
|---|---|
| **hosts** | Poll the configured hosts and show which PicPaks are attached where (USB VID:PID-gated, never touches an unrelated dongle). |
| **build** | Run the firmware codegen + ESP-IDF docker build, stream the output, verify the artifacts (offsets + lowercase SHA-256). |
| **flash** | Roll a build onto selected devices via esptool over SSH — per-device progress, factory-NVS write guard (refuses any erase / `0x9000` overlap). |
| **console** | Interactive device console (the firmware's USB-serial protocol) tunnelled over SSH; honest about the link being intermittent. |
| **telemetry** | Per-device battery / boots / uptime / health, read from the backend TimescaleDB. Last-seen age, never a fake "live". |
| **logs** | Background log viewer tailing the device log ring from the backend. |
| **ota** | Manage what the server offers (firmware versions, channels, per-device rollout) — devices pull. |

## Build & run

Requires Go 1.26+.

```sh
cd tools/picpak-ops
go build ./...                      # or: go build -o picpak-ops ./cmd/picpak-ops
go run ./cmd/picpak-ops --config ./picpak-ops.toml
```

Config resolution order (first hit wins): `--config <path>` → `$PICPAK_OPS_CONFIG` →
`$XDG_CONFIG_HOME/picpak-ops/config.toml`. Copy [`config.example.toml`](config.example.toml) to your
own file and fill in your hosts/devices/database — see [Configuration](#configuration).

The backend (telemetry/logs/OTA) is **optional**: with no `database.dsn` configured, the DB panes show
"no database configured" and the build/flash/console/hosts cockpit works without any backend.

## Configuration

Everything is configuration — there are no hardcoded hosts, devices, ports, offsets or keybindings
(Policy = Data). The file is TOML; see [`config.example.toml`](config.example.toml) for the full,
commented schema. Highlights:

- `[[hosts]]` — ssh targets (resolved through your `~/.ssh/config`; the tool shells out to system
  `ssh`, so your agent/keys/`ProxyJump` all just work). A `local = true` host bypasses ssh.
- `[[devices]]` — known PicPaks (serial/label/channel), display metadata only.
- `[poll]` — the USB enumeration probe + the strict `vendor_id`/`product_id` gate (`303a`/`1001`).
- `[build]` / `[flash]` — codegen + docker image + esptool flags + the artifact offset map. The
  factory-NVS region is protected and `forbid_erase` cannot be disabled.
- `[database]` — read-only pgx DSN to the backend TimescaleDB (telemetry/logs/OTA read path).
- `[ota]` — Admin-API URL/token (write path) and the interim direct-write flag (default off).

Secrets (DB password, admin token) support `$ENV:NAME` / `$FILE:/path` indirection and env overrides
(`PICPAK_OPS_DB_DSN`, `PICPAK_OPS_ADMIN_URL`, `PICPAK_OPS_ADMIN_TOKEN`), so your real config never has
to hold them in plain text.

**Air-gap:** keep your real config out of git — `picpak-ops.toml`, `config.toml` and `*.local.toml`
are gitignored. The committed `config.example.toml` carries only placeholders.

## Architecture

- **One Bubble Tea program, many panes.** A single root model routes addressed messages to the one
  pane that owns them, so a backgrounded pane (log tail, console) keeps receiving data without blocking
  the UI loop.
- **One transport.** All host interaction (poll, build, flash, console) goes through a single system-`ssh`
  runner with OpenSSH connection multiplexing; nothing in the tool reimplements ssh, scp or key handling.
- **Read-only DB by default.** The telemetry/logs/OTA read path runs on a pgx pool with the session
  forced read-only; the only writer is the (default-off, flagged) interim OTA path.

```
cmd/picpak-ops      entry point: load config → program → register pane factories
internal/pane       the pane contract (interface, base, scrollback)
internal/app        window-management runtime (router, layout, keymap, teardown)
internal/config     TOML schema, loader, validation, redaction
internal/sshhost    the one ssh/local transport + host inventory
internal/build      firmware build pipeline        internal/flash    esptool rollout
internal/console    device console                 internal/db       read-only pgx pool
internal/telemetry  per-device telemetry           internal/fleet    device cache
internal/logs       background log viewer          internal/ota      OTA management
```

## Validation gaps

Honest about what "built and tested" does and does not mean here:

- **First real terminal run** has not happened — the panes are exercised headless, not yet in a live TTY.
- **On-device gates are open** (marked `// TODO(on-device)` in the code): the USB by-id descriptor
  format, the console reset/quiet timing, a real flash on a device, and a from-scratch docker build.

These are the next steps before calling any of this production-ready.

## License

Part of [open-picpak](../../README.md). Mozilla Public License 2.0.
