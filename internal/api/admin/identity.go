package admin

// identity.go — the internal identity door (2026-11-29). The app planes
// (lumen-staging/lumen-production) own their tasks tables but never the
// users lineage; they resolve usernames, display names, and exact-email
// invite searches through these three token-gated routes.
//
// The door is IN-CLUSTER and read-only. Its token is not a user session:
// it authenticates the calling deployment, not a person, and it is mounted
// only when configured (honest absence, the EnableBao pattern). Email is a
// predicate inside SearchPeople and is NEVER selected into the response.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/store"
)

const identityBatchMax = 200

type identityPerson struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

func (h *Handler) requireIdentityToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || h.identityToken == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(h.identityToken)) != 1 {
			httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// identityPeople is the invite typeahead's server half: username prefix
// (ranked first), display-name prefix, or an EXACT email. The response
// never carries the email.
func (h *Handler) identityPeople(w http.ResponseWriter, r *http.Request) {
	limit, err := httpx.QueryInt(r, "limit", 8)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if limit <= 0 {
		limit = 8
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := h.svc.SearchPeople(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		slog.Error("identity.people", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]identityPerson, len(rows))
	for index, row := range rows {
		out[index] = identityPerson{ID: row.ID, Username: row.Username, Name: row.Name}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"people": out})
}

// identityUsers batches id -> username/name hydration. Unknown ids are
// simply absent; the caller treats absence as the deleted-account signal.
func (h *Handler) identityUsers(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json body required")
		return
	}
	if len(in.IDs) > identityBatchMax {
		httpx.WriteErr(w, http.StatusBadRequest, "too many ids")
		return
	}
	rows, err := h.svc.PeopleByIDs(r.Context(), in.IDs)
	if err != nil {
		slog.Error("identity.users", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]identityPerson, len(rows))
	for index, row := range rows {
		out[index] = identityPerson{ID: row.ID, Username: row.Username, Name: row.Name}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": out})
}

// identityByUsername resolves the immutable login handle used by the
// invite wire. A disabled account still resolves (the invite door decides);
// an unknown handle is the honest 404.
func (h *Handler) identityByUsername(w http.ResponseWriter, r *http.Request) {
	id, err := h.svc.UserIDByUsername(r.Context(), chi.URLParam(r, "username"))
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id})
	case errors.Is(err, store.ErrNoSuchUser):
		httpx.WriteErr(w, http.StatusNotFound, "no such user")
	default:
		slog.Error("identity.by_username", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}
