// Query side of the engine (owner ruling 2026-11-08): ALL list and search
// serving answers here — Postgres is truth for writes, hydration and point
// reads, never for search/list queries.
//
// Filter/sort mapping is the §1 contract in Meilisearch syntax:
//
//	status = published   — the leak rule, carried by EVERY public query
//	tags IN [...]        — OR-matched facets (§1)
//	day BETWEEN (yyyymmdd ints — inclusive windows with zero format ambiguity)
//	readTime >= / <=     — minute window
//	sort: rank|day|readTime — curation and explicit sorts are ATTRIBUTES,
//	so determinism survives the engine (the mock twin and Meilisearch agree
//	on lists; only relevance ordering is engine-authoritative).
package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	meili "github.com/meilisearch/meilisearch-go"

	"github.com/kanatabi/lumen/internal/domain"
)

// ErrUnavailable = the engine cannot answer. The pure-engine ruling makes
// this the ONLY search failure mode: 503, never a silent Postgres fallback.
var ErrUnavailable = errors.New("search engine unavailable")

// ListPage is one page of serving keys in engine order + the FULL filtered
// total (the "N posts" line, §1).
type ListPage struct {
	Slugs []string
	Total int64
}

// PostLimitWhenAbsent mirrors mock parity: absent limit serves the corpus;
// the engine bound that emulates (maxTotalHits, see Ensure).
const postLimitWhenAbsent = 1000

func (s *Service) ListPosts(ctx context.Context, q domain.ListQuery) (ListPage, error) {
	searchRequest := &meili.SearchRequest{
		Filter:               postFilter(q),
		AttributesToRetrieve: []string{"slug"},
		Offset:               int64(q.Offset),
		Limit:                int64(q.Limit),
	}
	if q.Limit == 0 {
		searchRequest.Limit = postLimitWhenAbsent
	}
	if sort := postSort(q.Sort, q.Q); len(sort) > 0 {
		searchRequest.Sort = sort
	}
	return s.searchPosts(ctx, q.Q, searchRequest)
}

// SearchPosts is the relevance face: identical machinery to ListPosts —
// a non-empty q makes Meilisearch rank, an explicit sort overrides (§1).
func (s *Service) SearchPosts(ctx context.Context, q domain.ListQuery) (ListPage, error) {
	return s.ListPosts(ctx, q)
}

func (s *Service) SuggestPosts(ctx context.Context, needle string, limit int) (ListPage, error) {
	searchRequest := &meili.SearchRequest{
		Filter:               `status = published`,
		Sort:                 []string{"rank:asc"}, // corpus order within matches (twin parity)
		Limit:                int64(limit),
		AttributesToRetrieve: []string{"slug"},
		AttributesToSearchOn: []string{"title", "preview"}, // the launcher's narrower field set (§1)
	}
	res, err := s.client.Index(s.postsIndex).Search(needle, searchRequest)
	if err != nil {
		return ListPage{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return ListPage{Slugs: slugsOf(res), Total: res.EstimatedTotalHits}, nil
}

func (s *Service) searchPosts(ctx context.Context, q string, searchRequest *meili.SearchRequest) (ListPage, error) {
	res, err := s.client.Index(s.postsIndex).Search(q, searchRequest)
	if err != nil {
		return ListPage{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return ListPage{Slugs: slugsOf(res), Total: res.EstimatedTotalHits}, nil
}

// Hit is map[string]json.RawMessage in the v0.36 SDK — decode per field,
// skip malformed silently (an un-decodable id is not worth failing a page).
func slugsOf(res *meili.SearchResponse) []string {
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		var slug string
		if raw, ok := h["slug"]; ok && json.Unmarshal(raw, &slug) == nil && slug != "" {
			out = append(out, slug)
		}
	}
	return out
}

func idsOf(res *meili.SearchResponse) []int64 {
	out := make([]int64, 0, len(res.Hits))
	for _, h := range res.Hits {
		var id int64
		if raw, ok := h["id"]; ok && json.Unmarshal(raw, &id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func postFilter(q domain.ListQuery) string {
	// EVERY public query carries the leak rule first
	return joinAnd(append([]string{"status = published"}, facetClauses(q)...))
}

func joinAnd(parts []string) string { return strings.Join(parts, " AND ") }

// facetClauses: the §1 facets without the status rule — shared by the
// public filter (postFilter prepends status) and the admin door (which
// deliberately omits it).
func facetClauses(q domain.ListQuery) []string {
	parts := []string{}
	if len(q.Tags) > 0 {
		quoted := make([]string, len(q.Tags))
		for i, t := range q.Tags {
			quoted[i] = fmt.Sprintf("%q", t)
		}
		parts = append(parts, "tags IN ["+strings.Join(quoted, ", ")+"]")
	}
	if q.From != "" {
		parts = append(parts, "day >= "+strconv.Itoa(DayInt(q.From)))
	}
	if q.To != "" {
		parts = append(parts, "day <= "+strconv.Itoa(DayInt(q.To)))
	}
	if q.ReadMin != nil {
		parts = append(parts, "readTime >= "+strconv.Itoa(*q.ReadMin))
	}
	if q.ReadMax != nil {
		parts = append(parts, "readTime <= "+strconv.Itoa(*q.ReadMax))
	}
	return parts
}

// postSort maps §1 sort to sortable attributes. Relevance (q present, no
// explicit sort) returns nil — engine ranking owns it. Lists (no q) default
// to curation: rank:asc.
func postSort(sort, q string) []string {
	switch sort {
	case "newest":
		return []string{"day:desc"}
	case "oldest":
		return []string{"day:asc"}
	case "quickest":
		return []string{"readTime:asc"}
	case "longest":
		return []string{"readTime:desc"}
	case "relevance":
		if q != "" {
			return nil
		}
		return []string{"rank:asc"}
	default: // "" — absent
		if q != "" {
			return nil // relevance is the implicit search default (§1)
		}
		return []string{"rank:asc"}
	}
}

// ---- photos ----

// ListPhotos serves the gallery (§1: gallery=true ONLY is a filter the
// public path always carries) in curation order.
func (s *Service) ListPhotos(ctx context.Context, limit int) (PhotoPage, error) {
	searchRequest := &meili.SearchRequest{
		Filter:               `gallery = true`,
		Sort:                 []string{"sort:asc"},
		Offset:               0,
		Limit:                int64(limit),
		AttributesToRetrieve: []string{"id"},
	}
	if limit <= 0 {
		searchRequest.Limit = postLimitWhenAbsent
	}
	res, err := s.client.Index(s.photosIndex).Search("", searchRequest)
	if err != nil {
		return PhotoPage{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return PhotoPage{IDs: idsOf(res), Total: res.EstimatedTotalHits}, nil
}

type PhotoPage struct {
	IDs   []int64
	Total int64
}
