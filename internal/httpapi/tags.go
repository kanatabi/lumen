package httpapi

// Tags: the home-rail cloud — curated membership + counts, rendered as-is by
// the FE (§1: counting and ordering are decided here, never at read).

import (
	"net/http"
)

func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.ListTags(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
