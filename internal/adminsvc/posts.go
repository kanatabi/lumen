package adminsvc

import (
	"context"

	"github.com/kanatabi/lumen/internal/store"
)

// The desk pass-throughs: these ARE decisions of shape (which store call,
// which enqueuer), keeping handlers from importing store/queue at all.

func (s *Service) ListPosts(ctx context.Context, opts store.AdminListOpts) (int, []store.AdminPostSummary, error) {
	return s.store.ListAdminPosts(ctx, opts)
}
func (s *Service) GetPost(ctx context.Context, slug string) (store.AdminPostDetail, error) {
	return s.store.GetAdminPost(ctx, slug)
}
func (s *Service) CreatePost(ctx context.Context, in store.PostDraftInput) (store.AdminPostDetail, error) {
	return s.store.CreateAdminPost(ctx, in, s.queue)
}
func (s *Service) UpdatePost(ctx context.Context, slug string, in store.PostDraftInput, rev int) (store.AdminPostDetail, error) {
	return s.store.UpdateAdminPost(ctx, slug, in, rev, s.queue)
}
func (s *Service) Unpublish(ctx context.Context, slug string) (store.AdminPostDetail, error) {
	return s.store.UnpublishAdminPost(ctx, slug, s.queue)
}
func (s *Service) DeletePost(ctx context.Context, slug string) error {
	return s.store.DeleteAdminPost(ctx, slug, s.queue)
}
