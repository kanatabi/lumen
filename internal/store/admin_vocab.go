package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ---- tags: the vocabulary (first-class store — exists from creation,
// used or not; §8 semantics identical to collections) ----

func (g *PG) ListAdminTags(ctx context.Context) ([]TagRowOut, error) {
	rows, err := g.pool.Query(ctx, `SELECT name, posts, featured FROM tags ORDER BY posts DESC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagRowOut{}
	for rows.Next() {
		var t TagRowOut
		if err := rows.Scan(&t.Name, &t.Posts, &t.Featured); err != nil {
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
	Featured bool `json:"featured"`
}

// TagPatch is the desk's single verb: rename and/or feature, either field
// alone or together (one round-trip per row edit is all the UI should need).
type TagPatch struct {
	Name     *string `json:"name"`
	Featured *bool   `json:"featured"`
}

func (g *PG) GetAdminTag(ctx context.Context, name string) (TagRowOut, error) {
	var t TagRowOut
	err := g.pool.QueryRow(ctx, `SELECT name, posts, featured FROM tags WHERE name = $1`, name).
		Scan(&t.Name, &t.Posts, &t.Featured)
	if errors.Is(err, pgx.ErrNoRows) {
		return TagRowOut{}, ErrUnknownVocab
	}
	return t, err
}

// RenameAdminTag: names are the slug; the FK web hangs off id, so renaming
// PROPAGATES to every post referencing the tag for free (the desk shows the
// new name on the post without further surgery). Collisions refuse; an
// identity rename is a no-op read.
func (g *PG) RenameAdminTag(ctx context.Context, old, name string) (TagRowOut, error) {
	name = Slugify(name)
	if name == "" {
		return TagRowOut{}, ErrBadName
	}
	cur, err := g.GetAdminTag(ctx, old)
	if err != nil {
		return TagRowOut{}, err
	}
	if name == old {
		return cur, nil
	}
	var clash bool
	if err := g.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tags WHERE name = $1)`, name).Scan(&clash); err != nil {
		return TagRowOut{}, err
	}
	if clash {
		return TagRowOut{}, ErrDupVocab
	}
	if _, err := g.pool.Exec(ctx, `UPDATE tags SET name = $1 WHERE name = $2`, name, old); err != nil {
		return TagRowOut{}, err
	}
	cur.Name = name
	return cur, nil
}

func (g *PG) SetTagFeatured(ctx context.Context, name string, featured bool) (TagRowOut, error) {
	tag, err := g.GetAdminTag(ctx, name)
	if err != nil {
		return TagRowOut{}, err
	}
	if _, err := g.pool.Exec(ctx, `UPDATE tags SET featured = $1 WHERE name = $2`, featured, name); err != nil {
		return TagRowOut{}, err
	}
	tag.Featured = featured
	return tag, nil
}

// DeleteAdminTag refuses while ANY live post (draft or published) references
// the tag — the unassignment is the editor's job, not a side effect of a
// delete button. Soft-deleted posts do not hold a tag hostage. The wrapped
// message carries the COUNT because "used" without a number makes the owner
// hunt through the desk to understand a refusal.
func (g *PG) DeleteAdminTag(ctx context.Context, name string) error {
	var id int64
	err := g.pool.QueryRow(ctx, `SELECT id FROM tags WHERE name = $1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnknownVocab
	}
	if err != nil {
		return err
	}
	var used int
	if err := g.pool.QueryRow(ctx, `
		SELECT count(*)::int FROM post_tags pt JOIN posts p ON p.id = pt.post_id
		WHERE pt.tag_id = $1 AND p.deleted_at IS NULL`, id).Scan(&used); err != nil {
		return err
	}
	if used > 0 {
		return fmt.Errorf("%w — %d posts still reference it", ErrTagInUse, used)
	}
	_, err = g.pool.Exec(ctx, `DELETE FROM tags WHERE id = $1`, id)
	return err
}

// Desk sentinels (handlers map: 400/404/409 — one vocabulary with the
// existing ErrBadName/ErrDupVocab).
var (
	ErrUnknownVocab = errStatic("vocabulary row not found")
	ErrTagInUse     = errStatic("tag is in use")
)

func (g *PG) CreateAdminTag(ctx context.Context, name string) (TagRowOut, error) {
	name = Slugify(name)
	if name == "" {
		return TagRowOut{}, ErrBadName
	}
	var exists bool
	if err := g.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tags WHERE name = $1)`, name).Scan(&exists); err != nil {
		return TagRowOut{}, err
	}
	if exists {
		return TagRowOut{}, ErrDupVocab
	}
	if _, err := g.pool.Exec(ctx, `INSERT INTO tags(name, posts, featured) VALUES ($1, 0, FALSE)`, name); err != nil {
		return TagRowOut{}, err
	}
	return TagRowOut{Name: name, Posts: 0}, nil
}

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

// ---- admin search: the tags GROUP is a vocabulary PREFIX lookup (lumen
// §4/§8: a name, not a document — deliberately not the search engine) ----

func (g *PG) SearchTagNames(ctx context.Context, q string, limit int) ([]TagRowOut, error) {
	rows, err := g.pool.Query(ctx,
		`SELECT name, posts FROM tags WHERE name LIKE $1
		 ORDER BY posts DESC, name ASC LIMIT $2`,
		strings.ToLower(q)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagRowOut{}
	for rows.Next() {
		var t TagRowOut
		if err := rows.Scan(&t.Name, &t.Posts); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

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
