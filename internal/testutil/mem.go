package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/store"
)

// Fixture is the shape of testdata/earendil-fixture.json — the contract twin
// generated from earendil's mock. Drafts ride along as leak-guard bait: the
// in-memory Source filters them out exactly like the Postgres Source's WHERE
// clause, so the offline harness tests the same rule the DB enforces.
type Fixture struct {
	Comment string          `json:"_comment"`
	Posts   []domain.Detail `json:"posts"`  // the published corpus, curation order
	Drafts  []domain.Detail `json:"drafts"` // must NEVER surface
	Photos  []domain.Photo  `json:"photos"` // gallery/featured flags populated
	Tags    []domain.Tag    `json:"tags"`   // curated cloud
}

type MemSource struct{ fixture Fixture }

// Mem builds the in-memory Source from the fixture — the offline twin of pg.
func Mem(path string) (store.Source, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fixture: %w", err)
	}
	var f Fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("fixture json: %w", err)
	}
	// synthetic stable ids (file order) — hydration glue, like the DB's
	for i := range f.Photos {
		f.Photos[i].ID = int64(i + 1)
	}
	return &MemSource{fixture: f}, nil
}

func (m *MemSource) PublishedPosts(_ context.Context) ([]domain.Detail, error) {
	out := make([]domain.Detail, len(m.fixture.Posts))
	copy(out, m.fixture.Posts) // drafts structurally absent — the leak rule, in-memory form
	return out, nil
}

func (m *MemSource) Photos(_ context.Context) ([]domain.Photo, error) {
	out := make([]domain.Photo, len(m.fixture.Photos))
	copy(out, m.fixture.Photos)
	return out, nil
}

func (m *MemSource) TagCloud(_ context.Context) ([]domain.Tag, error) {
	out := make([]domain.Tag, len(m.fixture.Tags))
	copy(out, m.fixture.Tags)
	return out, nil
}

// ---- the twin AS engine (service.Engine seam, offline) ----
//
// This is the M1 read-path logic re-homed: the mock's exact semantics behind
// the engine interface, so offline CI asserts byte-parity with earendil's
// mock while production answers through Meilisearch. The twin is a fixture
// tool — the serving-model ruling applies to the real thing, and the real
// thing is tested against REAL Meilisearch in the engine-tier suite.

func (m *MemSource) ListPosts(_ context.Context, q domain.ListQuery) (search.ListPage, error) {
	var kept []domain.Summary
	for _, d := range m.fixture.Posts { // publication order == curation rank
		if domain.Matches(d, q) {
			kept = append(kept, summary(d))
		}
	}
	domain.SortInPlace(kept, q.Sort)
	total := len(kept)
	if q.Limit == 0 { // absent limit = the whole corpus (mock parity)
		q.Limit = total
	}
	page := slicePage(kept, q.Offset, q.Limit)
	return search.ListPage{Slugs: slugsOf(page), Total: int64(total)}, nil
}

func (m *MemSource) SearchPosts(_ context.Context, q domain.ListQuery) (search.ListPage, error) {
	needle := strings.ToLower(strings.TrimSpace(q.Q))
	if needle == "" {
		return search.ListPage{Total: 0}, nil
	}
	type scored struct {
		sum   domain.Summary
		score int
	}
	var hits []scored
	for _, d := range m.fixture.Posts {
		if !domain.Matches(d, q) {
			continue
		}
		if sc := domain.Score(d, needle); sc > 0 {
			hits = append(hits, scored{summary(d), sc})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	sums := make([]domain.Summary, len(hits))
	for i, h := range hits {
		sums[i] = h.sum
	}
	domain.SortInPlace(sums, q.Sort) // explicit override rides over relevance
	limit := q.Limit
	if limit == 0 {
		limit = len(sums)
	}
	page := slicePage(sums, q.Offset, limit)
	return search.ListPage{Slugs: slugsOf(page), Total: int64(len(sums))}, nil
}

func (m *MemSource) SuggestPosts(_ context.Context, needle string, limit int) (search.ListPage, error) {
	needle = strings.ToLower(needle)
	var kept []domain.Summary
	for _, d := range m.fixture.Posts {
		if needle != "" && domain.SuggestMatch(d, needle) {
			kept = append(kept, summary(d))
			if len(kept) == limit {
				break
			}
		}
	}
	return search.ListPage{Slugs: slugsOf(kept), Total: int64(len(kept))}, nil
}

func (m *MemSource) ListPhotos(_ context.Context, limit int) (search.PhotoPage, error) {
	var ids []int64
	for _, p := range m.fixture.Photos {
		if p.Gallery {
			ids = append(ids, p.ID)
		}
	}
	total := len(ids)
	if limit > 0 && limit < total {
		ids = ids[:limit]
	}
	return search.PhotoPage{IDs: ids, Total: int64(total)}, nil
}

func summary(d domain.Detail) domain.Summary {
	return domain.Summary{
		Slug: d.Slug, Title: d.Title, Subtitle: d.Subtitle, Preview: d.Preview,
		Date: d.Date, ReadTime: d.ReadTime, Thumbnail: d.Thumbnail,
	}
}

func slugsOf(items []domain.Summary) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Slug)
	}
	return out
}

func slicePage(items []domain.Summary, offset, limit int) []domain.Summary {
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}
