package store

import (
	"context"
	"strings"
)

// ---- tags: the vocabulary (first-class store — exists from creation,
// used or not; §8 semantics identical to collections) ----

func (g *PG) ListAdminTags(ctx context.Context) ([]TagRowOut, error) {
	rows, err := g.pool.Query(ctx, `SELECT name, posts FROM tags ORDER BY posts DESC, name ASC`)
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

type TagRowOut struct {
	Name  string `json:"name"`
	Posts int    `json:"posts"`
}

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
	if _, err := g.pool.Exec(ctx, `INSERT INTO tags(name, posts, cloud) VALUES ($1, 0, FALSE)`, name); err != nil {
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
