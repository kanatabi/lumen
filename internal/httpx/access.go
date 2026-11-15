// access.go — the transport's access line: one INFO line per incoming
// request, nginx-shaped but slog-native and correlation-carrying. The
// complement to the speaking-errors rule: the access line says WHAT was
// hit (method/path/status/duration + the request-id), the error lines say
// WHY. The query string deliberately never logs (URLs can carry secrets —
// the path is the safe surface); the probes skip (they'd be pure noise).
package httpx

import (
	"net"
	"net/http"
	"strings"
	"time"
)

type accessWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *accessWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		aw := &accessWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			Logger(r).Info("http.access",
				"method", r.Method,
				"path", r.URL.Path,
				"status", aw.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"bytes", aw.bytes,
				// WHO is calling (2026-11-15): the production /admin/* 401
				// beat was a mystery client for hours because the line
				// carried no source. The router mounts
				// ClientIPFromXFFTrustedProxies(1) BEFORE the handlers run
				// — by log time XFF's trusted Traefik hop is trimmed and
				// the last entry is the caller's address. An IP is an
				// identifier, not a payload (the redaction doctrine is
				// untouched). Absent XFF = the pod-direct caller; log
				// RemoteAddr then.
				"client_ip", clientIP(r),
			)
		}()
		next.ServeHTTP(aw, r)
	})
}

// clientIP is the XFF-trusted read: the chain's LAST entry after the
// router's trusted-depth trim. Empty XFF (pod-direct: the probes, the
// worker's engine dials) falls back to RemoteAddr — the peer's socket
// address, which is exactly as honest as it sounds.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.LastIndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[i+1:])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
