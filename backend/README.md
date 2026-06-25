# open-picpak backend

Self-hostable telemetry / OTA / log backend for the open-picpak fleet. Go service + PostgreSQL
(TimescaleDB) + Grafana, run via Docker Compose. Replaces the legacy Python telemetry sink and the
ad-hoc plotting scripts.

> **Status:** waves **S5a + S5b1** - TimescaleDB + Grafana scaffold, schema migration, and legacy
> telemetry ingest. HOTP validation, OTA-signal piggyback, firmware.bin serving, and log reassembly
> land in later waves.

## Quick start (local / dev)

```sh
cp .env.example .env            # then edit: set POSTGRES_PASSWORD + GRAFANA_ADMIN_PASSWORD
docker compose up -d            # timescaledb -> migrate -> ingest + grafana
docker compose ps              # migrate should be "exited (0)"; others "running/healthy"
```

Grafana: http://127.0.0.1:3000 (admin / $GRAFANA_ADMIN_PASSWORD), TimescaleDB datasource pre-provisioned.
Ingest: `GET http://127.0.0.1:8080/$INGEST_TOKEN/pp?...` for the legacy migration path.

## Schema migration

`migrations/0001_init.up.sql` is the v1 schema: control tables `firmware_versions`,
`channels`, `devices`, `device_auth`, `device_log_fragment`, `device_log_cursor`, `rollout_targets` +
hypertables `telemetry`, `logs` (+ Hypercore columnstore & retention policies). It is **idempotent**
(`IF NOT EXISTS` / `if_not_exists`). The compose `migrate` one-shot applies it via
`psql -v ON_ERROR_STOP=1`.

Re-apply / verify:
```sh
docker compose run --rm migrate                                   # re-run (idempotent)
docker compose exec timescaledb psql -U picpak -d picpak -c '\dt' # list tables
```

Later waves wire the Go ingest/admin code to these base tables and may add non-breaking indexes or
views as the wire contract is finalized.

## Air-gap

This directory is part of the public open-picpak repo. **No real secrets/identifiers in committed
files** - `.env` (gitignored) holds local values; production secrets live in the deployment's secret
store. Compose ports bind to `127.0.0.1` for dev; production exposes only the ingest behind a
TLS-terminating reverse-proxy, DB/Grafana on the internal network.
