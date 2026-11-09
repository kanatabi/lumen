package admin

// /admin/bao — the §9 relay's HTTP face, thin as the doctrine asks. Values
// ride RESPONSES (the UI masks them); logs carry op + path + version +
// key-COUNTS only — Bao's audit devices hold the byte-level truth, lumen
// adds noise to none.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/httpx"
)

func (h *Handler) mountSecrets(a chi.Router) {
	a.Get("/bao/paths", h.secretPaths)
	a.Get("/bao/{app}/{env}", h.secretRead)
	a.Put("/bao/{app}/{env}", h.secretWrite)
}

func (h *Handler) secretPaths(w http.ResponseWriter, r *http.Request) {
	paths, err := h.svc.SecretPaths(r.Context())
	if err != nil {
		slog.Info("bao.list.error", "path", r.URL.Path)
		httpx.Fail(w, r, err)
		return
	}
	slog.Info("bao.list", "path", r.URL.Path)
	httpx.WriteJSON(w, http.StatusOK, paths)
}

func (h *Handler) secretRead(w http.ResponseWriter, r *http.Request) {
	app, env := chi.URLParam(r, "app"), chi.URLParam(r, "env")
	if r.URL.Query().Has("meta") { // presence, not value: mellon's relay shape
		meta, err := h.svc.SecretMeta(r.Context(), app, env)
		if err != nil {
			slog.Error("bao.meta.failed", "app", app, "env", env, "err", err)
			httpx.Fail(w, r, err)
			return
		}
		if meta == nil {
			httpx.WriteErr(w, http.StatusNotFound, "not found")
			return
		}
		slog.Info("bao.meta", "path", r.URL.Path, "app", app, "env", env, "versions", len(meta.Versions))
		httpx.WriteJSON(w, http.StatusOK, meta)
		return
	}
	version := 0
	if v := r.URL.Query().Get("version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			httpx.WriteErr(w, http.StatusBadRequest, "version must be a non-negative integer")
			return
		}
		version = n
	}
	payload, err := h.svc.ReadSecret(r.Context(), app, env, version)
	if err != nil {
		slog.Error("bao.read.failed", "app", app, "env", env, "err", err)
		httpx.Fail(w, r, err)
		return
	}
	if payload == nil {
		httpx.WriteErr(w, http.StatusNotFound, "not found")
		return
	}
	// values ride the RESPONSE; the LOG counts keys and reads none
	slog.Info("bao.read", "path", r.URL.Path, "app", app, "env", env, "version", payload.Version, "keys", len(payload.Data))
	httpx.WriteJSON(w, http.StatusOK, payload)
}

func (h *Handler) secretWrite(w http.ResponseWriter, r *http.Request) {
	app, env := chi.URLParam(r, "app"), chi.URLParam(r, "env")
	var in struct {
		Data map[string]string `json:"data"`
		CAS  int               `json:"cas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "expected {data, cas}")
		return
	}
	if in.Data == nil {
		httpx.WriteErr(w, http.StatusBadRequest, "data object required (may be empty)")
		return
	}
	version, err := h.svc.WriteSecret(r.Context(), app, env, in.Data, in.CAS)
	switch {
	case err == nil:
		slog.Info("bao.write", "path", r.URL.Path, "app", app, "env", env, "version", version, "keys", len(in.Data))
		httpx.WriteJSON(w, http.StatusOK, map[string]int{"version": version})
	case errors.Is(err, adminsvc.ErrStaleCAS):
		slog.Info("bao.write.error", "path", r.URL.Path, "app", app, "env", env, "cas", in.CAS)
		httpx.WriteErr(w, http.StatusConflict, "stale cas — someone wrote first")
	default:
		slog.Info("bao.write.error", "path", r.URL.Path, "app", app, "env", env, "cas", in.CAS)
		slog.Error("bao.write.failed", "app", app, "env", env, "err", err)
		httpx.Fail(w, r, err)
	}
}
