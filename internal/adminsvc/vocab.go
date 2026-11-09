package adminsvc

import (
	"context"

	"github.com/kanatabi/lumen/internal/store"
)

func (s *Service) ListTags(ctx context.Context) ([]store.TagRowOut, error) {
	return s.store.ListAdminTags(ctx)
}
func (s *Service) CreateTag(ctx context.Context, name string) (store.TagRowOut, error) {
	return s.store.CreateAdminTag(ctx, name)
}

// UpdateTag applies the patch: rename first (later steps work on the NEW
// name), then feature. A name-only or flag-only patch is the common case;
// both fields in one call land atomically-enough at this scale (two store
// ops, single operator — the desk re-reads the row after anyway).
func (s *Service) UpdateTag(ctx context.Context, name string, p store.TagPatch) (store.TagRowOut, error) {
	cur, out := name, store.TagRowOut{}
	var err error
	if p.Name != nil {
		if out, err = s.store.RenameAdminTag(ctx, cur, *p.Name); err != nil {
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
	return s.store.DeleteAdminTag(ctx, name)
}

func (s *Service) ListCollections(ctx context.Context) ([]store.CollectionRowOut, error) {
	return s.store.ListAdminCollections(ctx)
}
func (s *Service) CreateCollection(ctx context.Context, name string) (store.CollectionRowOut, error) {
	return s.store.CreateAdminCollection(ctx, name)
}
