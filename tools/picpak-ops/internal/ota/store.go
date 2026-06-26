package ota

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SQL strings isolated as the single audit point (column names/order match
// 0001_init.up.sql exactly). Every statement is a SELECT — the read path is read-only
// and the shared pool is read_only-enforced (internal/db), so a stray write would be
// rejected with SQLSTATE 25006 regardless.

const versionsSQL = `SELECT version, sha256, blob_path, size_bytes, COALESCE(notes,''), created_at
FROM firmware_versions
ORDER BY created_at DESC, version`

const channelsSQL = `SELECT name, COALESCE(default_version, '')
FROM channels
ORDER BY name`

const rolloutsSQL = `SELECT id, serial, channel, version, state, pinned, updated_at
FROM rollout_targets
ORDER BY channel, serial`

// Store is the read-only OTA repository over the shared *pgxpool.Pool. It never opens
// its own pool (internal/db owns the single handle) and never writes. Each query
// derives a statement-timeout-bounded child context from the caller's per-pane context
// so an in-flight SELECT is cancelled on Close()/app-quit.
type Store struct {
	pool             *pgxpool.Pool
	statementTimeout time.Duration
}

// NewStore binds a Store to the shared pool and the configured statement timeout.
func NewStore(pool *pgxpool.Pool, statementTimeout time.Duration) *Store {
	return &Store{pool: pool, statementTimeout: statementTimeout}
}

func (s *Store) queryCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.statementTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, s.statementTimeout)
}

// Load reads the three OTA tables into an OTAState (versions, channels, rollouts). It
// does NOT populate Devices — the pane assembles the per-device target-vs-running view
// from the fleet cache + telemetry running_ver + the pure BuildDeviceOTA. FetchedAt is
// stamped at read time.
func (s *Store) Load(ctx context.Context) (OTAState, error) {
	versions, err := s.versions(ctx)
	if err != nil {
		return OTAState{}, err
	}
	channels, err := s.channels(ctx)
	if err != nil {
		return OTAState{}, err
	}
	rollouts, err := s.rollouts(ctx)
	if err != nil {
		return OTAState{}, err
	}
	return OTAState{
		Versions:  versions,
		Channels:  channels,
		Rollouts:  rollouts,
		FetchedAt: time.Now(),
	}, nil
}

func (s *Store) versions(ctx context.Context) ([]FirmwareVersion, error) {
	qctx, cancel := s.queryCtx(ctx)
	defer cancel()
	rows, err := s.pool.Query(qctx, versionsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FirmwareVersion
	for rows.Next() {
		var v FirmwareVersion
		if err := rows.Scan(&v.Version, &v.SHA256, &v.BlobPath, &v.SizeBytes, &v.Notes, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) channels(ctx context.Context) ([]Channel, error) {
	qctx, cancel := s.queryCtx(ctx)
	defer cancel()
	rows, err := s.pool.Query(qctx, channelsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.Name, &c.DefaultVersion); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) rollouts(ctx context.Context) ([]Rollout, error) {
	qctx, cancel := s.queryCtx(ctx)
	defer cancel()
	rows, err := s.pool.Query(qctx, rolloutsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rollout
	for rows.Next() {
		var r Rollout
		if err := rows.Scan(&r.ID, &r.Serial, &r.Channel, &r.Version, &r.State, &r.Pinned, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
