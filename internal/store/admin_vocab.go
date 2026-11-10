package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kanatabi/lumen/internal/search"
)

// ---- tags: the vocabulary (first-class store — exists from creation,
// used or not; §8 semantics identical to collections).
//
// RULING 2026-11-10 (owner): tag MATCHING moves to the Meilisearch engine —
// docs {id, name}, typo tolerance as one story everywhere. Counts and
// `featured` are NOT indexed: they hydrate from this truth on every
// response, so the engine can never drift the vocabulary's facts. Every tag
// mutation is a transaction that enqueues its index_tag intent — rows and
// docs commit together, the §2 contract, same as posts. The old Postgres
// prefix lookup (SearchTagNames) is retired.

const tagCols = `name, posts, featured, created_at, updated_at`

func (g *PG) ListAdminTags(ctx context.Context, order string) ([]TagRowOut, error) {
	sortSQL := `ORDER BY posts DESC, name ASC`
	if order == "newest" {
		sortSQL = `ORDER BY created_at DESC, name ASC`
	}
	rows, err := g.pool.Query(ctx, `SELECT `+tagCols+` FROM tags `+sortSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagRowOut{}
	for rows.Next() {
		var t TagRowOut
		if err := rows.Scan(&t.Name, &t.Posts, &t.Featured, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type TagRowOut struct {
	Name  string `json:"name"`
	Posts int    `json:"posts"`
	// Featured: the desk's manual promotion flag (was `cloud` until 0005) —
	// membership in the earendil TOPICS widget. Vocabulary exists without it.
	Featured  bool      `json:"featured"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TagPatch is the desk's single verb: rename and/or feature.
type TagPatch struct {
	Name     *string `json:"name"`
	Featured *bool   `json:"featured"`
}

func scanTag(row pgx.Row) (TagRowOut, error) {
	var t TagRowOut
	err := row.Scan(&t.Name, &t.Posts, &t.Featured, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func (g *PG) GetAdminTag(ctx context.Context, name string) (TagRowOut, error) {
	t, err := scanTag(g.pool.QueryRow(ctx, `SELECT `+tagCols+` FROM tags WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return TagRowOut{}, ErrUnknownVocab
	}
	return t, err
}

// HydrateTagsByIDs serves engine-matched ids from truth, PRESERVING the
// engine's relevance order (the desk's search page reads ranked). Ids with
// no row (deleted mid-flight) are simply absent — convergence, not error.
func (g *PG) HydrateTagsByIDs(ctx context.Context, ids []int64) ([]TagRowOut, error) {
	if len(ids) == 0 {
		return []TagRowOut{}, nil
	}
	rows, err := g.pool.Query(ctx, `SELECT id, `+tagCols+` FROM tags WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]TagRowOut{}
	for rows.Next() {
		var id int64
		var t TagRowOut
		if err := rows.Scan(&id, &t.Name, &t.Posts, &t.Featured, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		byID[id] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]TagRowOut, 0, len(ids))
	for _, id := range ids {
		if t, ok := byID[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// TagDocByID + ListAllTagDocs: the projection the queue worker reads at RUN
// time (truth-at-execution, the index_post contract) and the boot/seed
// backfill.
func (g *PG) TagDocByID(ctx context.Context, id int64) (search.TagDoc, bool, error) {
	var d search.TagDoc
	err := g.pool.QueryRow(ctx, `SELECT id, name FROM tags WHERE id = $1`, id).Scan(&d.ID, &d.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return search.TagDoc{}, false, nil
	}
	return d, err == nil, err
}

// AllTagIDs: the seed's reconcile loop iterates ids and lets the WORKER
// project the doc at run time — same pattern as AllPostIDs.
func (g *PG) AllTagIDs(ctx context.Context) ([]int64, error) {
	rows, err := g.pool.Query(ctx, `SELECT id FROM tags ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (g *PG) ListAllTagDocs(ctx context.Context) ([]search.TagDoc, error) {
	rows, err := g.pool.Query(ctx, `SELECT id, name FROM tags ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []search.TagDoc{}
	for rows.Next() {
		var d search.TagDoc
		if err := rows.Scan(&d.ID, &d.Name); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreateAdminTag: slugified insert + the doc intent, one tx.
func (g *PG) CreateAdminTag(ctx context.Context, name string, enqueuer indexEnqueuer) (TagRowOut, error) {
	name = Slugify(name)
	if name == "" {
		return TagRowOut{}, ErrBadName
	}
	var out TagRowOut
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tags WHERE name = $1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrDupVocab
		}
		var id int64
		var t TagRowOut
		if err := tx.QueryRow(ctx,
			`INSERT INTO tags(name, posts, featured) VALUES ($1, 0, FALSE)
			 RETURNING id, `+tagCols, name).
			Scan(&id, &t.Name, &t.Posts, &t.Featured, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return err
		}
		if err := enqueuer.InsertIndexTagTx(ctx, tx, id); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

// RenameAdminTag: names are the slug; the FK web hangs off id, so the row
// change PROPAGATES to every referencing post for free — but post DOCS
// embed tag NAMES (searchable + facet), so the rename fans out index_post
// jobs inside the same tx, and the tag doc itself re-indexes. Collision
// refuses; identity rename is a no-op read.
func (g *PG) RenameAdminTag(ctx context.Context, old, name string, enqueuer indexEnqueuer) (TagRowOut, error) {
	name = Slugify(name)
	if name == "" {
		return TagRowOut{}, ErrBadName
	}
	var out TagRowOut
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `SELECT id FROM tags WHERE name = $1 FOR UPDATE`, old).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUnknownVocab
			}
			return err
		}
		if name == old {
			var err error
			out, err = scanTag(tx.QueryRow(ctx, `SELECT `+tagCols+` FROM tags WHERE id = $1`, id))
			return err
		}
		var clash bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tags WHERE name = $1 AND id <> $2)`, name, id).Scan(&clash); err != nil {
			return err
		}
		if clash {
			return ErrDupVocab
		}
		if _, err := tx.Exec(ctx, `UPDATE tags SET name = $1, updated_at = now() WHERE id = $2`, name, id); err != nil {
			return err
		}
		if err := enqueuer.InsertIndexTagTx(ctx, tx, id); err != nil {
			return err
		}
		prows, err := tx.Query(ctx, `
			SELECT pt.post_id FROM post_tags pt JOIN posts p ON p.id = pt.post_id
			WHERE pt.tag_id = $1 AND p.deleted_at IS NULL`, id)
		if err != nil {
			return err
		}
		var postIDs []int64
		for prows.Next() {
			var pid int64
			if err := prows.Scan(&pid); err != nil {
				prows.Close()
				return err
			}
			postIDs = append(postIDs, pid)
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			return err
		}
		for _, pid := range postIDs {
			if err := enqueuer.InsertIndexPostTx(ctx, tx, pid); err != nil {
				return err
			}
		}
		out, err = scanTag(tx.QueryRow(ctx, `SELECT `+tagCols+` FROM tags WHERE id = $1`, id))
		return err
	})
	return out, err
}

// SetTagFeatured: the desk's promotion toggle. No engine intent needed —
// featured is NOT in the doc; it hydrates from here. updated_at marks any
// desk touch (owner ruling: every update stamps it).
func (g *PG) SetTagFeatured(ctx context.Context, name string, featured bool) (TagRowOut, error) {
	tag, err := g.GetAdminTag(ctx, name)
	if err != nil {
		return TagRowOut{}, err
	}
	if _, err := g.pool.Exec(ctx, `UPDATE tags SET featured = $1, updated_at = now() WHERE name = $2`, featured, name); err != nil {
		return TagRowOut{}, err
	}
	tag.Featured = featured
	return tag, nil
}

// BulkSetFeatured returns the number of rows that ACTUALLY changed
// (IS DISTINCT FROM) — "updated: 3" means three, not "you sent three".
func (g *PG) BulkSetFeatured(ctx context.Context, names []string, featured bool) (int, error) {
	if len(names) == 0 {
		return 0, nil
	}
	res, err := g.pool.Exec(ctx, `
		UPDATE tags SET featured = $1, updated_at = now()
		WHERE name = ANY($2) AND featured IS DISTINCT FROM $1`, featured, names)
	if err != nil {
		return 0, err
	}
	return int(res.RowsAffected()), nil
}

// DeleteAdminTag refuses while ANY live post (draft or published) references
// the tag; the wrapped message carries the COUNT. The delete enqueues its
// index_tag intent — the worker finds the row GONE and deletes the doc, so
// one job kind converges both directions.
func (g *PG) DeleteAdminTag(ctx context.Context, name string, enqueuer indexEnqueuer) error {
	return pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `SELECT id FROM tags WHERE name = $1 FOR UPDATE`, name).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUnknownVocab
			}
			return err
		}
		var used int
		if err := tx.QueryRow(ctx, `
			SELECT count(*)::int FROM post_tags pt JOIN posts p ON p.id = pt.post_id
			WHERE pt.tag_id = $1 AND p.deleted_at IS NULL`, id).Scan(&used); err != nil {
			return err
		}
		if used > 0 {
			return fmt.Errorf("%w — %d posts still reference it", ErrTagInUse, used)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tags WHERE id = $1`, id); err != nil {
			return err
		}
		return enqueuer.InsertIndexTagTx(ctx, tx, id)
	})
}

// Desk sentinels (handlers map: 400/404/409 — one vocabulary with the
// existing ErrBadName/ErrDupVocab).
var (
	ErrUnknownVocab = errStatic("vocabulary row not found")
	ErrTagInUse     = errStatic("tag is in use")
)

// ---- collections: curation vocabulary, admin-internal ----

type CollectionRowOut struct {
	Name   string `json:"name"`
	Photos int    `json:"photos"`
}

func (g *PG) ListAdminCollections(ctx context.Context) ([]CollectionRowOut, error) {
	rows, err := g.pool.Query(ctx, `
		SELECT c.name, count(p.id)::int
		FROM collections c LEFT JOIN photos p ON p.collection = c.name
		GROUP BY c.name ORDER BY count(p.id) DESC, c.name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollectionRowOut{}
	for rows.Next() {
		var c CollectionRowOut
		if err := rows.Scan(&c.Name, &c.Photos); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (g *PG) CreateAdminCollection(ctx context.Context, name string) (CollectionRowOut, error) {
	name = Slugify(name)
	if name == "" {
		return CollectionRowOut{}, ErrBadName
	}
	var exists bool
	if err := g.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM collections WHERE name = $1)`, name).Scan(&exists); err != nil {
		return CollectionRowOut{}, err
	}
	if exists {
		return CollectionRowOut{}, ErrDupVocab
	}
	if _, err := g.pool.Exec(ctx, `INSERT INTO collections(name) VALUES ($1)`, name); err != nil {
		return CollectionRowOut{}, err
	}
	return CollectionRowOut{Name: name, Photos: 0}, nil
}

var ErrBadName = errStatic("name slugifies to nothing")

type errStatic string

func (e errStatic) Error() string { return string(e) }

// ---- hydration for the ADMIN door (drafts included, deleted excluded) ----

func (g *PG) AdminSummariesBySlugs(ctx context.Context, slugs []string) (map[string]AdminPostSummary, error) {
	if len(slugs) == 0 {
		return map[string]AdminPostSummary{}, nil
	}
	rows, err := g.pool.Query(ctx,
		`SELECT `+adminPostCols+` FROM posts WHERE slug = ANY($1) AND deleted_at IS NULL`, slugs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AdminPostSummary{}
	for rows.Next() {
		r, err := scanAdminPost(rows)
		if err != nil {
			return nil, err
		}
		out[r.slug] = r.summary()
	}
	return out, rows.Err()
}

func (g *PG) AdminPhotoRowsByIDs(ctx context.Context, ids []int64) (map[int64]AdminPhoto, error) {
	out := map[int64]AdminPhoto{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := g.pool.Query(ctx,
		`SELECT `+photoCols+` FROM photos WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanPhotoRow(rows)
		if err != nil {
			return nil, err
		}
		out[r.Doc.ID] = r.Admin()
	}
	return out, rows.Err()
}
