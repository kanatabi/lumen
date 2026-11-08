package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kanatabi/lumen/internal/domain"
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

type memSource struct{ f Fixture }

// Mem builds the in-memory Source from the fixture — the offline twin of pg.
func Mem(path string) (Source, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fixture: %w", err)
	}
	var f Fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("fixture json: %w", err)
	}
	return &memSource{f: f}, nil
}

func (m *memSource) PublishedPosts(_ context.Context) ([]domain.Detail, error) {
	out := make([]domain.Detail, len(m.f.Posts))
	copy(out, m.f.Posts) // drafts structurally absent — the leak rule, in-memory form
	return out, nil
}

func (m *memSource) Photos(_ context.Context) ([]domain.Photo, error) {
	out := make([]domain.Photo, len(m.f.Photos))
	copy(out, m.f.Photos)
	return out, nil
}

func (m *memSource) TagCloud(_ context.Context) ([]domain.Tag, error) {
	out := make([]domain.Tag, len(m.f.Tags))
	copy(out, m.f.Tags)
	return out, nil
}
