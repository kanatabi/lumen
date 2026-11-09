package public

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/httpx"
)

func (h *Handler) listPosts(w http.ResponseWriter, r *http.Request) {
	lq, err := httpx.ListQuery(r, false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	res, err := h.svc.GetPosts(r.Context(), lq)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) getPost(w http.ResponseWriter, r *http.Request) {
	d, err := h.svc.GetPost(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}
