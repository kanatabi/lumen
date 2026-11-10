package adminsvc

import (
	"context"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/search"
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
// (drafts included, photos unfiltered by gallery). The tags group rides the
// engine too since the 2026-11-10 ruling — matching is one story everywhere
// (typo tolerance included); the superseded doctrine ("a name, not a
// document") held while the only engine-side option was a prefix lookup,
// and {id, name} docs + DB hydration now give fuzzy names WITHOUT letting
// the index own any fact.
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
	photoPage, err := s.engine.AdminPhotosByQuery(ctx, search.PhotoQuery{Q: q, Limit: 8})
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
	tagPage, err := s.engine.SearchTags(ctx, q, 0, 8)
	if err != nil {
		return out, err
	}
	tags, err := s.store.HydrateTagsByIDs(ctx, tagPage.IDs)
	if err != nil {
		return out, err
	}
	out.Tags = tags
	return out, nil
}
