package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
)

const postsPageSize = 20

func (h *Handler) mountPosts(a chi.Router) {
	a.Get("/posts", h.listPosts)
	a.Post("/posts", h.createPost)
	a.Get("/posts/{slug}", h.getPost)
	a.Put("/posts/{slug}", h.updatePost)
	a.Post("/posts/{slug}/publish", h.publishPost)
	a.Post("/posts/{slug}/unpublish", h.unpublishPost)
	a.Delete("/posts/{slug}", h.deletePost)
}

func (h *Handler) listPosts(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	total, items, err := h.svc.ListPosts(r.Context(), adminsvc.AdminListOpts{
		Status: query.Get("status"),
		Tags:   query["tag"],
		From:   query.Get("from"),
		To:     query.Get("to"),
		Sort:   query.Get("sort"),
		Dir:    query.Get("dir"),
		Page:   clampPage(query.Get("page")),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if items == nil {
		items = []adminsvc.AdminPostSummary{}
	}
	httpx.WriteJSON(w, http.StatusOK, envelope[adminsvc.AdminPostSummary]{
		Total: total, Items: items, Page: clampPage(query.Get("page")), PageSize: postsPageSize,
	})
}

func (h *Handler) createPost(w http.ResponseWriter, r *http.Request) {
	var in adminsvc.PostDraftInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "json draft required")
		return
	}
	detail, err := h.svc.CreatePost(r.Context(), in)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusCreated, detail)
	case errors.Is(err, adminsvc.ErrSlugTaken):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	default:
		httpx.Fail(w, r, err)
	}
}

func (h *Handler) getPost(w http.ResponseWriter, r *http.Request) {
	detail, err := h.svc.GetPost(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		httpx.Fail(w, r, err) // ErrNoPosts maps to 404 on the ladder
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func (h *Handler) updatePost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Draft adminsvc.PostDraftInput `json:"draft"`
		Rev   int                     `json:"rev"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "expected {draft, rev}")
		return
	}
	detail, err := h.svc.UpdatePost(r.Context(), chi.URLParam(r, "slug"), body.Draft, body.Rev)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, detail)
	case errors.Is(err, adminsvc.ErrRevConflict):
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, adminsvc.ErrNoPosts):
		httpx.WriteErr(w, http.StatusNotFound, "not found")
	default:
		httpx.Fail(w, r, err)
	}
}

func (h *Handler) publishPost(w http.ResponseWriter, r *http.Request) {
	detail, err := h.svc.Publish(r.Context(), chi.URLParam(r, "slug"))
	h.stateMachineOr(w, r, err, detail)
}

func (h *Handler) unpublishPost(w http.ResponseWriter, r *http.Request) {
	detail, err := h.svc.Unpublish(r.Context(), chi.URLParam(r, "slug"))
	h.stateMachineOr(w, r, err, detail)
}

func (h *Handler) stateMachineOr(w http.ResponseWriter, r *http.Request, err error, detail adminsvc.AdminPostDetail) {
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, detail)
	case errors.Is(err, adminsvc.ErrBadTransition):
		// the §4 machine: draft→published⇄unpublished, never back to draft
		httpx.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, adminsvc.ErrNoPosts):
		httpx.WriteErr(w, http.StatusNotFound, "not found")
	default:
		httpx.Fail(w, r, err)
	}
}

func (h *Handler) deletePost(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeletePost(r.Context(), chi.URLParam(r, "slug")); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true}) // soft: row hidden, media GC-windowed
}
