package search

import (
	"context"
	"fmt"

	meili "github.com/meilisearch/meilisearch-go"

	"github.com/kanatabi/lumen/internal/domain"
)

// The ADMIN door (§8 GET /admin/search): same engine, minus the public
// restrictions — drafts included (no status clause), photos unfiltered by
// gallery. Deleted rows are simply ABSENT from the index: truth vanished →
// the worker deleted the doc, so convergence does the access control here.

// AdminPostsByQuery: facets + optional relevance, NO status filter.
func (s *Service) AdminPostsByQuery(ctx context.Context, q domain.ListQuery) (ListPage, error) {
	sr := &meili.SearchRequest{
		Filter:               adminPostFilter(q),
		AttributesToRetrieve: []string{"slug"},
		Offset:               int64(q.Offset),
		Limit:                int64(q.Limit),
	}
	if q.Limit == 0 {
		sr.Limit = postLimitWhenAbsent
	}
	if sort := postSort(q.Sort, q.Q); len(sort) > 0 {
		sr.Sort = sort
	}
	res, err := s.m.Index(IndexPosts).Search(q.Q, sr)
	if err != nil {
		return ListPage{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return ListPage{Slugs: slugsOf(res), Total: res.EstimatedTotalHits}, nil
}

// AdminPhotosByQuery: alt/collection search over the WHOLE library —
// gallery=false photos may surface, because the admin palette must find
// everything (§4).
func (s *Service) AdminPhotosByQuery(ctx context.Context, q string, limit int) (PhotoPage, error) {
	sr := &meili.SearchRequest{
		AttributesToRetrieve: []string{"id"},
		Sort:                 []string{"sort:asc"},
		Limit:                int64(max(limit, 1)),
	}
	res, err := s.m.Index(IndexPhotos).Search(q, sr)
	if err != nil {
		return PhotoPage{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return PhotoPage{IDs: idsOf(res), Total: res.EstimatedTotalHits}, nil
}

func adminPostFilter(q domain.ListQuery) string {
	f := facetClauses(q) // the SAME tag/window clauses minus the status rule
	if len(f) == 0 {
		return ""
	}
	return joinAnd(f)
}
