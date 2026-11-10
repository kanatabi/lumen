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
	a.Get("/bao/summary", h.secretSummary)
	a.Post("/bao/apps", h.secretOnboard)
	a.Post("/bao/{app}/envs", h.secretAddEnv)
	a.Get("/bao/{app}/{env}", h.secretRead)
	a.Put("/bao/{app}/{env}", h.secretWrite)
	a.Put("/bao/{app}/{env}/pin", h.secretPin)
	a.Delete("/bao/{app}/{env}/pin", h.secretUnpin)
}

// secretFail maps OUR secrets sentinels to statuses (the vocabulary plan:
// 400 slug, 404 unknown-path/version, 409 every "you lost a race or the
// namespace exists"). True means the response is written.
func secretFail(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case errors.Is(err, adminsvc.ErrInvalidSlug):
		httpx.WriteErr(w, http.StatusBadRequest, "app/env must match ^[a-z][a-z0-9-]{0,31}$")
	case errors.Is(err, adminsvc.ErrSecretNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "secret path not found")
	case errors.Is(err, adminsvc.ErrVersionUnknown):
		httpx.WriteErr(w, http.StatusNotFound, "pin version is not a live version of this secret")
	case errors.Is(err, adminsvc.ErrAppUnknown):
		httpx.WriteErr(w, http.StatusNotFound, "app has no envs — onboard it first")
	case errors.Is(err, adminsvc.ErrAppExists):
		httpx.WriteErr(w, http.StatusConflict, "app namespace already exists")
	case errors.Is(err, adminsvc.ErrEnvExists):
		httpx.WriteErr(w, http.StatusConflict, "env already exists for this app")
	case errors.Is(err, adminsvc.ErrStaleCAS):
		httpx.WriteErr(w, http.StatusConflict, "stale cas — someone wrote first")
	case errors.Is(err, adminsvc.ErrMirrorStale):
		// err carries the env: the story mellon shows verbatim (§7 partial)
		httpx.WriteErr(w, http.StatusConflict, err.Error()+" — head saved, retry after reload")
	default:
		return false
	}
	return true
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
	default:
		slog.Info("bao.write.error", "path", r.URL.Path, "app", app, "env", env, "cas", in.CAS)
		if secretFail(w, r, err) {
			return
		}
		slog.Error("bao.write.failed", "app", app, "env", env, "err", err)
		httpx.Fail(w, r, err)
	}
}

// ---- control plane (A4): onboard, add-env, pin, unpin ----

func (h *Handler) secretOnboard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		App  string   `json:"app"`
		Envs []string `json:"envs"` // optional; absent = the D4 trio
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "expected {app, envs?}")
		return
	}
	if err := h.svc.OnboardApp(r.Context(), in.App, in.Envs); err != nil {
		slog.Info("bao.onboard.error", "app", in.App, "err", err)
		if secretFail(w, r, err) {
			return
		}
		slog.Error("bao.onboard.failed", "app", in.App, "err", err)
		httpx.Fail(w, r, err)
		return
	}
	// names and counts only — the bundles are empty BY CONSTRUCTION anyway
	slog.Info("bao.onboard", "app", in.App)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"app": in.App, "envs": defaultEnvNames(in.Envs)})
}

func defaultEnvNames(envs []string) []string {
	if len(envs) == 0 {
		return adminsvc.DefaultEnvs
	}
	return envs
}

func (h *Handler) secretAddEnv(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	var in struct {
		Env string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "expected {env}")
		return
	}
	if err := h.svc.AddEnv(r.Context(), app, in.Env); err != nil {
		slog.Info("bao.addenv.error", "app", app, "env", in.Env, "err", err)
		if secretFail(w, r, err) {
			return
		}
		slog.Error("bao.addenv.failed", "app", app, "env", in.Env, "err", err)
		httpx.Fail(w, r, err)
		return
	}
	slog.Info("bao.addenv", "app", app, "env", in.Env)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"app": app, "env": in.Env})
}

func (h *Handler) secretPin(w http.ResponseWriter, r *http.Request) {
	app, env := chi.URLParam(r, "app"), chi.URLParam(r, "env")
	var in struct {
		Version int `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "expected {version}")
		return
	}
	if err := h.svc.Pin(r.Context(), app, env, in.Version); err != nil {
		slog.Info("bao.pin.error", "app", app, "env", env, "version", in.Version, "err", err)
		if secretFail(w, r, err) {
			return
		}
		slog.Error("bao.pin.failed", "app", app, "env", env, "err", err)
		httpx.Fail(w, r, err)
		return
	}
	// D1 on the wire: the response says what INTENT was recorded, never
	// implies a routing change.
	slog.Info("bao.pin", "app", app, "env", env, "version", in.Version)
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"pinnedVersion": in.Version})
}

func (h *Handler) secretUnpin(w http.ResponseWriter, r *http.Request) {
	app, env := chi.URLParam(r, "app"), chi.URLParam(r, "env")
	if err := h.svc.Unpin(r.Context(), app, env); err != nil {
		slog.Info("bao.unpin.error", "app", app, "env", env, "err", err)
		if secretFail(w, r, err) {
			return
		}
		slog.Error("bao.unpin.failed", "app", app, "env", env, "err", err)
		httpx.Fail(w, r, err)
		return
	}
	slog.Info("bao.unpin", "app", app, "env", env)
	w.WriteHeader(http.StatusNoContent)
}

// secretSummary: the estate matrix feed (C1) — every path, counts only.
// Logs name the operation; there is nothing else to log.
func (h *Handler) secretSummary(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.EstateSummary(r.Context())
	if err != nil {
		slog.Error("bao.summary.failed", "err", err)
		httpx.Fail(w, r, err)
		return
	}
	slog.Info("bao.summary", "paths", len(rows))
	httpx.WriteJSON(w, http.StatusOK, rows)
}
