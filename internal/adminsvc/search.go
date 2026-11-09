package adminsvc

import (
	"context"
	"errors"

	"github.com/kanatabi/lumen/internal/bao"
	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/store"
)

// SearchResults is the palette's door shape (§8): three groups, drafts
// included, ≤8 each, no pagination — a finder, not a browse axis.
type SearchResults struct {
	Posts  []store.AdminPostSummary `json:"posts"`
	Photos []store.AdminPhoto       `json:"photos"`
	Tags   []store.TagRowOut        `json:"tags"`
}

// Search is the admin door: the SAME engine minus the public restrictions
// (drafts included, photos unfiltered by gallery). The tags group is a
// VOCABULARY prefix lookup from Postgres — a name, not a document.
func (s *Service) Search(ctx context.Context, q string) (SearchResults, error) {
	out := SearchResults{Posts: []store.AdminPostSummary{}, Photos: []store.AdminPhoto{}, Tags: []store.TagRowOut{}}
	if q == "" {
		return out, nil
	}
	postPage, err := s.engine.AdminPostsByQuery(ctx, domain.ListQuery{Q: q, Limit: 8})
	if err != nil {
		return out, err
	}
	bySlug, err := s.store.AdminSummariesBySlugs(ctx, postPage.Slugs)
	if err != nil {
		return out, err
	}
	for _, slug := range postPage.Slugs {
		if sum, ok := bySlug[slug]; ok {
			out.Posts = append(out.Posts, sum)
		}
	}
	photoPage, err := s.engine.AdminPhotosByQuery(ctx, q, 8)
	if err != nil {
		return out, err
	}
	photos, err := s.store.AdminPhotoRowsByIDs(ctx, photoPage.IDs)
	if err != nil {
		return out, err
	}
	for _, id := range photoPage.IDs {
		if p, ok := photos[id]; ok {
			out.Photos = append(out.Photos, p)
		}
	}
	tags, err := s.store.SearchTagNames(ctx, q, 8)
	if err != nil {
		return out, err
	}
	out.Tags = tags
	return out, nil
}

// ---- secrets plane (§9): four operations, bao errors translated to OUR
// sentinels so handlers import neither the client nor its error types ----

func (s *Service) SecretPaths(ctx context.Context) ([]bao.Path, error) {
	return s.bao.ListPaths(ctx)
}

func (s *Service) ReadSecret(ctx context.Context, app, env string, version int) (*bao.Payload, error) {
	return s.bao.Read(ctx, app, env, version)
}

func (s *Service) SecretMeta(ctx context.Context, app, env string) (*bao.Meta, error) {
	return s.bao.Meta(ctx, app, env)
}

func (s *Service) WriteSecret(ctx context.Context, app, env string, data map[string]string, cas int) (int, error) {
	version, err := s.bao.WriteCAS(ctx, app, env, data, cas)
	var baoErr *bao.Error
	if errors.As(err, &baoErr) && baoErr.Kind == bao.ErrCheckAndSet {
		return 0, ErrStaleCAS
	}
	return version, err
}
