package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/store"
)

func (h *Handler) mountVocab(a chi.Router) {
	a.Get("/tags", h.listTags)
	a.Post("/tags", h.createTag)
	a.Get("/collections", h.listCollections)
	a.Post("/collections", h.createCollection)
}

func (h *Handler) listTags(w http.ResponseWriter, r *http.Request) {
	tags, err := h.svc.ListTags(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, tags)
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
	case errors.Is(err, store.ErrDupVocab):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrBadName):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	default:
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
	case errors.Is(err, store.ErrDupVocab):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrBadName):
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
