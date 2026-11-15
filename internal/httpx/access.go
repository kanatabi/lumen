// access.go — the transport's access line: one INFO line per incoming
// request, nginx-shaped but slog-native and correlation-carrying. The
// complement to the speaking-errors rule: the access line says WHAT was
// hit (method/path/status/duration + the request-id), the error lines say
// WHY. The query string deliberately never logs (URLs can carry secrets —
// the path is the safe surface); the probes skip (they'd be pure noise).
package httpx

import (
	"net/http"
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
			)
		}()
		next.ServeHTTP(aw, r)
	})
}
