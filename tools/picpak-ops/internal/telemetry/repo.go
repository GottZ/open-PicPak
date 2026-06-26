package telemetry

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo is the read-only telemetry repository over the shared *pgxpool.Pool. It never
// opens its own pool (internal/db owns the single, read_only-enforced handle) and
// never writes. Each query derives its context from the caller's per-pane context and
// bounds it with statementTimeout — the same database.statement_timeout the shared
// pool already enforces server-side — so an in-flight SELECT is cancelled on
// Close()/app-quit (per-pane ctx cancel), not merely at the statement timeout.
type Repo struct {
	pool             *pgxpool.Pool
	statementTimeout time.Duration
}

// NewRepo binds a Repo to the shared pool and the configured statement timeout
// (database.statement_timeout). The caller passes the pool it leased from internal/db;
// the Repo does not own its lifecycle.
func NewRepo(pool *pgxpool.Pool, statementTimeout time.Duration) *Repo {
	return &Repo{pool: pool, statementTimeout: statementTimeout}
}

// queryCtx derives a child context bounded by statementTimeout from the caller's ctx
// (the per-pane ctx). A non-positive timeout means "rely on the server-side
// statement_timeout only" — still cancellable via the parent ctx.
func (r *Repo) queryCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.statementTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, r.statementTimeout)
}

// LatestFleet returns one latest row per device (read-shape 1), capped at maxDevices.
// maxDevices <= 0 is treated as no cap is meaningful — the caller always passes
// fleet_max_devices, but a defensive floor of 1 keeps the LIMIT valid.
func (r *Repo) LatestFleet(ctx context.Context, maxDevices int) ([]FleetRow, error) {
	if maxDevices < 1 {
		maxDevices = 1
	}
	qctx, cancel := r.queryCtx(ctx)
	defer cancel()

	rows, err := r.pool.Query(qctx, fleetSQL, maxDevices)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FleetRow
	for rows.Next() {
		var (
			serial                        string
			t                             time.Time
			battMV, badBoots              *int32
			battPct, diagOTARR            *int16
			bootCount, uptime             *int64
			resetReason, running, channel *string
			usb                           *bool
		)
		if err := rows.Scan(
			&serial, &t, &battMV, &battPct, &badBoots, &bootCount, &resetReason,
			&usb, &uptime, &running, &channel, &diagOTARR,
		); err != nil {
			return nil, err
		}
		out = append(out, FleetRow{
			Serial:        serial,
			Time:          t,
			BattMV:        nullI32(battMV, true),
			BattPct:       nullI16(battPct, true),
			BadBoots:      nullI32(badBoots, false),
			BootCount:     nullI64(bootCount, false),
			ResetReason:   nullStr(resetReason),
			USB:           nullBool(usb),
			Uptime:        nullI64(uptime, true),
			RunningVer:    derefStr(running),
			DeviceChannel: derefStr(channel),
			DiagOTARR:     nullI16(diagOTARR, false),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LatestDevice returns the newest full row for a serial (read-shape 2a), or (nil, nil)
// when the device has no telemetry yet (sparse — the normal case).
func (r *Repo) LatestDevice(ctx context.Context, serial string) (*DeviceTelemetry, error) {
	qctx, cancel := r.queryCtx(ctx)
	defer cancel()

	row := r.pool.QueryRow(qctx, latestDeviceSQL, serial)

	var (
		sn                                    string
		t                                     time.Time
		battMV, badBoots                      *int32
		battPct, diagRunstate, diagO0, diagO1 *int16
		diagRR, diagOTARR, diagMVErr          *int16
		bootCount, uptime, diagBoots, diagMV  *int64
		resetReason, running, channel         *string
		diagRunPart, diagInv, diagStage       *string
		usb                                   *bool
		extra                                 []byte
	)
	err := row.Scan(
		&sn, &t, &battMV, &battPct, &badBoots, &bootCount, &resetReason,
		&usb, &uptime, &running, &channel,
		&diagRunPart, &diagRunstate, &diagO0, &diagO1, &diagInv, &diagBoots,
		&diagRR, &diagOTARR, &diagMV, &diagMVErr, &diagStage, &extra,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	return &DeviceTelemetry{
		Serial:        sn,
		Time:          t,
		BattMV:        nullI32(battMV, true),
		BattPct:       nullI16(battPct, true),
		BadBoots:      nullI32(badBoots, false),
		BootCount:     nullI64(bootCount, false),
		ResetReason:   nullStr(resetReason),
		USB:           nullBool(usb),
		Uptime:        nullI64(uptime, true),
		RunningVer:    derefStr(running),
		DeviceChannel: derefStr(channel),
		DiagRunPart:   nullStr(diagRunPart),
		DiagRunstate:  nullI16(diagRunstate, false), // SMALLINT, NULL-only sentinel
		DiagO0:        nullI16(diagO0, false),       // SMALLINT, NULL-only sentinel
		DiagO1:        nullI16(diagO1, false),       // SMALLINT, NULL-only sentinel
		DiagInv:       nullStr(diagInv),
		DiagBoots:     nullI64(diagBoots, false),
		DiagRR:        nullI16(diagRR, false),
		DiagOTARR:     nullI16(diagOTARR, false),
		DiagMV:        nullI64(diagMV, false),
		DiagMVErr:     nullI16(diagMVErr, false),
		DiagStage:     nullStr(diagStage),
		Extra:         DecodeExtra(extra),
	}, nil
}

// DeviceHistory returns the recent history window for a serial (read-shape 2b),
// newest-first, bounded by window (now-window..now) and maxRows.
func (r *Repo) DeviceHistory(ctx context.Context, serial string, window time.Duration, maxRows int) ([]HistoryPoint, error) {
	if maxRows < 1 {
		maxRows = 1
	}
	since := time.Now().Add(-window)
	if window <= 0 {
		// No window configured → fall back to the row cap alone (epoch start).
		since = time.Unix(0, 0)
	}

	qctx, cancel := r.queryCtx(ctx)
	defer cancel()

	rows, err := r.pool.Query(qctx, deviceHistorySQL, serial, since, maxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HistoryPoint
	for rows.Next() {
		var (
			t                 time.Time
			battMV, badBoots  *int32
			battPct           *int16
			bootCount, uptime *int64
			resetReason       *string
		)
		if err := rows.Scan(&t, &battMV, &battPct, &badBoots, &bootCount, &resetReason, &uptime); err != nil {
			return nil, err
		}
		out = append(out, HistoryPoint{
			Time:        t,
			BattMV:      nullI32(battMV, true),
			BattPct:     nullI16(battPct, true),
			BadBoots:    nullI32(badBoots, false),
			BootCount:   nullI64(bootCount, false),
			ResetReason: nullStr(resetReason),
			Uptime:      nullI64(uptime, true),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
