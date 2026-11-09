package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/search"
)

// PG is the Postgres Source. One hard rule lives in its SQL: status =
// 'published' (§1 — drafts never leak, no matter what the service asks).
// Beyond that it stays honest at blog scale: the corpus is rows-hundreds, so
// it loads published rows and lets the shared domain logic shape them — the
// ONE place §1 subtleties live is the service, not two dialects of SQL.
// M3's Meilisearch replaces the scoring source, not this seam.
type PG struct{ pool *pgxpool.Pool }

func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

const publishedPostsSQL = `
SELECT p.slug, p.title, p.subtitle, p.preview,
       to_char(p.date, 'YYYY-MM-DD'), p.read_time, p.thumbnail, p.content_md,
       COALESCE(
         (SELECT array_agg(t.name ORDER BY pt.position)
            FROM post_tags pt JOIN tags t ON t.id = pt.tag_id
           WHERE pt.post_id = p.id),
         '{}')
FROM posts p
WHERE p.status = 'published'
ORDER BY p.rank ASC, p.slug ASC`

func (g *PG) PublishedPosts(ctx context.Context) ([]domain.Detail, error) {
	rows, err := g.pool.Query(ctx, publishedPostsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Detail
	for rows.Next() {
		var d domain.Detail
		var tags []string
		var thumb *string
		if err := rows.Scan(&d.Slug, &d.Title, &d.Subtitle, &d.Preview, &d.Date,
			&d.ReadTime, &thumb, &d.Content, &tags); err != nil {
			return nil, err
		}
		d.Thumbnail = thumb
		d.Tags = tags
		out = append(out, d)
	}
	return out, rows.Err()
}

func (g *PG) Photos(ctx context.Context) ([]domain.Photo, error) {
	rows, err := g.pool.Query(ctx, `
		SELECT id, src, alt, width, height, gallery, featured, sort, collection
		FROM photos ORDER BY sort ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Photo
	for rows.Next() {
		var p domain.Photo
		if err := rows.Scan(&p.ID, &p.Src, &p.Alt, &p.Width, &p.Height,
			&p.Gallery, &p.Featured, &p.Sort, &p.Collection); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TagCloud reads the curated widget (0005: the flag is `featured`; the desk
// toggles it by hand — promotion is curation, never arithmetic). The public
// list shows FEATURED tags WITH at least one live published post: a featured
// tag at zero is ready, not display-worthy (the widget never shows "0").
// Counts stay publish-maintained (§1: never aggregated at read for SHAPE —
// this filters on the maintained column, it does not compute it). Ties break
// by name, the documented tie order.
func (g *PG) TagCloud(ctx context.Context) ([]domain.Tag, error) {
	rows, err := g.pool.Query(ctx, `SELECT name, posts FROM tags WHERE featured AND posts > 0 ORDER BY posts DESC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tag
	for rows.Next() {
		var t domain.Tag
		if err := rows.Scan(&t.Name, &t.Posts); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

var _ Source = (*PG)(nil)

// ---- doc lookups for the indexing worker (truth read at RUN time — jobs
// carry ids only, so retries naturally coalesce to current state).
// SOFT-DELETED rows count as VANISHED: the worker deletes the DOC. Without
// the deleted_at filter a soft-deleted published post re-indexes with its
// stale status — items stay honest (hydration drops them) but the engine's
// TOTAL inflates. Found by the mellon cutover e2e: total 13, truth 12. ----

func (g *PG) PostDocByID(ctx context.Context, id int64) (search.PostDoc, bool, error) {
	var d search.PostDoc
	var status string
	var tags []string
	err := g.pool.QueryRow(ctx, `
		SELECT p.id, p.slug, p.title, p.subtitle, p.preview, p.content_md,
		       p.status::text, EXTRACT(YEAR FROM p.date)::int*10000
		         + EXTRACT(MONTH FROM p.date)::int*100 + EXTRACT(DAY FROM p.date)::int,
		       to_char(p.date,'YYYY-MM-DD'), p.read_time, p.rank,
		       COALESCE(
		         (SELECT array_agg(t.name ORDER BY pt.position)
		            FROM post_tags pt JOIN tags t ON t.id = pt.tag_id
		           WHERE pt.post_id = p.id),
		         '{}')
		FROM posts p
		WHERE p.id = $1 AND p.deleted_at IS NULL`, id).
		Scan(&d.ID, &d.Slug, &d.Title, &d.Subtitle, &d.Preview, &d.Content,
			&status, &d.Day, &d.Date, &d.ReadTime, &d.Rank, &tags)
	if err != nil {
		if err == pgx.ErrNoRows {
			return search.PostDoc{}, false, nil
		}
		return search.PostDoc{}, false, err
	}
	d.Status = status
	d.Tags = tags
	return d, true, nil
}

func (g *PG) PhotoDocByID(ctx context.Context, id int64) (search.PhotoDoc, bool, error) {
	var p search.PhotoDoc
	var collection *string
	err := g.pool.QueryRow(ctx,
		`SELECT id, src, alt, width, height, gallery, featured, sort, collection
		 FROM photos WHERE id = $1`, id).
		Scan(&p.ID, &p.Src, &p.Alt, &p.Width, &p.Height, &p.Gallery, &p.Featured,
			&p.Sort, &collection)
	if err != nil {
		if err == pgx.ErrNoRows {
			return search.PhotoDoc{}, false, nil
		}
		return search.PhotoDoc{}, false, err
	}
	p.Collection = collection
	return p, true, nil
}

// AllPostIDs / AllPhotoIDs drive cmd/index-init (full reconcile).
func (g *PG) AllPostIDs(ctx context.Context) ([]int64, error) {
	rows, err := g.pool.Query(ctx, `SELECT id FROM posts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (g *PG) AllPhotoIDs(ctx context.Context) ([]int64, error) {
	rows, err := g.pool.Query(ctx, `SELECT id FROM photos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetPost keeps the 404 distinction at the data edge for callers that want it
// (the service uses the loaded set; this exists for the integration harness
// asserting the WHERE clause actually excludes drafts).
func (g *PG) PostPublished(ctx context.Context, slug string) (domain.Detail, error) {
	var d domain.Detail
	var tags []string
	err := g.pool.QueryRow(ctx, `
		SELECT p.slug, p.title, p.subtitle, p.preview, to_char(p.date,'YYYY-MM-DD'),
		       p.read_time, p.thumbnail, p.content_md,
		       COALESCE(
		         (SELECT array_agg(t.name ORDER BY pt.position)
		            FROM post_tags pt JOIN tags t ON t.id = pt.tag_id
		           WHERE pt.post_id = p.id),
		         '{}')
		FROM posts p
		WHERE p.slug = $1 AND p.status = 'published'`).
		Scan(&d.Slug, &d.Title, &d.Subtitle, &d.Preview, &d.Date,
			&d.ReadTime, &d.Thumbnail, &d.Content, &tags)
	if err != nil {
		if err == pgx.ErrNoRows {
			return domain.Detail{}, ErrNotFound
		}
		return domain.Detail{}, err
	}
	d.Tags = tags
	return d, nil
}
