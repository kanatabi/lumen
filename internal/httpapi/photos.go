package httpapi

// Photos: the public gallery (gallery=true only — curation is the server's)
// and the featured answer (404 when unset; the FE tolerates).

import (
	"net/http"
)

func (s *Server) photos(w http.ResponseWriter, r *http.Request) {
	limit, err := qsInt(r, "limit", -1) // absent = the full curated gallery page
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.svc.GetPhotos(r.Context(), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) featured(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.FeaturedPhoto(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
