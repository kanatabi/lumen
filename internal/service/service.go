// Package service orchestrates the §1 read path under the serving model
// (owner ruling, 2026-11-08): the ENGINE answers lists, facets and
// relevance; Postgres is the source of truth that results HYDRATE from —
// never a query engine for search, never a fallback. Every public surface
// re-checks truth at the hydration edge (status/gallery), so a lagging
// index can at worst omit a row that just committed — never leak one.
//
// Both seams are interfaces: production wires search.Service (Meilisearch)
// + store.PG (truth); the offline twin answers with the mock's own
// semantics (store.Mem) so CI keeps byte-parity without a container.
package service

import (
	"context"
	"sort"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/store"
)

// Engine is the serving-query seam (implemented by *search.Service and,
// offline, by store.Mem's twin).
type Engine interface {
	// ListPosts: facets + explicit/curation sorts; a relevance engine MAY
	// honor q.Q inside (Meilisearch does — the real engine serves both faces
	// from one call; the twin keeps list semantics pure).
	ListPosts(ctx context.Context, q domain.ListQuery) (search.ListPage, error)
	// SearchPosts: the relevance face — required explicitly so the twin's
	// mock-scoring and the engine's ranking are both honest implementations.
	SearchPosts(ctx context.Context, q domain.ListQuery) (search.ListPage, error)
	SuggestPosts(ctx context.Context, needle string, limit int) (search.ListPage, error)
	ListPhotos(ctx context.Context, limit int) (search.PhotoPage, error)
	// TagCounts: the PUBLISHED-post count per tag name (the engine's facet
	// distribution; the twin derives from its corpus). The count's only
	// home — the counter column died, design amendment 2026-11-15.
	TagCounts(ctx context.Context) (map[string]int64, error)
}

// Truth is the hydration + point-read seam (store.PG online, store.Mem twin —
// the existing Source shape remains the hydration contract).
type Service struct {
	truth  store.Source // truth reads: point lookups, hydration, tag cloud
	engine Engine       // serving queries: lists, facets, relevance
}

func New(truth store.Source, engine Engine) *Service { return &Service{truth: truth, engine: engine} }

func (s *Service) listPostSummaries(ctx context.Context, slugs []string) ([]domain.Summary, error) {
	all, err := s.truth.PublishedPosts(ctx)
	if err != nil {
		return nil, err
	}
	bySlug := make(map[string]domain.Summary, len(all))
	for _, d := range all {
		bySlug[d.Slug] = summarize(d)
	}
	out := make([]domain.Summary, 0, len(slugs))
	for _, slug := range slugs { // engine order preserved; hydration misses DROPPED
		if sum, ok := bySlug[slug]; ok { // (truth re-check: gone or un-published = no serve)
			out = append(out, sum)
		}
	}
	return out, nil
}

func (s *Service) GetPosts(ctx context.Context, q domain.ListQuery) (domain.ListResult, error) {
	page, err := s.engine.ListPosts(ctx, q)
	if err != nil {
		return domain.ListResult{}, err
	}
	items, err := s.listPostSummaries(ctx, page.Slugs)
	if err != nil {
		return domain.ListResult{}, err
	}
	return domain.ListResult{Total: int(page.Total), Items: items}, nil
}

// GetPost: a point read — truth direct (the §1 rule is about LISTING; a
// single slug is a primary lookup, not a search).
func (s *Service) GetPost(ctx context.Context, slug string) (domain.Detail, error) {
	all, err := s.truth.PublishedPosts(ctx)
	if err != nil {
		return domain.Detail{}, err
	}
	for _, d := range all {
		if d.Slug == slug {
			out := d
			if out.Tags == nil {
				out.Tags = []string{}
			}
			return out, nil
		}
	}
	return domain.Detail{}, store.ErrNotFound
}

func (s *Service) SuggestPosts(ctx context.Context, q string, limit int) (domain.SuggestResult, error) {
	page, err := s.engine.SuggestPosts(ctx, q, limit)
	if err != nil {
		return domain.SuggestResult{}, err
	}
	items, err := s.listPostSummaries(ctx, page.Slugs)
	if err != nil {
		return domain.SuggestResult{}, err
	}
	return domain.SuggestResult{Results: items}, nil
}

// SearchPosts: relevance is ENGINE-owned (the mock twin's ordering is a
// fixture courtesy, not the spec — contract assertions moved to rules for
// this surface). Empty q short-circuits before the call.
func (s *Service) SearchPosts(ctx context.Context, q domain.ListQuery) (domain.ListResult, error) {
	if q.Q == "" {
		return domain.ListResult{Total: 0, Items: []domain.Summary{}}, nil
	}
	page, err := s.engine.SearchPosts(ctx, q)
	if err != nil {
		return domain.ListResult{}, err
	}
	items, err := s.listPostSummaries(ctx, page.Slugs)
	if err != nil {
		return domain.ListResult{}, err
	}
	return domain.ListResult{Total: int(page.Total), Items: items}, nil
}

func (s *Service) GetPhotos(ctx context.Context, limit int) (domain.PhotoList, error) {
	page, err := s.engine.ListPhotos(ctx, limit)
	if err != nil {
		return domain.PhotoList{}, err
	}
	all, err := s.truth.Photos(ctx)
	if err != nil {
		return domain.PhotoList{}, err
	}
	byID := make(map[int64]domain.Photo, len(all))
	for _, p := range all {
		if p.Gallery { // truth re-check at the hydration edge — the engine
			b := p // filter can lag, §1 cannot
			b.Gallery, b.Featured, b.Sort = false, false, 0
			byID[p.ID] = b
		}
	}
	items := make([]domain.Photo, 0, len(page.IDs))
	for _, id := range page.IDs {
		if p, ok := byID[id]; ok {
			items = append(items, p)
		}
	}
	return domain.PhotoList{Total: int(page.Total), Items: items}, nil
}

// FeaturedPhoto: curation point read from truth (which photo is featured is
// a fact, not a search result).
func (s *Service) FeaturedPhoto(ctx context.Context) (domain.Photo, error) {
	all, err := s.truth.Photos(ctx)
	if err != nil {
		return domain.Photo{}, err
	}
	for _, p := range all {
		if p.Featured {
			return domain.Photo{ID: p.ID, Src: p.Src, Alt: p.Alt, Width: p.Width, Height: p.Height}, nil
		}
	}
	return domain.Photo{}, store.ErrNotFound
}

// ListTags: the Topics widget's feed. The DB owns the LIST (featured
// names); the engine owns the COUNTS (the published facet — merged here,
// filtered to count > 0: a tag at zero is ready, not display-worthy).
// Sorted count desc, name ASC (the documented tie order). Pure-engine:
// the engine down = visible 503, no truth-side fallback.
func (s *Service) ListTags(ctx context.Context) ([]domain.Tag, error) {
	names, err := s.truth.FeaturedTagNames(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.engine.TagCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Tag, 0, len(names))
	for _, name := range names {
		if c := counts[name]; c > 0 {
			out = append(out, domain.Tag{Name: name, Posts: int(c)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Posts != out[j].Posts {
			return out[i].Posts > out[j].Posts
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func summarize(d domain.Detail) domain.Summary {
	return domain.Summary{
		Slug: d.Slug, Title: d.Title, Subtitle: d.Subtitle, Preview: d.Preview,
		Date: d.Date, ReadTime: d.ReadTime, Thumbnail: d.Thumbnail,
	}
}
