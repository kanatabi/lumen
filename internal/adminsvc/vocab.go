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
func (s *Service) ListCollections(ctx context.Context) ([]store.CollectionRowOut, error) {
	return s.store.ListAdminCollections(ctx)
}
func (s *Service) CreateCollection(ctx context.Context, name string) (store.CollectionRowOut, error) {
	return s.store.CreateAdminCollection(ctx, name)
}
