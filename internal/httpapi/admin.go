package httpapi

// Admin plumbing (§8): session endpoints, auth middleware, throttle gate.
// Resources mount beside this file (admin_posts.go, admin_photos.go,
// admin_vocab.go, admin_search.go) — one file per resource, doctrine again.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/bao"
	"github.com/kanatabi/lumen/internal/media"
	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
	"github.com/kanatabi/lumen/internal/throttle"
)

// AdminDeps is the /admin group's world: truth (store), the queue (index
// jobs ride write transactions), the signing secret, the login throttle.
type AdminDeps struct {
	Pool      *store.PG
	Queue     *queue.Client
	JWTSecret string
	Limiter   *throttle.Limiter
	// secrets plane (§9): nil Bao ⇒ no /admin/bao surface at all
	Bao *bao.Client
	// media relay (D1-revised §5): nil Media ⇒ uploads answer 503 honestly
	Media         media.Store
	MediaEndpoint string
	MediaBucket   string
	MediaCDNBase  string
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json body required")
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || in.Password == "" {
		writeErr(w, http.StatusBadRequest, "username and password required")
		return
	}
	ipKey := "ip:" + clientIP(r)
	accKey := "acc:" + strings.ToLower(in.Username)
	for _, k := range []string{ipKey, accKey} {
		if ok, wait := s.admin.Limiter.Allow(k); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(secsUp(wait)))
			writeErr(w, http.StatusTooManyRequests, "too many attempts")
			return
		}
	}
	tv, err := s.admin.Pool.Authenticate(r.Context(), in.Username, in.Password)
	if err != nil {
		if errors.Is(err, store.ErrWrongPassword) {
			s.admin.Limiter.Fail(ipKey)
			s.admin.Limiter.Fail(accKey)
			// ONE indistinguishable message for unknown-user and
			// wrong-password: existence is not a signal to leak
			slog.Info("session.deny", "user", in.Username, "ip", clientIP(r))
			writeErr(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		slog.Error("session.error", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	tok, expiresIn, err := session.MintSession(s.admin.JWTSecret, in.Username, tv)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.admin.Limiter.Success(ipKey)
	s.admin.Limiter.Success(accKey)
	slog.Info("session.grant", "user", in.Username)
	// camelCase per mellon's AdminApi (the executable spec — types.ts
	// consumes expiresIn to size the cookie; the snake_case drift here
	// survived only because the integration test mirrored the HANDLER,
	// not the spec. Fixed by the Postman round's contract assert.)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "expiresIn": expiresIn})
}

// adminAuth: signature says WHO (pinned HS256 — session.Parse), truth says
// WHETHER (the live token_version row). Revocation is therefore instant and
// stateless-scaled at once: no session cache to invalidate.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		claims, err := session.Parse(s.admin.JWTSecret, tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		tv, exists, err := s.admin.Pool.TokenVersion(r.Context(), claims.Subject)
		if err != nil || !exists || tv != claims.TV {
			writeErr(w, http.StatusUnauthorized, "unauthorized") // bumped = dead
			return
		}
		next.ServeHTTP(w, r)
	})
}

// DELETE /admin/session — revocation itself (§8: bump tv; mellon also
// always clears its own cookie, which is its half of the promise).
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := session.Parse(s.admin.JWTSecret, tok)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, err := s.admin.Pool.BumpTokenVersion(r.Context(), claims.Subject); err != nil {
		slog.Error("session.revoke.error", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("session.revoke", "user", claims.Subject)
	w.WriteHeader(http.StatusNoContent)
}

// mountAdmin: /admin/* with session POST outside auth, everything inside.
func (s *Server) mountAdmin(r chi.Router) {
	if s.admin == nil {
		return // fixture mode has no admin surface — honest absence, not a 404 theater
	}
	r.Route("/admin", func(a chi.Router) {
		a.Post("/session", s.adminLogin)
		a.Group(func(auth chi.Router) {
			auth.Use(s.adminAuth)
			auth.Delete("/session", s.adminLogout)
			s.mountAdminPosts(auth)
			s.mountAdminPhotos(auth)
			s.mountAdminVocab(auth)
			s.mountAdminSearch(auth)
			s.mountAdminBao(auth)
		})
	})
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func secsUp(d time.Duration) int { return int(d.Seconds()) + 1 }
