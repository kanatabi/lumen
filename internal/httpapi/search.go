package httpapi

// The search door: ranked/paginated /search and the capped /search/suggest —
// one engine, two faces (§1). M3 swaps the ranking to Meilisearch; the
// endpoint shapes never move.

import (
	"net/http"

	"github.com/kanatabi/lumen/internal/domain"
)

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q, err := s.listQuery(r, true)
	if err != nil {
		s.fail(w, err)
		return
	}
	if r.URL.Query().Get("limit") == "" && q.Limit == 0 {
		q.Limit = 10 // the mock's destructured default — parity under defaults
	}
	res, err := s.svc.SearchPosts(r.Context(), q)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) suggest(w http.ResponseWriter, r *http.Request) {
	limit, err := qsInt(r, "limit", domain.DefaultSuggestLim)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.svc.SuggestPosts(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
