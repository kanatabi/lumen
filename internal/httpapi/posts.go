package httpapi

// Posts: the list (§1 curation order + facets) and the single-post door.

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) listPosts(w http.ResponseWriter, r *http.Request) {
	q, err := s.listQuery(r, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.svc.GetPosts(r.Context(), q)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) getPost(w http.ResponseWriter, r *http.Request) {
	d, err := s.svc.GetPost(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}
