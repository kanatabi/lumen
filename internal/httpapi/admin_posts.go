package httpapi

// /admin posts (§8): list envelope, create/read/update (rev CAS), the
// publish/unpublish machine, soft delete. Handlers stay thin: decode →
// store (whose transactions ride index jobs) → encode → best-effort
// enqueue fallback for non-tx paths is unnecessary — every mutation here
// is tx-bound to the queue.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/store"
)

type envelope[T any] struct {
	Total    int `json:"total"`
	Items    []T `json:"items"`
	Page     int `json:"page"`
	PageSize int `json:"pageSize"`
}

func (s *Server) mountAdminPosts(a chi.Router) {
	a.Get("/posts", s.adminListPosts)
	a.Post("/posts", s.adminCreatePost)
	a.Get("/posts/{slug}", s.adminGetPost)
	a.Put("/posts/{slug}", s.adminUpdatePost)
	a.Post("/posts/{slug}/publish", s.adminPublish)
	a.Post("/posts/{slug}/unpublish", s.adminUnpublish)
	a.Delete("/posts/{slug}", s.adminDeletePost)
}

func (s *Server) adminListPosts(w http.ResponseWriter, r *http.Request) {
	qy := r.URL.Query()
	page, _ := strconv.Atoi(qy.Get("page"))
	total, items, err := s.admin.Pool.ListAdminPosts(r.Context(), store.AdminListOpts{
		Status: qy.Get("status"),
		Tags:   qy["tag"],
		From:   qy.Get("from"),
		To:     qy.Get("to"),
		Sort:   qy.Get("sort"),
		Dir:    qy.Get("dir"),
		Page:   page,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if items == nil {
		items = []store.AdminPostSummary{}
	}
	writeJSON(w, http.StatusOK, envelope[store.AdminPostSummary]{
		Total: total, Items: items, Page: clampPage(qy.Get("page")), PageSize: 20,
	})
}

func clampPage(raw string) int {
	n, _ := strconv.Atoi(raw)
	return max(n, 1)
}

func (s *Server) adminCreatePost(w http.ResponseWriter, r *http.Request) {
	var in store.PostDraftInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json draft required")
		return
	}
	d, err := s.admin.Pool.CreateAdminPost(r.Context(), in)
	if err != nil {
		if errors.Is(err, store.ErrSlugTaken) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) adminGetPost(w http.ResponseWriter, r *http.Request) {
	d, err := s.admin.Pool.GetAdminPost(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, store.ErrNoPosts) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) adminUpdatePost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Draft store.PostDraftInput `json:"draft"`
		Rev   int                  `json:"rev"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "expected {draft, rev}")
		return
	}
	d, err := s.admin.Pool.UpdateAdminPost(r.Context(), chi.URLParam(r, "slug"), body.Draft, body.Rev, s.admin.Queue)
	if errors.Is(err, store.ErrRevConflict) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, store.ErrNoPosts) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) adminPublish(w http.ResponseWriter, r *http.Request) {
	d, err := s.admin.Pool.PublishAdminPost(r.Context(), chi.URLParam(r, "slug"), s.admin.Queue)
	s.statusMachineOr(w, err, d)
}

func (s *Server) adminUnpublish(w http.ResponseWriter, r *http.Request) {
	d, err := s.admin.Pool.UnpublishAdminPost(r.Context(), chi.URLParam(r, "slug"), s.admin.Queue)
	s.statusMachineOr(w, err, d)
}

func (s *Server) statusMachineOr(w http.ResponseWriter, err error, d store.AdminPostDetail) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, d)
	case errors.Is(err, store.ErrBadTransition):
		// the §4 machine: draft→published⇄unpublished, never back to draft
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNoPosts):
		writeErr(w, http.StatusNotFound, "not found")
	default:
		s.fail(w, err)
	}
}

func (s *Server) adminDeletePost(w http.ResponseWriter, r *http.Request) {
	if err := s.admin.Pool.DeleteAdminPost(r.Context(), chi.URLParam(r, "slug"), s.admin.Queue); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) // soft: row hidden, media GC-windowed
}
