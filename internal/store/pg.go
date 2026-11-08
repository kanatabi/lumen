package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/domain"
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
		SELECT src, alt, width, height, gallery, featured, sort, collection
		FROM photos ORDER BY sort ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Photo
	for rows.Next() {
		var p domain.Photo
		if err := rows.Scan(&p.Src, &p.Alt, &p.Width, &p.Height,
			&p.Gallery, &p.Featured, &p.Sort, &p.Collection); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TagCloud reads the CURATED cloud (flag + count maintained at publish/seed,
// §1 — never aggregated at read; vocabulary rows exist for FK integrity but
// do not surface). Ties break by name, the documented tie order.
func (g *PG) TagCloud(ctx context.Context) ([]domain.Tag, error) {
	rows, err := g.pool.Query(ctx, `SELECT name, posts FROM tags WHERE cloud ORDER BY posts DESC, name ASC`)
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
