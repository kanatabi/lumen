package httpapi

// /admin/bao — the §9 relay's operations, thin as the doctrine asks:
// paths, read (+ ?version pin, + ?meta history), CAS write. Values are
// relayed, never logged: audit lines carry op+path+actor-adjacent facts+
// version+KEY COUNT — §9 says Bao's audit devices hold the byte-level
// truth; lumen adds noise to none.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/bao"
)

func (s *Server) mountAdminBao(a chi.Router) {
	if s.admin.Bao == nil {
		return // plane not configured: honest absence, not 404 theater
	}
	a.Get("/bao/paths", s.baoPaths)
	a.Get("/bao/{app}/{env}", s.baoRead)
	a.Put("/bao/{app}/{env}", s.baoWrite)
}

func baoAudit(r *http.Request, op string, fields ...any) {
	slog.Info("bao."+op, append([]any{"path", r.URL.Path}, fields...)...)
}

func (s *Server) baoPaths(w http.ResponseWriter, r *http.Request) {
	paths, err := s.admin.Bao.ListPaths(r.Context())
	if err != nil {
		baoAudit(r, "list.error")
		s.fail(w, err)
		return
	}
	baoAudit(r, "list")
	writeJSON(w, http.StatusOK, paths)
}

func (s *Server) baoRead(w http.ResponseWriter, r *http.Request) {
	app, env := chi.URLParam(r, "app"), chi.URLParam(r, "env")
	if r.URL.Query().Has("meta") { // presence, not value: `?meta` is the contract shape (mellon relay)
		m, err := s.admin.Bao.Meta(r.Context(), app, env)
		if err != nil {
			baoAudit(r, "meta.error", "app", app, "env", env)
			s.fail(w, err)
			return
		}
		if m == nil {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		baoAudit(r, "meta", "app", app, "env", env, "versions", len(m.Versions))
		writeJSON(w, http.StatusOK, m)
		return
	}
	version := 0
	if v := r.URL.Query().Get("version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "version must be a non-negative integer")
			return
		}
		version = n
	}
	p, err := s.admin.Bao.Read(r.Context(), app, env, version)
	if err != nil {
		baoAudit(r, "read.error", "app", app, "env", env)
		slog.Error("bao.read.failed", "app", app, "env", env, "err", err)
		s.fail(w, err)
		return
	}
	if p == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	// the RESPONSE carries values (the UI masks them); the LOG counts keys
	// and never reads one — the redaction test polices exactly this line
	baoAudit(r, "read", "app", app, "env", env, "version", p.Version, "keys", len(p.Data))
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) baoWrite(w http.ResponseWriter, r *http.Request) {
	app, env := chi.URLParam(r, "app"), chi.URLParam(r, "env")
	var in struct {
		Data map[string]string `json:"data"`
		CAS  int               `json:"cas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "expected {data, cas}")
		return
	}
	if in.Data == nil {
		writeErr(w, http.StatusBadRequest, "data object required (may be empty)")
		return
	}
	v, err := s.admin.Bao.WriteCAS(r.Context(), app, env, in.Data, in.CAS)
	if err != nil {
		baoAudit(r, "write.error", "app", app, "env", env, "cas", in.CAS)
		slog.Error("bao.write.failed", "app", app, "env", env, "err", err)
		var be *bao.Error
		if errors.As(err, &be) && be.Kind == bao.ErrCheckAndSet {
			writeErr(w, http.StatusConflict, "stale cas — someone wrote first")
			return
		}
		s.fail(w, err)
		return
	}
	baoAudit(r, "write", "app", app, "env", env, "version", v, "keys", len(in.Data))
	writeJSON(w, http.StatusOK, map[string]int{"version": v})
}
