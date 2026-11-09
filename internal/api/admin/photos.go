package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
)

const photosPageSize = 24

func (h *Handler) mountPhotos(a chi.Router) {
	a.Post("/photos", h.uploadPhoto) // multipart relay (§5, D1-revised)
	a.Get("/photos", h.listPhotos)
	a.Get("/photos/{id}", h.getPhoto)
	a.Get("/photos/{id}/references", h.photoReferences)
	a.Patch("/photos/{id}", h.patchPhoto)
	a.Delete("/photos/{id}", h.deletePhoto)
}

func photoIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteErr(w, http.StatusBadRequest, "photo id must be a positive integer")
		return 0, false
	}
	return id, true
}

func (h *Handler) listPhotos(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var gallery *bool
	switch query.Get("gallery") {
	case "1":
		yes := true
		gallery = &yes
	case "0":
		no := false
		gallery = &no
	}
	page := clampPage(query.Get("page"))
	total, items, err := h.svc.ListPhotos(r.Context(), adminsvc.PhotoListOpts{
		Collections: query["collection"],
		Gallery:     gallery,
		From:        query.Get("from"),
		To:          query.Get("to"),
		Sort:        query.Get("sort"),
		Dir:         query.Get("dir"),
		Page:        page,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, envelope[adminsvc.AdminPhoto]{
		Total: total, Items: items, Page: page, PageSize: photosPageSize,
	})
}

func (h *Handler) getPhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := photoIDParam(w, r)
	if !ok {
		return
	}
	row, err := h.svc.GetPhotoRow(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row.Admin())
}

func (h *Handler) photoReferences(w http.ResponseWriter, r *http.Request) {
	id, ok := photoIDParam(w, r)
	if !ok {
		return
	}
	refs, err := h.svc.PhotoReferences(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, refs)
}

func (h *Handler) patchPhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := photoIDParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Alt        *string  `json:"alt"`
		Collection **string `json:"collection"` // JSON null clears; absent skips
		Gallery    *bool    `json:"gallery"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json patch required")
		return
	}
	row, err := h.svc.PatchPhoto(r.Context(), id, adminsvc.PhotoPatch{
		Alt: body.Alt, Collection: body.Collection, Gallery: body.Gallery,
	})
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, row.Admin())
	case errors.Is(err, adminsvc.ErrNoPhoto):
		httpx.WriteErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, adminsvc.ErrUnknownColl):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	default:
		httpx.Fail(w, r, err)
	}
}

func (h *Handler) deletePhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := photoIDParam(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeletePhoto(r.Context(), id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
