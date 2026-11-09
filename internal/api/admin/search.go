package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/httpx"
)

// GET /admin/search — the palette's content door: one q, three groups.
func (h *Handler) mountSearch(a chi.Router) { a.Get("/search", h.search) }

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	results, err := h.svc.Search(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, results)
}
