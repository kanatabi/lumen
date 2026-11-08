package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kanatabi/lumen/internal/search"
)

// AdminPhoto mirrors mellon's AdminPhoto — id is a STRING on the wire (the
// contract), BIGSERIAL here (the key); serialization bridges.
type AdminPhoto struct {
	ID         string  `json:"id"`
	Src        string  `json:"src"`
	Alt        string  `json:"alt"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Gallery    bool    `json:"gallery"`
	Collection *string `json:"collection"`
	CreatedAt  string  `json:"createdAt"`
}

type PhotoDocRow struct {
	Doc     search.PhotoDoc
	Created time.Time
}

func (r PhotoDocRow) Admin() AdminPhoto {
	return AdminPhoto{
		ID:  strconv.FormatInt(r.Doc.ID, 10),
		Src: r.Doc.Src, Alt: r.Doc.Alt, Width: r.Doc.Width, Height: r.Doc.Height,
		Gallery: r.Doc.Gallery, Collection: r.Doc.Collection, CreatedAt: iso(r.Created),
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

const photoCols = `id, src, alt, width, height, gallery, featured, sort, collection, created_at`

func scanPhotoRow(row pgx.Row) (PhotoDocRow, error) {
	var r PhotoDocRow
	err := row.Scan(&r.Doc.ID, &r.Doc.Src, &r.Doc.Alt, &r.Doc.Width, &r.Doc.Height,
		&r.Doc.Gallery, &r.Doc.Featured, &r.Doc.Sort, &r.Doc.Collection, &r.Created)
	return r, err
}

type PhotoListOpts struct {
	Collections []string
	Gallery     *bool
	From        string
	To          string
	Sort        string // created|title(alt)|collection — §8 whitelist
	Dir         string
	Page        int
	PageSize    int
}

// ListAdminPhotos: the darkroom envelope (§8: pageSize 24, inclusive
// created_at windows, sort whitelist, page clamp — mellon's contract).
func (g *PG) ListAdminPhotos(ctx context.Context, o PhotoListOpts) (int, []AdminPhoto, error) {
	where := []string{"TRUE"}
	args := []any{}
	n := 0
	bind := func(v any) int { n++; args = append(args, v); return n }
	if len(o.Collections) > 0 {
		var others []string
		hasUncat := false
		for _, c := range o.Collections {
			if c == "uncategorized" {
				hasUncat = true
			} else {
				others = append(others, c)
			}
		}
		conds := []string{}
		if len(others) > 0 {
			conds = append(conds, "collection = ANY($"+itoa(bind(others))+")")
		}
		if hasUncat {
			conds = append(conds, "collection IS NULL")
		}
		where = append(where, "("+strings.Join(conds, " OR ")+")")
	}
	if o.Gallery != nil {
		where = append(where, "gallery = $"+itoa(bind(*o.Gallery)))
	}
	if o.From != "" {
		where = append(where, "created_at >= $"+itoa(bind(o.From))+"::date")
	}
	if o.To != "" {
		where = append(where, "created_at < ($"+itoa(bind(o.To))+"::date + interval '1 day')")
	}
	sortCol := map[string]string{"created": "created_at", "title": "lower(alt)", "collection": "collection"}[o.Sort]
	if sortCol == "" {
		sortCol = "created_at"
	}
	dir := "DESC"
	if strings.EqualFold(o.Dir, "asc") {
		dir = "ASC"
	}
	pageSize := o.PageSize
	if pageSize <= 0 {
		pageSize = 24
	}
	qs := strings.Join(where, " AND ")

	var total int
	if err := g.pool.QueryRow(ctx, `SELECT count(*) FROM photos WHERE `+qs, args...).Scan(&total); err != nil {
		return 0, nil, err
	}
	totalPages := max(1, (total+pageSize-1)/pageSize)
	page := min(max(o.Page, 1), totalPages) // clamp, never empty-error
	rows, err := g.pool.Query(ctx,
		`SELECT `+photoCols+` FROM photos WHERE `+qs+
			` ORDER BY `+sortCol+` `+dir+`, id ASC LIMIT $`+itoa(len(args)+1)+` OFFSET $`+itoa(len(args)+2),
		append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	out := []AdminPhoto{}
	for rows.Next() {
		r, err := scanPhotoRow(rows)
		if err != nil {
			return 0, nil, err
		}
		out = append(out, r.Admin())
	}
	return total, out, rows.Err()
}

func (g *PG) GetAdminPhotoRow(ctx context.Context, id int64) (PhotoDocRow, error) {
	r, err := scanPhotoRow(g.pool.QueryRow(ctx,
		`SELECT `+photoCols+` FROM photos WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNoPhoto
	}
	return r, err
}

type PhotoRef struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

// ListPhotoReferences: the content scan feeding the delete dialog's
// warn-list (§8 — warn with full information, never block).
func (g *PG) ListPhotoReferences(ctx context.Context, id int64) ([]PhotoRef, error) {
	var src string
	if err := g.pool.QueryRow(ctx, `SELECT src FROM photos WHERE id = $1`, id).Scan(&src); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoPhoto
		}
		return nil, err
	}
	rows, err := g.pool.Query(ctx,
		`SELECT slug, title FROM posts
		 WHERE deleted_at IS NULL AND content_md LIKE '%' || $1 || '%'
		 ORDER BY rank ASC, slug ASC`, src)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PhotoRef{}
	for rows.Next() {
		var r PhotoRef
		if err := rows.Scan(&r.Slug, &r.Title); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type PhotoPatch struct {
	Alt        *string
	Collection **string // pointer-to-pointer: JSON null clears, absent skips
	Gallery    *bool
}

// UpdateAdminPhoto: curation metadata (§8 PATCH; last-write-wins — photos
// carry no rev, single-author album truth). Unknown collection → 400.
func (g *PG) UpdateAdminPhoto(ctx context.Context, id int64, in PhotoPatch, qc indexEnqueuer) (PhotoDocRow, error) {
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM photos WHERE id = $1 FOR UPDATE`, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNoPhoto
			}
			return err
		}
		if in.Alt != nil {
			if _, err := tx.Exec(ctx, `UPDATE photos SET alt = $2 WHERE id = $1`, id, *in.Alt); err != nil {
				return err
			}
		}
		if in.Collection != nil {
			if *in.Collection != nil {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM collections WHERE name = $1)`,
					**in.Collection).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					return ErrUnknownColl
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE photos SET collection = $2 WHERE id = $1`, id, *in.Collection); err != nil {
				return err
			}
		}
		if in.Gallery != nil {
			if _, err := tx.Exec(ctx, `UPDATE photos SET gallery = $2 WHERE id = $1`, id, *in.Gallery); err != nil {
				return err
			}
		}
		return qc.InsertIndexPhotoTx(ctx, tx, id)
	})
	if err != nil {
		return PhotoDocRow{}, err
	}
	return g.GetAdminPhotoRow(ctx, id)
}

// DeleteAdminPhoto removes the LIBRARY ROW; the object outlives it until
// storage GC (§8). The worker's truth-read finds the row gone and deletes
// the DOC — convergence by construction.
func (g *PG) DeleteAdminPhoto(ctx context.Context, id int64, qc indexEnqueuer) error {
	return pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM photos WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // idempotent
		}
		return qc.InsertIndexPhotoTx(ctx, tx, id)
	})
}
