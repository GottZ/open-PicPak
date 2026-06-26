package logs

import (
	"charm.land/bubbles/v2/viewport"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/fleet"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// New is the logs-pane factory (registered for pane.KindLogs in main.go). It closes over
// the shared (possibly nil) read pool and the shared fleet cache (K9). A nil pool ⇒ no
// store ⇒ the pane renders "no database configured" and never tails — the bench cockpit
// (Phase C) runs without a DB. The store is constructed but NOT started here; the pane's
// Init starts the single tail goroutine.
func New(pool *pgxpool.Pool, cache *fleet.Cache) app.PaneFactory {
	return func(base pane.BasePane, cfg *config.Config) pane.Pane {
		lc := configFrom(cfg)
		p := &logsPane{
			BasePane: base,
			cache:    cache,
			cfg:      lc,
			keys:     resolveKeys(cfg.Logs.Keys),
			colors:   resolveColors(cfg),
			loc:      resolveLocation(lc.Timezone),
			vp:       viewport.New(),
			follow:   lc.FollowDefault,
		}
		p.vp.SoftWrap = true
		if pool != nil {
			p.store = newLogStore(p.Context(), pool, p.ID(), p.Sender(), lc)
		}
		return p
	}
}

// configFrom maps the validated [logs] section into the package's own Config snapshot
// (Policy=Data — every separator/cadence/bound/template/color comes from here, never a
// code literal in the logs package).
func configFrom(c *config.Config) Config {
	l := c.Logs
	return Config{
		LineSeparator:   l.LineSeparator,
		PollInterval:    l.PollInterval.D(),
		PollBatch:       l.PollBatch,
		BackfillPage:    l.BackfillPage,
		LiveRingLines:   l.LiveRingLines,
		HistoryMaxRows:  l.HistoryMaxRows,
		DefaultSerials:  append([]string(nil), l.DefaultFilterSerials...),
		DefaultSources:  append([]string(nil), l.DefaultSources...),
		ShowGapMarkers:  l.ShowGapMarkers,
		GapMarkerFormat: l.GapMarkerFormat,
		ShowSuspect:     l.ShowSuspectMarkers,
		SuspectMarker:   l.SuspectMarker,
		FollowDefault:   l.FollowDefault,
		ErrorBackoff:    l.ErrorBackoff.D(),
		ErrorBackoffMax: l.ErrorBackoffMax.D(),
		TimestampFormat: l.TimestampFormat,
		Timezone:        l.Timezone,
		ColorizeBy:      l.ColorizeBy,
		Palette:         append([]string(nil), l.Palette...),
		QueryTimeout:    l.QueryTimeout.D(),
	}
}
