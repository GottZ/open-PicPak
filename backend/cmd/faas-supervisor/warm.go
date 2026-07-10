package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/open-picpak/backend/internal/commandstore"
	"github.com/open-picpak/backend/internal/imgstore"
	"github.com/open-picpak/backend/internal/playliststore"
)

// playlistPushEnabled reports whether the server-driven playlist-push feature (pre-pack warmer +
// direct-display refresh fan-out) is armed. DEFAULT-OFF (K10): the feature couples to the on-device
// HOTP gate and must never expose the fleet until that gate is armed, so nothing warms or fans out
// unless FAAS_PLAYLIST_PUSH is explicitly "true" (§5, K-HOTP). The flag gates the whole warmer
// goroutine — off means neither the warmer nor the bridge runs (a strict non-regression).
func playlistPushEnabled() bool { return os.Getenv("FAAS_PLAYLIST_PUSH") == "true" }

// warmPlaylist proactively packs a playlist's distinct (image, fit, dither) variants through the SAME
// production path a live /frame takes — VariantGet → single-flight → renderPlaylist → VariantPut
// (s.packedVariant) — so the fleet-shared frame_variant_cache is warm BEFORE the refresh wake wave
// arrives and the first device never eats a cold ~18s render (27:§4.6, cache-stampede prevention).
//
// The warm is STAGGERED under a bounded semaphore (s.warmLimit, default 2): a mutation on an N-serial
// playlist must NOT drive all its variants through the worker at once — that is exactly the inline
// sync-render storm K5 forbids (a version bump couples to rate-limited warming, never a fan-out of
// simultaneous renders). Distinct variants only: the same image at the same policy packs identically
// fleet-wide (content-addressed), so warming it once suffices. Best-effort: a per-variant failure is
// logged and skipped (the /frame single-flight remains the correctness backstop).
func (s *supervisor) warmPlaylist(ctx context.Context, playlistID int64) {
	items, err := playliststore.RotationItems(ctx, s.pool, playlistID)
	if err != nil {
		log.Printf("faas-supervisor: warm playlist %d items: %v", playlistID, err)
		return
	}

	type variant struct {
		imageID int64
		fit     string
		dither  string
	}
	seen := map[variant]bool{}
	todo := make([]variant, 0, len(items))
	for _, it := range items {
		v := variant{it.ImageID, it.Fit, it.Dither}
		if !seen[v] {
			seen[v] = true
			todo = append(todo, v)
		}
	}

	limit := s.warmLimit
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, v := range todo {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(v variant) {
			defer wg.Done()
			defer func() { <-sem }()
			img, err := imgstore.GetImage(ctx, s.pool, v.imageID)
			if err != nil {
				log.Printf("faas-supervisor: warm image %d: %v", v.imageID, err)
				return
			}
			item := playliststore.PlaylistItem{ImageID: v.imageID, Fit: v.fit, Dither: v.dither}
			if _, err := s.packedVariant(ctx, img, item); err != nil {
				log.Printf("faas-supervisor: warm variant sha=%s fit=%s dither=%s: %v", img.Sha256, v.fit, v.dither, err)
			}
		}(v)
	}
	wg.Wait()
}

// runPlaylistWarmer holds a dedicated LISTEN connection on playlist_changed and, per notification,
// warms the playlist's variants then fans out the refresh (warm THEN wake, so the pre-pack lands
// before the device polls). Mirrors cmd/ingest's c2Notifier.listenLoop: reconnect-on-error, one DB
// connection for the process lifetime. Started only when playlistPushEnabled() (default-off).
func (s *supervisor) runPlaylistWarmer(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.warmListenOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("faas-supervisor: playlist warmer listen: %v (reconnect in 1s)", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (s *supervisor) warmListenOnce(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN playlist_changed"); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		playlistID, perr := strconv.ParseInt(n.Payload, 10, 64)
		if perr != nil {
			log.Printf("faas-supervisor: playlist warmer bad payload %q: %v", n.Payload, perr)
			continue
		}
		// Warm first so the cache is hot before the wake wave the enqueue triggers (§4.6). The refresh
		// is a server-side mutation side-effect → NULL operator attribution (the edit itself is already
		// operator-stamped on playlist.operator_key_id).
		s.warmPlaylist(ctx, playlistID)
		if _, err := commandstore.EnqueuePlaylistRefresh(ctx, s.pool, playlistID, nil); err != nil {
			log.Printf("faas-supervisor: playlist %d refresh fan-out: %v", playlistID, err)
		}
	}
}
