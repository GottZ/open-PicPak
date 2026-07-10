package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/imgstore"
	"github.com/open-picpak/backend/internal/plrender"
)

// startOrphanBlobSweep reconciles the imgblobs volume against the image table on a ticker until ctx is
// cancelled (maintenance, W6 / design §6). The volume grows monotonically with orphaned blobs from the
// two documented crash windows (a committed-less insert's blob, a delete that dropped the row before
// its file) unless this reconcile removes them. It runs HERE because admin mounts imgblobs :rw (the
// supervisor has it :ro and cannot unlink), symmetric to registerImageRoutes. Mirror of
// startFragmentPrune. IMG_ORPHAN_BLOB_GRACE shields the write→commit window (a blob younger than grace
// may belong to an as-yet-uncommitted row); it is a safety floor, NOT a created_at TTL.
func startOrphanBlobSweep(ctx context.Context, pool *pgxpool.Pool, blobDir string) {
	interval := envDurOr("IMG_ORPHAN_SWEEP_INTERVAL", 6*time.Hour)
	grace := envDurOr("IMG_ORPHAN_BLOB_GRACE", time.Hour)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				switch removed, missing, err := imgstore.SweepOrphanBlobs(ctx, pool, blobDir, grace, time.Now()); {
				case err != nil:
					log.Printf("imgblob orphan sweep: %v", err)
				default:
					if removed > 0 {
						log.Printf("imgblob orphan sweep removed %d blob(s)", removed)
					}
					if missing > 0 {
						log.Printf("imgblob orphan sweep: %d image row(s) missing their blob (heals on re-upload)", missing)
					}
				}
			}
		}
	}()
}

// startVariantCacheGC evicts unreferenced frame_variant_cache rows on a ticker until ctx is cancelled
// (maintenance, W6 / design §6). The cache grows with every distinct (sha, fit, dither) rendered; the
// GC is refcount-driven (a row goes when no playlist_item references its content+policy tuple), NOT a
// created_at TTL — a time-based sweep would evict the hottest, still-served variants and cause a
// re-pack storm (§6). Mirror of startFragmentPrune / startSessionPrune.
func startVariantCacheGC(ctx context.Context, pool *pgxpool.Pool) {
	interval := envDurOr("VARIANT_CACHE_GC_INTERVAL", time.Hour)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				switch n, err := plrender.GCVariantCache(ctx, pool); {
				case err != nil:
					log.Printf("variant-cache GC: %v", err)
				case n > 0:
					log.Printf("variant-cache GC removed %d row(s)", n)
				}
			}
		}
	}()
}
