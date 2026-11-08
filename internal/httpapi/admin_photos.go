package httpapi

// /admin photos & references & vocab & the search door — thin handlers
// over the store/engine seams, one file per resource keeps that honest.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/domain"
	"github.com/kanatabi/lumen/internal/store"
)

func (s *Server) mountAdminPhotos(a chi.Router) {
	a.Get("/photos", s.adminListPhotos)
	a.Get("/photos/{id}", s.adminGetPhoto)
	a.Get("/photos/{id}/references", s.adminPhotoRefs)
	a.Patch("/photos/{id}", s.adminPatchPhoto)
	a.Delete("/photos/{id}", s.adminDeletePhoto)
}

func photoID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "photo id must be a positive integer")
		return 0, false
	}
	return id, true
}

func (s *Server) adminListPhotos(w http.ResponseWriter, r *http.Request) {
	qy := r.URL.Query()
	var gallery *bool
	switch qy.Get("gallery") {
	case "1":
		t := true
		gallery = &t
	case "0":
		f := false
		gallery = &f
	}
	page, _ := strconv.Atoi(qy.Get("page"))
	total, items, err := s.admin.Pool.ListAdminPhotos(r.Context(), store.PhotoListOpts{
		Collections: qy["collection"],
		Gallery:     gallery,
		From:        qy.Get("from"),
		To:          qy.Get("to"),
		Sort:        qy.Get("sort"),
		Dir:         qy.Get("dir"),
		Page:        page,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, envelope[store.AdminPhoto]{
		Total: total, Items: items, Page: clampPage(qy.Get("page")), PageSize: 24,
	})
}

func (s *Server) adminGetPhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := photoID(w, r)
	if !ok {
		return
	}
	row, err := s.admin.Pool.GetAdminPhotoRow(r.Context(), id)
	if errors.Is(err, store.ErrNoPhoto) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adminPhotoJSON(row))
}

func adminPhotoJSON(row store.PhotoDocRow) store.AdminPhoto { return row.Admin() }

func (s *Server) adminPhotoRefs(w http.ResponseWriter, r *http.Request) {
	id, ok := photoID(w, r)
	if !ok {
		return
	}
	refs, err := s.admin.Pool.ListPhotoReferences(r.Context(), id)
	if errors.Is(err, store.ErrNoPhoto) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, refs)
}

func (s *Server) adminPatchPhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := photoID(w, r)
	if !ok {
		return
	}
	var body struct {
		Alt        *string  `json:"alt"`
		Collection **string `json:"collection"`
		Gallery    *bool    `json:"gallery"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "json patch required")
		return
	}
	row, err := s.admin.Pool.UpdateAdminPhoto(r.Context(), id, store.PhotoPatch{
		Alt: body.Alt, Collection: body.Collection, Gallery: body.Gallery,
	}, s.admin.Queue)
	switch {
	case errors.Is(err, store.ErrNoPhoto):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrUnknownColl):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusOK, adminPhotoJSON(row))
	}
}

func (s *Server) adminDeletePhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := photoID(w, r)
	if !ok {
		return
	}
	if err := s.admin.Pool.DeleteAdminPhoto(r.Context(), id, s.admin.Queue); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- vocab: tags & collections (§8 eager creation, 409 duplicates) ----

func (s *Server) mountAdminVocab(a chi.Router) {
	a.Get("/tags", s.adminListTags)
	a.Post("/tags", s.adminCreateTag)
	a.Get("/collections", s.adminListCollections)
	a.Post("/collections", s.adminCreateCollection)
}

func (s *Server) adminListTags(w http.ResponseWriter, r *http.Request) {
	t, err := s.admin.Pool.ListAdminTags(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) adminCreateTag(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	t, err := s.admin.Pool.CreateAdminTag(r.Context(), in.Name)
	if errors.Is(err, store.ErrDupVocab) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, store.ErrBadName) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) adminListCollections(w http.ResponseWriter, r *http.Request) {
	c, err := s.admin.Pool.ListAdminCollections(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) adminCreateCollection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	c, err := s.admin.Pool.CreateAdminCollection(r.Context(), in.Name)
	if errors.Is(err, store.ErrDupVocab) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, store.ErrBadName) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// ---- the palette's content door: GET /admin/search?q ----
// Three groups, ≤8 each, NO pagination (a finder, not a browse axis):
// posts incl. drafts (engine), photos (engine), tags (Postgres name lookup).

func (s *Server) mountAdminSearch(a chi.Router) {
	a.Get("/search", s.adminSearch)
}

func (s *Server) adminSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	out := struct {
		Posts  []store.AdminPostSummary `json:"posts"`
		Photos []store.AdminPhoto       `json:"photos"`
		Tags   []store.TagRowOut        `json:"tags"`
	}{Posts: []store.AdminPostSummary{}, Photos: []store.AdminPhoto{}, Tags: []store.TagRowOut{}}
	if q == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	postPage, err := s.engineAdmin.AdminPostsByQuery(r.Context(), domain.ListQuery{Q: q, Limit: 8})
	if err != nil {
		s.fail(w, err)
		return
	}
	bySlug, err := s.admin.Pool.AdminSummariesBySlugs(r.Context(), postPage.Slugs)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, slug := range postPage.Slugs {
		if sum, ok := bySlug[slug]; ok {
			out.Posts = append(out.Posts, sum)
		}
	}
	photoPage, err := s.engineAdmin.AdminPhotosByQuery(r.Context(), q, 8)
	if err != nil {
		s.fail(w, err)
		return
	}
	photos, err := s.admin.Pool.AdminPhotoRowsByIDs(r.Context(), photoPage.IDs)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, id := range photoPage.IDs {
		if p, ok := photos[id]; ok {
			out.Photos = append(out.Photos, p)
		}
	}
	tags, err := s.admin.Pool.SearchTagNames(r.Context(), q, 8)
	if err != nil {
		s.fail(w, err)
		return
	}
	out.Tags = tags
	writeJSON(w, http.StatusOK, out)
}
