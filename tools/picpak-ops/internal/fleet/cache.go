// Package fleet is the shared serial→device cache the read-side panes reuse. The
// devices table (serial PK, channel, mac, label, last_seen) is the authoritative
// label/channel mapping for a serial; telemetry and logs both need it to render a
// row as more than a bare serial. Rather than each pane querying devices
// independently (09-logs §6 "should reuse one devices cache, not query
// independently"), they share one Cache loaded over the shared read-only pool.
//
// The cache is read-only over the pool (a single SELECT) and thread-safe: Load
// builds a fresh snapshot and atomically swaps it in under a write lock, so
// concurrent BySerial/List readers always see a consistent map and never a
// half-built one.
package fleet

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// selectDevices is the single read query backing the cache. Column order matches
// the Scan below and the 0001_init.up.sql devices table. mac/label/last_seen are
// nullable in the schema, so they are scanned through pointers.
const selectDevices = `SELECT serial, channel, mac, label, last_seen FROM devices`

// Device is one cached devices row. mac and label are "" when NULL; LastSeen is
// the zero time.Time when the device has never reported (last_seen NULL). The
// identity truth is NVS on the device; this is the backend's label/channel
// cross-reference, not an authority on the hardware.
type Device struct {
	Serial   string
	Channel  string
	MAC      string
	Label    string
	LastSeen time.Time
}

// HasLastSeen reports whether the device has ever reported (last_seen non-NULL),
// distinguishing "never seen" from a zero timestamp without leaking the zero value
// into age math.
func (d Device) HasLastSeen() bool { return !d.LastSeen.IsZero() }

// Cache is the shared serial→Device map. The zero value is not usable; construct
// with New. It holds only the last loaded snapshot — no open cursor, no streaming
// connection — so it is cheap to keep around for the life of the program.
type Cache struct {
	pool *pgxpool.Pool

	mu       sync.RWMutex
	bySerial map[string]Device
}

// New returns a Cache bound to the shared pool. It does not query; call Load to
// populate it.
func New(pool *pgxpool.Pool) *Cache {
	return &Cache{
		pool:     pool,
		bySerial: map[string]Device{},
	}
}

// Load runs the devices SELECT and atomically replaces the in-memory snapshot. It
// is read-only and safe to call repeatedly (e.g. on a refresh cadence). On error
// the previous snapshot is left untouched, so a transient DB failure does not blank
// the cache. The caller supplies the context (and thus the timeout / cancellation).
func (c *Cache) Load(ctx context.Context) error {
	rows, err := c.pool.Query(ctx, selectDevices)
	if err != nil {
		return err
	}
	defer rows.Close()

	next := make(map[string]Device)
	for rows.Next() {
		var (
			d        Device
			mac      *string
			label    *string
			lastSeen *time.Time
		)
		if err := rows.Scan(&d.Serial, &d.Channel, &mac, &label, &lastSeen); err != nil {
			return err
		}
		if mac != nil {
			d.MAC = *mac
		}
		if label != nil {
			d.Label = *label
		}
		if lastSeen != nil {
			d.LastSeen = *lastSeen
		}
		next[d.Serial] = d
	}
	if err := rows.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	c.bySerial = next
	c.mu.Unlock()
	return nil
}

// BySerial returns the cached device for a serial and whether it was present. A
// missing serial returns the zero Device and false — the caller decides how to
// render an unknown serial (telemetry/logs show the bare serial).
func (c *Cache) BySerial(serial string) (Device, bool) {
	c.mu.RLock()
	d, ok := c.bySerial[serial]
	c.mu.RUnlock()
	return d, ok
}

// List returns the cached devices sorted by serial (stable order for a device
// list / picker). It returns a fresh slice each call, so the caller may sort or
// mutate it without affecting the cache.
func (c *Cache) List() []Device {
	c.mu.RLock()
	out := make([]Device, 0, len(c.bySerial))
	for _, d := range c.bySerial {
		out = append(out, d)
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

// Len reports the number of cached devices (cheap, lock-guarded).
func (c *Cache) Len() int {
	c.mu.RLock()
	n := len(c.bySerial)
	c.mu.RUnlock()
	return n
}
