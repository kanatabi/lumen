package adminsvc

import (
	"context"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/store"
)

// The desk pass-throughs: these ARE decisions of shape (which store call,
// which enqueuer), keeping handlers from importing store/queue at all.

func (s *Service) ListPosts(ctx context.Context, opts store.AdminListOpts) (int, []store.AdminPostSummary, error) {
	if opts.Q != "" {
		return s.deskSearchPosts(ctx, opts)
	}
	return s.store.ListAdminPosts(ctx, opts)
}

// deskSearchPosts: the ENGINE door for ?q (ruling 2026-11-10) — typo-
// tolerant across the WHOLE corpus, status/tags/window facets ride along,
// order becomes relevance by design (sort chips pause while searching),
// and total is the FULL match count so the pager tells the truth.
func (s *Service) deskSearchPosts(ctx context.Context, opts store.AdminListOpts) (int, []store.AdminPostSummary, error) {
	page := max(opts.Page, 1)
	size := opts.PageSize
	if size <= 0 {
		size = 12
	}
	lp, err := s.engine.AdminPostsByQuery(ctx, domain.ListQuery{
		Q: opts.Q, Status: opts.Status, Tags: opts.Tags,
		From: opts.From, To: opts.To,
		Offset: (page - 1) * size, Limit: size,
	})
	if err != nil {
		return 0, nil, err
	}
	bySlug, err := s.store.AdminSummariesBySlugs(ctx, lp.Slugs)
	if err != nil {
		return 0, nil, err
	}
	out := make([]store.AdminPostSummary, 0, len(lp.Slugs))
	for _, slug := range lp.Slugs {
		if sum, ok := bySlug[slug]; ok {
			out = append(out, sum)
		}
	}
	return int(lp.Total), out, nil
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
