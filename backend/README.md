# open-picpak backend

Self-hostable telemetry / OTA / log backend for the open-picpak fleet. Go service + PostgreSQL
(TimescaleDB) + Grafana, run via Docker Compose. Replaces the legacy Python telemetry sink and the
ad-hoc plotting scripts.

> **Status:** TimescaleDB + Grafana scaffold, schema migration, legacy telemetry ingest, and the
> **C2 command channel** (per-device Berry command queue + per-device HOTP auth), plus a
> bearer-authenticated **operator control plane** (`cmd/admin`: admin API, encrypted secrets KV, and an
> embedded Svelte operator web UI). OTA-signal piggyback, firmware.bin serving, and log reassembly land
> in later waves.

## Quick start (local / dev)

```sh
cp .env.example .env            # then edit: set POSTGRES_PASSWORD + GRAFANA_ADMIN_PASSWORD
docker compose up -d            # timescaledb -> migrate -> ingest + grafana
docker compose ps              # migrate should be "exited (0)"; others "running/healthy"
```

Grafana: http://127.0.0.1:3000 (admin / $GRAFANA_ADMIN_PASSWORD), TimescaleDB datasource pre-provisioned.
Ingest: `GET http://127.0.0.1:8080/$INGEST_TOKEN/pp?...` for the legacy migration path.
The ingest/C2 host bind address defaults to `127.0.0.1`; set `INGEST_BIND` in `.env` to expose it on a
specific IP (kept out of the repo). Production should front it with a TLS-terminating reverse proxy.

## C2 command channel

The device polls a command-and-control endpoint and executes the returned Berry script (the command
surface = the device's console actions, expressed in Berry). Endpoint:

```
GET  /<INGEST_TOKEN>/c2/challenge?sn=<serial>   -> 200 {nonce}     (re-key step 1)
POST /<INGEST_TOKEN>/c2/rekey?sn=<serial>       -> 204             (re-key step 2: ECDSA-signed)
GET  /<INGEST_TOKEN>/c2?sn=<serial>&ack=<applied_seq>&c=<counter>&otp=<hotp>[&wait=<s>]
  -> 200 + body = the next pending Berry script, header X-C2-Seq: <seq>   (work to do)
  -> 204                                                              (in sync / wait budget elapsed)
  -> 401 (any auth failure, constant, no detail)   -> 404 (bad token / unknown device, noise)
```

- **Ack-cursor change feed.** `command_queue` is append-only with a global monotonic `seq`; each device
  has a `device_c2_cursor` (`applied_seq`). The serve picks the lowest `seq > applied_seq` whose
  `serial ∈ {sn, '*'}` (`'*'` = whole fleet). `ack` advances the cursor (`GREATEST`, forward-only), so
  a replayed ack can never roll it back. The cursor is the only per-device state — a fleet command is
  served to every device independently.
- **Session auth (ECDSA bond + re-key).** The device holds a long-term ECDSA P-256 keypair; the backend
  stores only the public key. A signed challenge/re-key handshake installs a per-session HOTP secret;
  polls then carry RFC-4226 HMAC-SHA1 HOTP over a **flat** session counter (no boot_count composite),
  validated under `device_auth FOR UPDATE` (fail-closed, monotonic, replay-safe). Auth runs **before**
  the cursor is touched, so a forged request cannot advance a device's cursor. C2 ships remote code
  execution: serve over HTTPS only (the device refuses a non-https C2 URL).
- **Long-poll (opt-in `?wait=<s>`, cap 25 s).** When in sync, the handler holds the request until a
  command for the device (or `'*'`) lands — a `command_queue` `AFTER INSERT` trigger `pg_notify`s one
  process-wide `LISTEN` connection that fans out in-process to every waiter — or the budget elapses
  (→ 204). Absent/`wait<=0` returns 204 immediately (the battery field poll, no connection hold). One
  DB connection signals the whole fleet.
- **Enqueue (operator):** `POST /api/command` (or per device, `POST /api/devices/{serial}/command`) on
  the operator control plane below; the enqueue is attributed to the operator key. A direct SQL insert
  into `command_queue (serial, script[, note])` still works for break-glass.

## Operator control plane (`cmd/admin`)

A second binary, built from `Dockerfile.admin` and run as its own compose service — a **process and
address space separate from the public `ingest` parser**, so the privileged write path never sits in the
binary the internet reaches. Bound to host loopback / VPN only (TLS terminates at a tunnel or proxy);
never published.

- **Bearer auth.** Operator keys live in `operator_keys` (sha256 of the token; the plaintext is shown
  once at mint and never stored). A soft-revoke (`disabled_at`) authenticates as if absent. Routes split
  `auth` (any valid key — reads) vs `requireAdmin` (mutations); `GET /api/whoami` reports
  `{key_id, is_admin, label}`. Keys are minted/listed/revoked out-of-band:
  `admin create-operator -label <name> [-admin]`.
- **Encrypted secrets KV.** `PUT/GET/DELETE /api/secrets/{name}` over an AES-256-GCM sealed store; the
  value is never echoed back. The master key (`SECRETS_KEY`, 32 bytes hex) is injected only into the
  admin container, lives in a root-only file outside `.env`, and the service **fails closed** without it.
  Break-glass decrypt is an out-of-band subcommand, not an HTTP route. Back up the key with the host —
  key loss is total loss.
- **Live events.** `GET /api/events` is a server-sent-events stream (device-roster snapshot + deltas
  now; telemetry/log payloads in later waves) with a periodic re-auth that tears the stream down within
  ~60 s of a key revocation.
- **Operator web UI.** A Svelte 5 SPA embedded via `//go:embed` (one binary, no second container, no
  CORS): key login, a live device roster, and read-only degradation for non-admin keys. A checkout
  without the bun toolchain still builds — the Go binary serves a 503 hint while every `/api` route stays
  functional. Feature pages (OTA, logs, telemetry, Berry, FaaS, Web-USB onboarding) mount into the shell
  in later waves. Source + build: `backend/web/` (`bun install && bun run build`).

## Schema migration

`migrations/0001..0006` are the schema: v1 control tables (`firmware_versions`, `channels`, `devices`,
`device_auth`, `device_log_fragment`, `device_log_cursor`, `rollout_targets`) + hypertables
(`telemetry`, `logs`, with Hypercore columnstore & retention); v2 HOTP-digits default; v3 the C2
`command_queue` + `device_c2_cursor`; v4 the ECDSA session-auth columns + `c2_nonce`; v5 the cutover
(drops the legacy HOTP/boot_count columns); v6 the long-poll `NOTIFY` trigger. All are **idempotent**
(`IF NOT EXISTS` / `if_not_exists`, and each must stay re-runnable against the FINAL schema since the
one-shot re-applies all in order); the compose `migrate` applies them via `psql -v ON_ERROR_STOP=1`.

Re-apply / verify:
```sh
docker compose run --rm migrate                                   # re-run (idempotent)
docker compose exec timescaledb psql -U picpak -d picpak -c '\dt' # list tables
```

Later waves wire the remaining Go ingest/admin code to these base tables and may add non-breaking
indexes or views as the wire contract is finalized.

## Air-gap

This directory is part of the public open-picpak repo. **No real secrets/identifiers in committed
files** - `.env` (gitignored) holds local values; production secrets live in the deployment's secret
store. Compose ports bind to `127.0.0.1` for dev; production exposes only the ingest behind a
TLS-terminating reverse-proxy, DB/Grafana on the internal network.
