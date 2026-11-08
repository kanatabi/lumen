// Package service shapes §1 responses from a Source using the ported domain
// predicates. This is THE place list semantics live (facets, sort, ranking,
// the suggest/search split, pagination) — one implementation shared by the
// offline harness and the real Postgres-backed server, which is how the
// contract stays one truth.
package service

import (
	"context"
	"sort"
	"strings"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/store"
)

type Service struct{ src store.Source }

func New(src store.Source) *Service { return &Service{src: src} }

func summarize(d domain.Detail) domain.Summary {
	return domain.Summary{
		Slug: d.Slug, Title: d.Title, Subtitle: d.Subtitle, Preview: d.Preview,
		Date: d.Date, ReadTime: d.ReadTime, Thumbnail: d.Thumbnail,
	}
}

func (s *Service) GetPosts(ctx context.Context, q domain.ListQuery) (domain.ListResult, error) {
	all, err := s.src.PublishedPosts(ctx)
	if err != nil {
		return domain.ListResult{}, err
	}
	var kept []domain.Summary
	for _, d := range all { // rank order preserved (curation, §1)
		if domain.Matches(d, q) {
			kept = append(kept, summarize(d))
		}
	}
	domain.SortInPlace(kept, q.Sort)
	return page(kept, q.Offset, q.Limit), nil
}

// GetPost: the draft-leak boundary — a draft slug 404s exactly like an unknown
// one (Source guarantees only published rows ever reach here).
func (s *Service) GetPost(ctx context.Context, slug string) (domain.Detail, error) {
	all, err := s.src.PublishedPosts(ctx)
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

// SuggestPosts: the palette launcher — contains-match over title+preview only,
// corpus order, hard cap. No pagination (it is the same engine's low-limit
// face; §1).
func (s *Service) SuggestPosts(ctx context.Context, q string, limit int) (domain.SuggestResult, error) {
	if limit <= 0 {
		limit = domain.DefaultSuggestLim
	}
	if limit > domain.SuggestHardCap {
		limit = domain.SuggestHardCap
	}
	all, err := s.src.PublishedPosts(ctx)
	if err != nil {
		return domain.SuggestResult{}, err
	}
	needle := strings.ToLower(q)
	res := []domain.Summary{}
	for _, d := range all {
		if needle != "" && domain.SuggestMatch(d, needle) {
			res = append(res, summarize(d))
			if len(res) == limit {
				break
			}
		}
	}
	return domain.SuggestResult{Results: res}, nil
}

// SearchPosts: ranked + faceted + paginated. Relevance (occurrence count, the
// mock-ported score) is the implicit default when q is present; an explicit
// sort overrides it keeping totals identical (§1, harness-tested).
// M3 swaps the scoring source to Meilisearch; the endpoint shape never moves.
func (s *Service) SearchPosts(ctx context.Context, q domain.ListQuery) (domain.ListResult, error) {
	needle := strings.ToLower(strings.TrimSpace(q.Q))
	if needle == "" {
		return domain.ListResult{Total: 0, Items: []domain.Summary{}}, nil
	}
	all, err := s.src.PublishedPosts(ctx)
	if err != nil {
		return domain.ListResult{}, err
	}
	type scored struct {
		sum   domain.Summary
		score int
	}
	var hits []scored
	for _, d := range all {
		if !domain.Matches(d, q) {
			continue
		}
		sc := domain.Score(d, needle)
		if sc > 0 {
			hits = append(hits, scored{summarize(d), sc})
		}
	}
	// relevance: score desc, stable — corpus (rank) order breaks ties, exactly
	// the mock's Array.sort behavior
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	sums := make([]domain.Summary, len(hits))
	for i, h := range hits {
		sums[i] = h.sum
	}
	domain.SortInPlace(sums, q.Sort) // explicit override rides over relevance
	return page(sums, q.Offset, q.Limit), nil
}

func (s *Service) GetPhotos(ctx context.Context, limit int) (domain.PhotoList, error) {
	all, err := s.src.Photos(ctx)
	if err != nil {
		return domain.PhotoList{}, err
	}
	var gallery = []domain.Photo{} // §1 serves gallery=true ONLY; flags are BE-internal
	for _, p := range all {
		if p.Gallery {
			gallery = append(gallery, domain.Photo{
				Src: p.Src, Alt: p.Alt, Width: p.Width, Height: p.Height,
			})
		}
	}
	total := len(gallery)
	if limit < 0 || limit > total {
		limit = total
	}
	return domain.PhotoList{Total: total, Items: gallery[:limit]}, nil
}

// FeaturedPhoto: 404 when unset (the FE tolerates; kept for BE parity, §1).
func (s *Service) FeaturedPhoto(ctx context.Context) (domain.Photo, error) {
	all, err := s.src.Photos(ctx)
	if err != nil {
		return domain.Photo{}, err
	}
	for _, p := range all {
		if p.Featured {
			return domain.Photo{Src: p.Src, Alt: p.Alt, Width: p.Width, Height: p.Height}, nil
		}
	}
	return domain.Photo{}, store.ErrNotFound
}

func (s *Service) ListTags(ctx context.Context) ([]domain.Tag, error) {
	tags, err := s.src.TagCloud(ctx)
	if err != nil {
		return nil, err
	}
	if tags == nil {
		return []domain.Tag{}, nil
	}
	return tags, nil
}

// ---- helpers ----

func page(items []domain.Summary, offset, limit int) domain.ListResult {
	total := len(items)
	if offset > total {
		offset = total
	}
	if limit <= 0 { // absent limit = the rest (mock parity, §1)
		limit = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]domain.Summary, 0, end-offset)
	out = append(out, items[offset:end]...)
	return domain.ListResult{Total: total, Items: out}
}
