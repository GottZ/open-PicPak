package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo is the read-only telemetry repository over the shared *pgxpool.Pool (owned by cmd/admin, never
// opened here). It never writes (D22.1). Per-query timeouts ride the caller's context (the shared pool's
// DSN policy, not a second divergent one — design 22 §5); the SSE producer (W3) bounds its own ctx.
type Repo struct {
	pool *pgxpool.Pool
}

// NewRepo binds a Repo to the shared pool. The Repo does not own the pool lifecycle.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// LatestFleet returns one enriched row per REGISTERED device (D22.3 — driven by `devices`, so a silent
// device survives with HasData=false → NO_DATA), capped at maxDevices (fleet_max_devices). The lateral
// join attaches each device's newest telemetry row, or none.
func (r *Repo) LatestFleet(ctx context.Context, maxDevices int) ([]FleetRow, error) {
	return r.fleetRows(ctx, nil, maxDevices)
}

// FleetForSerials enriches a specific set of serials with the SAME lateral query (D22.7 — the SSE
// producer reuses one verdict authority for the changed-serial deltas). An empty/nil set yields no rows.
func (r *Repo) FleetForSerials(ctx context.Context, serials []string, maxDevices int) ([]FleetRow, error) {
	if len(serials) == 0 {
		return []FleetRow{}, nil
	}
	return r.fleetRows(ctx, serials, maxDevices)
}

func (r *Repo) fleetRows(ctx context.Context, serials []string, maxDevices int) ([]FleetRow, error) {
	if maxDevices < 1 {
		maxDevices = 1
	}
	// A nil slice encodes as SQL NULL, and cardinality(NULL)=NULL makes the WHERE NULL → drops EVERY row.
	// An empty NON-nil slice encodes as '{}' so cardinality('{}')=0 → the all-devices path (logquery K-pattern).
	sl := serials
	if sl == nil {
		sl = []string{}
	}

	rows, err := r.pool.Query(ctx, fleetSQL, sl, maxDevices)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []FleetRow{}
	for rows.Next() {
		var (
			serial                                string
			label                                 *string
			assignedChannel                       string
			regLastSeen                           *time.Time
			bonded                                bool
			c2LastSeen                            *time.Time
			tTime                                 *time.Time // nullable: LEFT JOIN LATERAL miss → NULL
			battMV, badBoots                      *int32
			battPct, diagOTARR                    *int16
			bootCount, uptime                     *int64
			resetReason, running, reportedChannel *string
			usb                                   *bool
		)
		if err := rows.Scan(
			&serial, &label, &assignedChannel, &regLastSeen, &bonded, &c2LastSeen,
			&tTime, &battMV, &battPct, &badBoots, &bootCount, &resetReason, &usb,
			&uptime, &running, &reportedChannel, &diagOTARR,
		); err != nil {
			return nil, err
		}
		fr := FleetRow{
			Serial:          serial,
			Label:           label,
			AssignedChannel: assignedChannel,
			RegLastSeen:     regLastSeen,
			Bonded:          bonded,
			C2LastSeen:      c2LastSeen,
		}
		if tTime != nil { // a telemetry row exists for this device
			fr.HasData = true
			fr.Time = *tTime
			fr.BattMV = nullI32(battMV, true)
			fr.BattPct = nullI16(battPct, true)
			fr.BadBoots = nullI32(badBoots, false)
			fr.BootCount = nullI64(bootCount, false)
			fr.ResetReason = nullStr(resetReason)
			fr.USB = nullBool(usb)
			fr.Uptime = nullI64(uptime, true)
			fr.RunningVer = derefStr(running)
			fr.ReportedChannel = derefStr(reportedChannel)
			fr.DiagOTARR = nullI16(diagOTARR, false)
		}
		out = append(out, fr)
	}
	return out, rows.Err()
}

// LatestDevice returns the newest full row for a serial (read-shape 2a), or (nil, nil) when the device
// has no telemetry yet (sparse — the normal case; never an error, never a panic — D22.9).
func (r *Repo) LatestDevice(ctx context.Context, serial string) (*DeviceTelemetry, error) {
	row := r.pool.QueryRow(ctx, latestDeviceSQL, serial)

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
		if errors.Is(err, pgx.ErrNoRows) {
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
		DiagRunstate:  nullI16(diagRunstate, false), // SMALLINT, NULL-only sentinel (real -32768 survives)
		DiagO0:        nullI16(diagO0, false),
		DiagO1:        nullI16(diagO1, false),
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

// DeviceHistory returns the recent history window for a serial (read-shape 2b), newest-first, double
// bounded by window (now-window..now) and maxRows (D22.8). A non-positive window falls back to the row
// cap alone (epoch start).
func (r *Repo) DeviceHistory(ctx context.Context, serial string, window time.Duration, maxRows int) ([]HistoryPoint, error) {
	if maxRows < 1 {
		maxRows = 1
	}
	since := time.Now().Add(-window)
	if window <= 0 {
		since = time.Unix(0, 0)
	}

	rows, err := r.pool.Query(ctx, deviceHistorySQL, serial, since, maxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HistoryPoint{}
	for rows.Next() {
		var (
			t                 time.Time
			battMV, badBoots  *int32
			battPct, diagOTARR *int16
			bootCount, uptime *int64
			resetReason       *string
		)
		if err := rows.Scan(&t, &battMV, &battPct, &badBoots, &bootCount, &resetReason, &uptime, &diagOTARR); err != nil {
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
			DiagOTARR:   nullI16(diagOTARR, false),
		})
	}
	return out, rows.Err()
}
