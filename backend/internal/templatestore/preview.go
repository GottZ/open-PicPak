package templatestore

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// PreviewTarget is one builtin render_fn row the warmup pre-renders into template_preview (§4.5a). Only
// render_fn templates produce a frame; a berry_snippet is a device command with no preview.
type PreviewTarget struct {
	ID      int64
	Name    string
	Version int
	Source  string
}

// BuiltinPreviewTargets returns every builtin render_fn (id, name, version, source) for the warmup loop.
// berry_snippet / playlist_preset builtins are excluded — they have no rendered frame. Ordered by id so
// the warmup walks the catalog deterministically.
func BuiltinPreviewTargets(ctx context.Context, q Querier) ([]PreviewTarget, error) {
	rows, err := q.Query(ctx, `
		SELECT id, name, version, source FROM templates
		WHERE builtin AND kind = $1 ORDER BY id`, KindRenderFn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PreviewTarget{}
	for rows.Next() {
		var t PreviewTarget
		if err := rows.Scan(&t.ID, &t.Name, &t.Version, &t.Source); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LoadPreview returns the cached PNG for (templateID, version) or ErrNotFound. The version is the cache
// discriminator: a bumped template is a miss (re-render), never a stale serve.
func LoadPreview(ctx context.Context, q Querier, templateID int64, version int) ([]byte, error) {
	var png []byte
	err := q.QueryRow(ctx, `SELECT png FROM template_preview WHERE template_id = $1 AND version = $2`,
		templateID, version).Scan(&png)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return png, nil
}

// StorePreview upserts the rendered PNG for (templateID, version). The upsert is idempotent on the cache
// key, so a warmup that races an on-demand render (or a restart) converges on the same row without a
// duplicate-key error.
func StorePreview(ctx context.Context, q Querier, templateID int64, version int, png []byte) error {
	_, err := q.Exec(ctx, `
		INSERT INTO template_preview (template_id, version, png) VALUES ($1, $2, $3)
		ON CONFLICT (template_id, version) DO UPDATE SET png = EXCLUDED.png, rendered_at = now()`,
		templateID, version, png)
	return err
}

// PreviewFixtures returns name→PNG for every builtin catalog entry that ships a curated preview fixture
// (§4.5a): a fetch builtin whose live render would hit egress-deny (EgressAllow=[] on the preview
// profile) and therefore serve the error frame — its flagship card must instead show a labelled
// example render. The fixture bytes are the preview; the live-render path is skipped for these names.
// Fails loud on a broken embed (a packaging defect, parity loadCatalog).
func PreviewFixtures() (map[string][]byte, error) {
	entries, _, err := loadCatalog()
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.PreviewFixture == "" {
			continue
		}
		png, err := builtinsFS.ReadFile("builtins/" + e.PreviewFixture)
		if err != nil {
			return nil, err
		}
		out[e.Name] = png
	}
	return out, nil
}
