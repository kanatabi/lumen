package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/kanatabi/lumen/internal/httpx"
)

// The correlation seam's contract (the observability amendment,
// 2026-11-15): every request carries an id; the response echoes it; the
// catch-all's log line carries it; the ctx read feeds the queue's stamps.

func TestRequestIDAdopted(t *testing.T) {
	var seen string
	// the REAL chain: chi's RequestID adopts-or-mints, the echo publishes
	h := middleware.RequestID(httpx.EchoRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = middleware.GetReqID(r.Context())
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "caller-id-42")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Header().Get("X-Request-Id") != "caller-id-42" {
		t.Fatalf("the response must echo the ADOPTED id, got %q", rr.Header().Get("X-Request-Id"))
	}
	if seen != "caller-id-42" {
		t.Fatalf("the caller's id must be adopted, got %q", seen)
	}
}

func TestRequestIDMintedAndEchoed(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if middleware.GetReqID(r.Context()) == "" {
			t.Fatal("no id in the context")
		}
	})
	rr := httptest.NewRecorder()
	middleware.RequestID(httpx.EchoRequestID(h)).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Header().Get("X-Request-Id") == "" {
		t.Fatal("the response must echo the id")
	}
}

func TestLoggerCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	var throughCtx context.Context
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		throughCtx = r.Context()
		httpx.Logger(r).Error("probe.line", "err", "boom")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !strings.Contains(buf.String(), "probe.line") || !strings.Contains(buf.String(), "request_id=") {
		t.Fatalf("the log line must carry the request_id: %s", buf.String())
	}
	if httpx.RequestIDFromCtx(throughCtx) == "" {
		t.Fatal("the ctx-scoped read must find the id")
	}
}

func TestFailLogsUnhandledWithRequestID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	r := httptest.NewRequest(http.MethodPost, "/admin/photos", nil)
	r = r.WithContext(context.WithValue(r.Context(), middleware.RequestIDKey, "req-xyz"))
	httpx.Fail(httptest.NewRecorder(), r, errors.New("the S3 said no"))

	if !strings.Contains(buf.String(), "http.unhandled") ||
		!strings.Contains(buf.String(), "request_id=req-xyz") ||
		!strings.Contains(buf.String(), "the S3 said no") {
		t.Fatalf("the catch-all must log err + request_id: %s", buf.String())
	}
}
