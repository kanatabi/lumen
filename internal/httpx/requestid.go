// requestid.go — the correlation seam. chi's RequestID middleware (mounted
// in the router) already mints or ADOPTS an `X-Request-Id` into the request
// context; these helpers make it USEFUL: every log line down the stack
// extracts it from the ctx, the response echoes it back, and the queue's
// enqueue sites stamp it into the job args so a worker's logs chain to the
// HTTP request that spawned the work.
//
// Redaction note: the request-id is an identifier, not a payload — the
// never-log-values rules (§9) are untouched by any of this.
package httpx

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// Logger returns a slog carrying the request's correlation id — the ONE
// way handlers log, so every error line prints the same request_id the
// caller (mellon's relay, earendil's proxy) holds.
func Logger(r *http.Request) *slog.Logger {
	id := middleware.GetReqID(r.Context())
	if id == "" {
		return slog.Default()
	}
	return slog.Default().With("request_id", id)
}

// LoggerCtx: the context-scoped form for code that holds a ctx but not
// the request (services, store) — empty id means the default logger.
func LoggerCtx(ctx context.Context) *slog.Logger {
	id := middleware.GetReqID(ctx)
	if id == "" {
		return slog.Default()
	}
	return slog.Default().With("request_id", id)
}

// EchoRequestID sets the response's X-Request-Id — the caller sees the
// SAME id its own logs can grep: the chain is one id end to end.
func EchoRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set("X-Request-Id", id)
		}
		next.ServeHTTP(w, r)
	})
}

// RequestIDFromCtx: the enqueue-side read (the queue's InsertTx stamps it
// into the job args — the worker's logs chain to the creating request).
func RequestIDFromCtx(ctx context.Context) string {
	return middleware.GetReqID(ctx)
}
