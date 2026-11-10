package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Admin payloads mirror mellon's AdminApi shapes EXACTLY (client.ts is the
// executable spec; drift is a bug in whichever moved).

type AdminPostSummary struct {
	Slug        string  `json:"slug"`
	Title       string  `json:"title"`
	Subtitle    string  `json:"subtitle"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"createdAt"`
	PublishedAt *string `json:"publishedAt"`
	UpdatedAt   string  `json:"updatedAt"`
	Rev         int     `json:"rev"`
}

type AdminPostDetail struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle"`
	Preview     string   `json:"preview"`
	Date        string   `json:"date"`
	Tags        []string `json:"tags"`
	Content     string   `json:"content"`
	Thumbnail   *string  `json:"thumbnail"`
	Status      string   `json:"status"`
	ReadTime    int      `json:"readTime"`
	Rev         int      `json:"rev"`
	CreatedAt   string   `json:"createdAt"`
	PublishedAt *string  `json:"publishedAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

type PostDraftInput struct {
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	Subtitle  string   `json:"subtitle"`
	Preview   string   `json:"preview"`
	Date      string   `json:"date"`
	Tags      []string `json:"tags"`
	Content   string   `json:"content"`
	Thumbnail *string  `json:"thumbnail"`
}

var (
	ErrSlugTaken     = errors.New("slug already exists")
	ErrRevConflict   = errors.New("stale rev — someone saved first")
	ErrBadTransition = errors.New("illegal status transition")
	ErrUnknownColl   = errors.New("unknown collection")
	ErrDupVocab      = errors.New("already exists")
	ErrNoPosts       = errors.New("no such post")
	ErrNoPhoto       = errors.New("no such photo")
)

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }
func isoPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := iso(*t)
	return &s
}

type adminPostRow struct {
	slug, title, subtitle, status string
	createdAt, updatedAt          time.Time
	publishedAt                   *time.Time
	rev                           int
}

const adminPostCols = `slug, title, subtitle, status::text,
	created_at, published_at, updated_at, rev`

func scanAdminPost(row pgx.Row) (adminPostRow, error) {
	var r adminPostRow
	var pub pgtype.Timestamptz
	err := row.Scan(&r.slug, &r.title, &r.subtitle, &r.status,
		&r.createdAt, &pub, &r.updatedAt, &r.rev)
	if pub.Valid {
		t := pub.Time
		r.publishedAt = &t
	}
	return r, err
}

func (r adminPostRow) summary() AdminPostSummary {
	return AdminPostSummary{
		Slug: r.slug, Title: r.title, Subtitle: r.subtitle, Status: r.status,
		CreatedAt: iso(r.createdAt), PublishedAt: isoPtr(r.publishedAt),
		UpdatedAt: iso(r.updatedAt), Rev: r.rev,
	}
}

// ---- posts: list (paged envelope, §8) ----

type AdminListOpts struct {
	Status   string
	Tags     []string
	From     string
	To       string
	Sort     string // updated|created|published|title
	Dir      string
	Page     int
	PageSize int
}

func (g *PG) ListAdminPosts(ctx context.Context, o AdminListOpts) (int, []AdminPostSummary, error) {
	where := []string{"deleted_at IS NULL"}
	args := []any{}
	n := 0
	add := func(cond string, v any) { n++; args = append(args, v); where = append(where, fmt.Sprintf(cond, n)) }
	if o.Status != "" {
		add("status = $%d::post_status", o.Status)
	}
	if o.From != "" {
		add("date >= $%d::date", o.From)
	}
	if o.To != "" {
		add("date <= $%d::date", o.To)
	}
	if len(o.Tags) > 0 {
		n++
		args = append(args, o.Tags)
		where = append(where, fmt.Sprintf(
			`EXISTS (SELECT 1 FROM post_tags pt JOIN tags t ON t.id = pt.tag_id
			 WHERE pt.post_id = posts.id AND t.name = ANY($%d))`, n))
	}
	sortCol := map[string]string{"updated": "updated_at", "created": "created_at",
		"published": "published_at", "title": "lower(title)"}[o.Sort]
	if sortCol == "" {
		sortCol = "updated_at"
	}
	dir := "DESC"
	if strings.EqualFold(o.Dir, "asc") {
		dir = "ASC"
	}
	pageSize := o.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	page := max(o.Page, 1)

	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := g.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(*) FROM posts WHERE %s`, whereSQL), args...).Scan(&total); err != nil {
		return 0, nil, err
	}
	totalPages := max(1, (total+pageSize-1)/pageSize)
	page = min(page, totalPages) // out-of-range clamps (mellon envelope contract)
	rows, err := g.pool.Query(ctx,
		fmt.Sprintf(`SELECT %s FROM posts WHERE %s ORDER BY %s %s, slug ASC LIMIT $%d OFFSET $%d`,
			adminPostCols, whereSQL, sortCol, dir, n+1, n+2),
		append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var out []AdminPostSummary
	for rows.Next() {
		r, err := scanAdminPost(rows)
		if err != nil {
			return 0, nil, err
		}
		out = append(out, r.summary())
	}
	if out == nil {
		out = []AdminPostSummary{}
	}
	return total, out, nil
}

// ---- posts: detail / create / update / publish / delete ----

func (g *PG) GetAdminPost(ctx context.Context, slug string) (AdminPostDetail, error) {
	var d AdminPostDetail
	var pub pgtype.Timestamptz
	var created, updated time.Time
	var tags []string
	err := g.pool.QueryRow(ctx, `
		SELECT p.slug, p.title, p.subtitle, p.preview, to_char(p.date,'YYYY-MM-DD'),
		       p.content_md, p.thumbnail, p.status::text, p.read_time, p.rev,
		       p.created_at, p.published_at, p.updated_at,
		       COALESCE((SELECT array_agg(t.name ORDER BY pt.position)
		                 FROM post_tags pt JOIN tags t ON t.id = pt.tag_id
		                WHERE pt.post_id = p.id), '{}')
		FROM posts p WHERE p.slug = $1 AND p.deleted_at IS NULL`, slug).
		Scan(&d.Slug, &d.Title, &d.Subtitle, &d.Preview, &d.Date, &d.Content,
			&d.Thumbnail, &d.Status, &d.ReadTime, &d.Rev,
			&created, &pub, &updated, &tags)
	if err == pgx.ErrNoRows {
		return d, ErrNoPosts
	}
	if err != nil {
		return d, err
	}
	d.Tags = tags
	d.CreatedAt, d.UpdatedAt = iso(created), iso(updated)
	if pub.Valid {
		p := iso(pub.Time)
		d.PublishedAt = &p
	}
	return d, nil
}

func (g *PG) CreateAdminPost(ctx context.Context, in PostDraftInput, enqueuer indexEnqueuer) (AdminPostDetail, error) {
	if in.Slug == "" || in.Title == "" {
		return AdminPostDetail{}, fmt.Errorf("slug and title required")
	}
	d, err := g.GetAdminPost(ctx, in.Slug)
	if err == nil {
		return d, ErrSlugTaken
	}
	if !errors.Is(err, ErrNoPosts) {
		return d, err
	}
	err = pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if err := g.replaceTagsTx(ctx, tx, in.Tags, enqueuer); err != nil {
			return err
		}
		pid, err := insertPostTx(ctx, tx, in, "draft", 0)
		if err != nil {
			return err
		}
		if err := linkTagsTx(ctx, tx, pid, in.Tags); err != nil {
			return err
		}
		// a created draft must VISIBLE-AT-THE-DOOR immediately (the §8 admin
		// search shows drafts — no engine doc until the first update made
		// fresh drafts invisible there; the owner's code-read found this).
		return enqueuer.InsertIndexPostTx(ctx, tx, pid)
	})
	if err != nil {
		// the GetAdminPost pre-check is polite, not protective: two concurrent
		// creates both pass it and the loser hits the partial unique. Map the
		// race to the same 409 the pre-check path returns — never a 500.
		var pqErr *pgconn.PgError
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return AdminPostDetail{}, ErrSlugTaken
		}
		return AdminPostDetail{}, err
	}
	return g.GetAdminPost(ctx, in.Slug)
}

func insertPostTx(ctx context.Context, tx pgx.Tx, in PostDraftInput, status string, rev int) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO posts(slug, title, subtitle, preview, content_md, rank, status,
		                  date, read_time, word_count, thumbnail, rev)
		VALUES ($1,$2,$3,$4,$5,0,$6::post_status,$7::date,0,0,$8,$9)
		RETURNING id`,
		in.Slug, in.Title, in.Subtitle, in.Preview, in.Content, status, in.Date,
		in.Thumbnail, max(rev, 1)).Scan(&id)
	return id, err
}

// replaceTagsTx mints vocabulary rows for tag names that are new (implicit
// creation is unchanged doctrine) — but a MINTED row now also mints its
// engine doc intent: the index_tag job commits inside this very tx. Rows
// that already exist are skipped (ErrNoRows from the DO NOTHING RETURNING):
// their docs are the backfill's or a previous job's business, no churn here.
func (g *PG) replaceTagsTx(ctx context.Context, tx pgx.Tx, tags []string, enqueuer indexEnqueuer) error {
	for _, t := range tags {
		var id int64
		err := tx.QueryRow(ctx,
			`INSERT INTO tags(name, posts, featured) VALUES ($1, 0, FALSE) ON CONFLICT DO NOTHING RETURNING id`, t).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // existing vocabulary row
		}
		if err != nil {
			return err
		}
		if err := enqueuer.InsertIndexTagTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func linkTagsTx(ctx context.Context, tx pgx.Tx, postID int64, tags []string) error {
	for i, t := range tags {
		var tid int64
		if err := tx.QueryRow(ctx, `SELECT id FROM tags WHERE name = $1`, t).Scan(&tid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO post_tags(post_id, tag_id, position) VALUES ($1,$2,$3)
			 ON CONFLICT DO NOTHING`, postID, tid, i); err != nil {
			return err
		}
	}
	return nil
}

// UpdateAdminPost is the optimistic-locked write (§8): rev must match or
// NOTHING is touched — the portal's CAS promise, enforced here. The index
// job rides the same transaction (river InsertTx — the whole reason river
// won the queue seat).
func (g *PG) UpdateAdminPost(ctx context.Context, slug string, in PostDraftInput, rev int, enqueuer indexEnqueuer) (AdminPostDetail, error) {
	var foundID int64
	var curRev int
	err := g.pool.QueryRow(ctx,
		`SELECT id, rev FROM posts WHERE slug = $1 AND deleted_at IS NULL`, slug).
		Scan(&foundID, &curRev)
	if err == pgx.ErrNoRows {
		return AdminPostDetail{}, ErrNoPosts
	}
	if err != nil {
		return AdminPostDetail{}, err
	}
	if curRev != rev {
		return AdminPostDetail{}, ErrRevConflict
	}
	err = pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx,
			`UPDATE posts SET title=$2, subtitle=$3, preview=$4, content_md=$5, date=$6::date,
			              thumbnail=$7, rev = rev + 1, updated_at = now()
			 WHERE id = $1 AND rev = $8`,
			foundID, in.Title, in.Subtitle, in.Preview, in.Content, in.Date, in.Thumbnail, rev)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrRevConflict // raced between the read and this CAS — refuse
		}
		if err := g.replaceTagsTx(ctx, tx, in.Tags, enqueuer); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM post_tags WHERE post_id = $1`, foundID); err != nil {
			return err
		}
		if err := linkTagsTx(ctx, tx, foundID, in.Tags); err != nil {
			return err
		}
		return enqueuer.InsertIndexPostTx(ctx, tx, foundID)
	})
	if err != nil {
		return AdminPostDetail{}, err
	}
	return g.GetAdminPost(ctx, slug)
}

// indexEnqueuer keeps store decoupled from river's generics.
type indexEnqueuer interface {
	InsertIndexPostTx(ctx context.Context, tx pgx.Tx, postID int64) error
	InsertIndexPhotoTx(ctx context.Context, tx pgx.Tx, photoID int64) error
	InsertIndexTagTx(ctx context.Context, tx pgx.Tx, tagID int64) error
}

// Publish is the §2 UNIT OF WORK: derivations (word_count/read_time from
// FINAL content, preview default = first paragraph, rank assignment,
// content_hash for idempotence) + state machine + Postgres row + index job
// — one transaction, re-publishing unchanged input is a no-op (byte-stable
// outputs; deterministic caches depend on it).
// PublishSource is the publish DECISION's input — the row facts a service
// needs to decide + derive. Reading belongs here; deciding does not.
type PublishSource struct {
	ID         int64
	Status     string
	Content    string
	Preview    string
	StoredHash string // "" until a first publish ever wrote a hash
}

func (g *PG) PublishSource(ctx context.Context, slug string) (PublishSource, error) {
	var src PublishSource
	var hash *string
	err := g.pool.QueryRow(ctx,
		`SELECT id, status::text, content_md, preview, content_hash
		 FROM posts WHERE slug = $1 AND deleted_at IS NULL`, slug).
		Scan(&src.ID, &src.Status, &src.Content, &src.Preview, &hash)
	if err == pgx.ErrNoRows {
		return src, ErrNoPosts
	}
	if hash != nil {
		src.StoredHash = *hash
	}
	return src, err
}

// PublishDerivation is the computed half of the §2 unit of work — prepared
// by adminsvc's PURE functions, persisted here without recomputation: one
// derivation implementation, unit-testable without any DB.
type PublishDerivation struct {
	WordCount int
	ReadTime  int
	Preview   string
	Hash      string
}

// ApplyPublish is the persistence half: state + derived fields + tag counts
// + the index job, ONE transaction (the atomicity river was chosen for).
func (g *PG) ApplyPublish(ctx context.Context, slug string, src PublishSource, d PublishDerivation, enqueuer indexEnqueuer) (AdminPostDetail, error) {
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE posts SET status = 'published', word_count = $2, read_time = $3,
			              preview = $4, content_hash = $5, rank = (SELECT COALESCE(MAX(rank),0)+1 FROM posts WHERE deleted_at IS NULL),
			              published_at = COALESCE(published_at, now()), updated_at = now()
			 WHERE id = $1`,
			src.ID, d.WordCount, d.ReadTime, d.Preview, d.Hash); err != nil {
			return err
		}
		// tag cloud counts are publish-maintained (§1: NEVER aggregated at read)
		if err := recomputeTagCountsTx(ctx, tx); err != nil {
			return err
		}
		return enqueuer.InsertIndexPostTx(ctx, tx, src.ID)
	})
	if err != nil {
		return AdminPostDetail{}, err
	}
	return g.GetAdminPost(ctx, slug)
}

// recomputeTagCountsTx is TOTAL by design: every tag row takes the scalar
// count, so a tag whose last published post went unpublished (or died) is
// ZEROED — the join-shaped version silently kept ghost counts forever
// (rows missing from the subquery were never written; found while planning
// the tag desk, 2026-11-10). Scale makes the per-row subquery free; the
// rule is what matters: the count column is truth, always recomputed from
// it, at every publish/unpublish/delete.
func recomputeTagCountsTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
		UPDATE tags t SET posts = (
			SELECT count(*)::int
			FROM post_tags pt JOIN posts p ON p.id = pt.post_id
			WHERE pt.tag_id = t.id AND p.status = 'published' AND p.deleted_at IS NULL)`)
	return err
}

func (g *PG) UnpublishAdminPost(ctx context.Context, slug string, enqueuer indexEnqueuer) (AdminPostDetail, error) {
	var id int64
	var status string
	err := g.pool.QueryRow(ctx,
		`SELECT id, status::text FROM posts WHERE slug = $1 AND deleted_at IS NULL`, slug).
		Scan(&id, &status)
	if err == pgx.ErrNoRows {
		return AdminPostDetail{}, ErrNoPosts
	}
	if err != nil {
		return AdminPostDetail{}, err
	}
	if status != "published" {
		return AdminPostDetail{}, ErrBadTransition
	}
	err = pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE posts SET status='unpublished', updated_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		if err := recomputeTagCountsTx(ctx, tx); err != nil {
			return err
		}
		return enqueuer.InsertIndexPostTx(ctx, tx, id)
	})
	if err != nil {
		return AdminPostDetail{}, err
	}
	return g.GetAdminPost(ctx, slug)
}

// DeleteAdminPost soft-deletes (§8: row hidden, media retained for the GC
// window). The index worker re-reads truth, finds nothing alive, and deletes
// the DOC — convergence by construction.
func (g *PG) DeleteAdminPost(ctx context.Context, slug string, enqueuer indexEnqueuer) error {
	var id int64
	err := g.pool.QueryRow(ctx,
		`SELECT id FROM posts WHERE slug = $1 AND deleted_at IS NULL`, slug).Scan(&id)
	if err == pgx.ErrNoRows {
		return nil // already gone; DELETE is idempotent
	}
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE posts SET deleted_at = now(), updated_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		if err := recomputeTagCountsTx(ctx, tx); err != nil {
			return err
		}
		return enqueuer.InsertIndexPostTx(ctx, tx, id)
	})
}
