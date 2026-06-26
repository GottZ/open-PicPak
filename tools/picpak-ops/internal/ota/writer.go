package ota

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// Typed errors mapping the Admin-API contract's error codes (409/422/404) AND the
// equivalent DirectPGXWriter integrity failures, so the pane renders one message
// regardless of which backend wrote. ErrBadSHA is the tool-side fail-closed gate the
// firmware/DB-CHECK lowercase contract demands; it fires BEFORE any write.
var (
	ErrBadSHA         = errors.New("sha256 must be 64 lowercase hex digits")
	ErrVersionExists  = errors.New("firmware version already exists")
	ErrSHAMismatch    = errors.New("sha/blob mismatch (server re-hash rejected the upload)")
	ErrUnknownRef     = errors.New("unknown channel or version")
	ErrBadState       = errors.New("rollout state must be active|paused|done")
	ErrWritesDisabled = errors.New("OTA writes are disabled (no admin_api_url and allow_direct_write=false)")
)

// RegisterFirmware is the request to register a built firmware as a firmware_versions
// row. SHA256 must be 64 lowercase hex (validated before the call). Blob carries the
// app bytes for the AdminAPIWriter's multipart upload; BlobPath is the serving-location
// path the DirectPGXWriter records (and copies Blob to when a serving dir is set) —
// the schema stores only blob_path.
type RegisterFirmware struct {
	Version   string
	SHA256    string
	SizeBytes int64
	Notes     string
	Blob      []byte
	BlobPath  string
}

// SetChannelDefault sets channels.default_version (FK-checked against firmware_versions).
type SetChannelDefault struct {
	Channel string
	Version string
}

// PinRollout upserts a rollout_targets row on (serial,channel) with state='active'.
// Serial "*" denotes a channel-fleet target. Pinned=true makes a per-serial pin beat
// the channel default; Pinned=false keeps the target active but unpinned (the "unpin"
// action re-submits the existing version with Pinned=false).
type PinRollout struct {
	Serial  string
	Channel string
	Version string
	Pinned  bool
}

// SetRolloutState sets a rollout_targets row's state by id (active|paused|done).
type SetRolloutState struct {
	ID    int64
	State string
}

// Writer is the single OTA write interface; the two backends (AdminAPIWriter,
// DirectPGXWriter) implement it. Every method runs in a pane goroutine (never on the
// Bubble Tea loop). IsDirect reports whether the active backend bypasses the backend
// authority (drives the red banner). Close releases any backend resources.
type Writer interface {
	RegisterFirmware(ctx context.Context, req RegisterFirmware) error
	SetChannelDefault(ctx context.Context, req SetChannelDefault) error
	PinRollout(ctx context.Context, req PinRollout) error
	SetRolloutState(ctx context.Context, req SetRolloutState) error
	IsDirect() bool
	Close() error
}

// NewWriter picks the write backend by ota.allow_direct_write (design 07): the
// AdminAPIWriter is the default/preferred path; the DirectPGXWriter is the interim,
// flag-gated path (config rule 3 already guarantees a writable session + DSN when the
// flag is set). It returns (nil, nil) when writes are disabled (flag off AND no
// admin_api_url) so the pane runs read-only without crashing. A direct-write request
// with a nil pool is a misconfiguration the caller surfaces (returns an error).
func NewWriter(cfg *config.Config, pool *pgxpool.Pool) (Writer, error) {
	o := cfg.OTA
	if o.AllowDirectWrite {
		if pool == nil {
			return nil, errors.New("ota: allow_direct_write=true but no database pool (set database.dsn / read_only=false)")
		}
		return NewDirectPGXWriter(pool, cfg.Database.StatementTimeout.D()), nil
	}
	if o.AdminAPIURL != "" {
		return NewAdminAPIWriter(o.AdminAPIURL, o.AdminAPIToken, o.AdminAPITimeout.D()), nil
	}
	return nil, nil // writes disabled (read-only mode)
}
