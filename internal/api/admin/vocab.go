package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
)

func (h *Handler) mountVocab(a chi.Router) {
	a.Get("/tags", h.listTags)
	a.Get("/tags/search", h.searchTags)
	a.Post("/tags", h.createTag)
	a.Patch("/tags/bulk", h.bulkTags)
	a.Patch("/tags/{name}", h.patchTag)
	a.Delete("/tags/{name}", h.deleteTag)
	a.Get("/collections", h.listCollections)
	a.Post("/collections", h.createCollection)
}

func (h *Handler) listTags(w http.ResponseWriter, r *http.Request) {
	tags, err := h.svc.ListTags(r.Context(), r.URL.Query().Get("order"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, tags)
}

// searchTags: the desk bar — fuzzy lives in the engine (ruling 2026-11-10),
// pagination rides the URL, total is honest. q is REQUIRED (an empty bar is
// the client's list mode, not this endpoint's problem).
func (h *Handler) searchTags(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		httpx.WriteErr(w, http.StatusBadRequest, "q required")
		return
	}
	page, err := httpx.QueryInt(r, "page", 1)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "page must be an integer")
		return
	}
	pageSize, err := httpx.QueryInt(r, "pageSize", 30)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "pageSize must be an integer")
		return
	}
	out, err := h.svc.SearchTags(r.Context(), q, page, pageSize)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// bulkTags: the desk multi-select. One UPDATE, count of ACTUAL changes.
func (h *Handler) bulkTags(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Names    []string `json:"names"`
		Featured bool     `json:"featured"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Names) == 0 {
		httpx.WriteErr(w, http.StatusBadRequest, "expected {names: [..], featured: bool}")
		return
	}
	updated, err := h.svc.BulkFeatureTags(r.Context(), in.Names, in.Featured)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	slog.Info("vocab.tag.bulk", "sent", len(in.Names), "updated", updated, "featured", in.Featured)
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"updated": updated})
}

func (h *Handler) createTag(w http.ResponseWriter, r *http.Request) {
	name, err := decodeName(r)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "expected {name}")
		return
	}
	tag, err := h.svc.CreateTag(r.Context(), name)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusCreated, tag)
	case errors.Is(err, adminsvc.ErrDupVocab):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, adminsvc.ErrBadName):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	default:
		httpx.Fail(w, r, err)
	}
}

// patchTag is the desk's row verb: {name?, featured?} — rename propagates
// to every post via the id-keyed FK web; the featured flag is the manual
// promotion to the earendil TOPICS widget (curation is an owner decision,
// never arithmetic — 2026-11-10 ruling).
func (h *Handler) patchTag(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var in adminsvc.TagPatch
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "expected {name?, featured?}")
		return
	}
	if in.Name == nil && in.Featured == nil {
		httpx.WriteErr(w, http.StatusBadRequest, "nothing to patch")
		return
	}
	out, err := h.svc.UpdateTag(r.Context(), name, in)
	switch {
	case err == nil:
		slog.Info("vocab.tag.patch", "name", name, "renamed", out.Name)
		httpx.WriteJSON(w, http.StatusOK, out)
	case errors.Is(err, adminsvc.ErrBadName):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, adminsvc.ErrDupVocab):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, adminsvc.ErrUnknownVocab):
		httpx.WriteErr(w, http.StatusNotFound, err.Error())
	default:
		slog.Error("vocab.tag.patch.failed", "name", name, "err", err)
		httpx.Fail(w, r, err)
	}
}

func (h *Handler) deleteTag(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	err := h.svc.DeleteTag(r.Context(), name)
	switch {
	case err == nil:
		slog.Info("vocab.tag.delete", "name", name)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, adminsvc.ErrUnknownVocab):
		httpx.WriteErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, adminsvc.ErrTagInUse):
		// arrives with the referencing COUNT embedded — shown verbatim
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	default:
		slog.Error("vocab.tag.delete.failed", "name", name, "err", err)
		httpx.Fail(w, r, err)
	}
}

func (h *Handler) listCollections(w http.ResponseWriter, r *http.Request) {
	collections, err := h.svc.ListCollections(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, collections)
}

func (h *Handler) createCollection(w http.ResponseWriter, r *http.Request) {
	name, err := decodeName(r)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "expected {name}")
		return
	}
	collection, err := h.svc.CreateCollection(r.Context(), name)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusCreated, collection)
	case errors.Is(err, adminsvc.ErrDupVocab):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, adminsvc.ErrBadName):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	default:
		httpx.Fail(w, r, err)
	}
}

func decodeName(r *http.Request) (string, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return "", err
	}
	return in.Name, nil
}
