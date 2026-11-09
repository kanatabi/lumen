package public

import (
	"net/http"

	"github.com/kanatabi/lumen/internal/httpx"
)

func (h *Handler) tags(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.ListTags(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}
