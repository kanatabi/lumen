package adminsvc

import (
	"context"
	"sort"

	"github.com/kanatabi/lumen/internal/store"
)

// ListTags: the full vocabulary for the desk and the editor comboboxes.
// The DB owns the LIST; the engine owns the COUNTS (the published facet —
// the counter column died, design amendment 2026-11-15; the counts merge
// here). order "" = count desc then name (the desk's curated order);
// "newest" = creation recency (the dashboard) — the DB's order stands,
// counts still filled (the desk's POSTS column reads on both orders).
func (s *Service) ListTags(ctx context.Context, order string) ([]store.TagRowOut, error) {
	rows, err := s.store.ListAdminTags(ctx, order)
	if err != nil {
		return nil, err
	}
	counts, err := s.engine.TagCounts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Posts = int(counts[rows[i].Name])
	}
	if order == "newest" {
		return rows, nil
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Posts != rows[j].Posts {
			return rows[i].Posts > rows[j].Posts
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, nil
}

func (s *Service) CreateTag(ctx context.Context, name string) (store.TagRowOut, error) {
	return s.store.CreateAdminTag(ctx, name, s.queue)
}

// TagPage: one page of engine-matched vocabulary + the full match count —
// the desk's search axis (ruling 2026-11-10: tag matching is the engine's,
// typo tolerance one story everywhere; counts/featured hydrate from truth).
type TagPage struct {
	Total int               `json:"total"`
	Items []store.TagRowOut `json:"items"`
}

func (s *Service) SearchTags(ctx context.Context, q string, page, pageSize int) (TagPage, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 30
	}
	tp, err := s.engine.SearchTags(ctx, q, (page-1)*pageSize, pageSize)
	if err != nil {
		return TagPage{}, err
	}
	items, err := s.store.HydrateTagsByIDs(ctx, tp.IDs)
	if err != nil {
		return TagPage{}, err
	}
	return TagPage{Total: tp.Total, Items: items}, nil
}

// BulkFeatureTags: the desk's multi-select promotion. No engine intent —
// featured is truth-side, never indexed.
func (s *Service) BulkFeatureTags(ctx context.Context, names []string, featured bool) (int, error) {
	return s.store.BulkSetFeatured(ctx, names, featured)
}

// UpdateTag applies the patch: rename first (later steps work on the NEW
// name), then feature. A name-only or flag-only patch is the common case;
// both fields in one call land atomically-enough at this scale (two store
// ops, single operator — the desk re-reads the row after anyway).
func (s *Service) UpdateTag(ctx context.Context, name string, p store.TagPatch) (store.TagRowOut, error) {
	cur, out := name, store.TagRowOut{}
	var err error
	if p.Name != nil {
		if out, err = s.store.RenameAdminTag(ctx, cur, *p.Name, s.queue); err != nil {
			return store.TagRowOut{}, err
		}
		cur = out.Name
	}
	if p.Featured != nil {
		if out, err = s.store.SetTagFeatured(ctx, cur, *p.Featured); err != nil {
			return store.TagRowOut{}, err
		}
		return out, nil
	}
	if p.Name != nil {
		return out, nil
	}
	return s.store.GetAdminTag(ctx, cur) // patch had no fields — the handler guards, this is the honest fallback
}

func (s *Service) DeleteTag(ctx context.Context, name string) error {
	return s.store.DeleteAdminTag(ctx, name, s.queue)
}

func (s *Service) ListCollections(ctx context.Context) ([]store.CollectionRowOut, error) {
	return s.store.ListAdminCollections(ctx)
}
func (s *Service) CreateCollection(ctx context.Context, name string) (store.CollectionRowOut, error) {
	return s.store.CreateAdminCollection(ctx, name)
}
