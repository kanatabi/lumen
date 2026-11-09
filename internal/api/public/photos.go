package public

import (
	"net/http"

	"github.com/kanatabi/lumen/internal/httpx"
)

func (h *Handler) photos(w http.ResponseWriter, r *http.Request) {
	limit, err := httpx.QueryInt(r, "limit", -1) // absent = the full curated gallery page
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	res, err := h.svc.GetPhotos(r.Context(), limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) featured(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.FeaturedPhoto(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}
