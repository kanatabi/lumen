package public

// The search door: ranked/paginated /search and the capped /search/suggest —
// one engine, two faces (§1). Shapes never move; ranking is engine-owned.

import (
	"net/http"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/httpx"
)

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	lq, err := httpx.ListQuery(r, true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if r.URL.Query().Get("limit") == "" && lq.Limit == 0 {
		lq.Limit = 10 // the mock's destructured default — parity under defaults
	}
	res, err := h.svc.SearchPosts(r.Context(), lq)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) suggest(w http.ResponseWriter, r *http.Request) {
	limit, err := httpx.QueryInt(r, "limit", domain.DefaultSuggestLim)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	res, err := h.svc.SuggestPosts(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
