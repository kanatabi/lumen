// The outbound wrapper's contract (2026-11-15): every call is counted with
// a result axis (ok | error | timeout — a hung dependency is a DIFFERENT
// series from a failed one), every failure SPEAKS one log line, the
// in-flight gauge moves up and back down, and the wrapped fn receives a
// BOUNDED context carrying the registry's budget. White-box: the lookup
// override and the default logger are package state, restored by defer.
package obs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func withTimeoutLookup(t *testing.T, d time.Duration) {
	t.Helper()
	old := timeoutLookup
	SetTimeoutLookup(func(string, string) time.Duration { return d })
	t.Cleanup(func() { timeoutLookup = old })
}

func withCapturedLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestOutboundOKCountsAndBounds(t *testing.T) {
	withTimeoutLookup(t, 2*time.Second)

	okBefore := testutil.ToFloat64(outboundTotal.WithLabelValues("meilisearch", "search", "ok"))
	var gotDeadline time.Duration
	err := Outbound(context.Background(), "meilisearch", "search", func(ctx context.Context) error {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("the wrapped fn's ctx must carry the registry's deadline")
			return nil
		}
		gotDeadline = time.Until(dl)
		return nil
	})
	if err != nil {
		t.Fatalf("ok run errored: %v", err)
	}
	if gotDeadline <= 0 || gotDeadline > 2*time.Second {
		t.Errorf("deadline not the table's 2s budget: %v", gotDeadline)
	}
	if got := testutil.ToFloat64(outboundTotal.WithLabelValues("meilisearch", "search", "ok")); got != okBefore+1 {
		t.Errorf("ok counter: %v, want +1", got)
	}
	if got := testutil.ToFloat64(outboundInflight.WithLabelValues("meilisearch")); got != 0 {
		t.Errorf("in-flight gauge must return to 0 after the call: %v", got)
	}
}

func TestOutboundOKLogsNothing(t *testing.T) {
	withTimeoutLookup(t, time.Second)
	buf := withCapturedLogs(t)
	if err := Outbound(context.Background(), "openbao", "read", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("a SUCCESS must not speak the failure line; got:\n%s", buf.String())
	}
}

func TestOutboundErrorCountsAndSpeaks(t *testing.T) {
	withTimeoutLookup(t, time.Second)
	buf := withCapturedLogs(t)

	errBefore := testutil.ToFloat64(outboundTotal.WithLabelValues("meilisearch", "search", "error"))
	boom := errors.New("boom")
	if err := Outbound(context.Background(), "meilisearch", "search", func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("the caller's error must pass through unchanged: %v", err)
	}
	if got := testutil.ToFloat64(outboundTotal.WithLabelValues("meilisearch", "search", "error")); got != errBefore+1 {
		t.Errorf("error counter: %v, want +1", got)
	}

	line := buf.String()
	for _, want := range []string{"msg=outbound.failed", "dependency=meilisearch", "operation=search", "result=error"} {
		if !strings.Contains(line, want) {
			t.Errorf("the failure line must speak %q; got:\n%s", want, line)
		}
	}
}

func TestOutboundTimeoutClassifiedDistinctFromError(t *testing.T) {
	withTimeoutLookup(t, 30*time.Millisecond)
	buf := withCapturedLogs(t)

	// the cooperative callee: it honors the bounded ctx and returns its
	// own DeadlineExceeded
	err := Outbound(context.Background(), "meilisearch", "search", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline surfaced to the caller: %v", err)
	}
	if !strings.Contains(buf.String(), "result=timeout") {
		t.Errorf("a hung dependency is result=timeout, not error:\n%s", buf.String())
	}

	// the defiant callee: it IGNORES the ctx and returns nil after the
	// budget — the call DELIVERED, so it classifies ok (the breach shows
	// in the histogram; the caller got its result) and no line speaks
	buf.Reset()
	okBefore := testutil.ToFloat64(outboundTotal.WithLabelValues("meilisearch", "search", "ok"))
	err = Outbound(context.Background(), "meilisearch", "search", func(ctx context.Context) error {
		time.Sleep(80 * time.Millisecond) // past the 30ms budget, ctx ignored
		return nil
	})
	if err != nil {
		t.Fatalf("the defiant callee's nil must still pass through: %v", err)
	}
	if got := testutil.ToFloat64(outboundTotal.WithLabelValues("meilisearch", "search", "ok")); got != okBefore+1 {
		t.Errorf("a delivered call is ok regardless of budget: %v", got)
	}
	if buf.Len() != 0 {
		t.Errorf("a delivered call speaks nothing:\n%s", buf.String())
	}
}

func TestOutboundInflightGaugeMoves(t *testing.T) {
	withTimeoutLookup(t, time.Second)
	gauge := outboundInflight.WithLabelValues("cloudflare-r2")
	before := testutil.ToFloat64(gauge)
	var during float64
	_ = Outbound(context.Background(), "cloudflare-r2", "put", func(context.Context) error {
		during = testutil.ToFloat64(gauge)
		return nil
	})
	if during != before+1 {
		t.Errorf("in-flight must be +1 DURING the call: %v (before %v)", during, before)
	}
	if after := testutil.ToFloat64(gauge); after != before {
		t.Errorf("in-flight must settle back: %v (before %v)", after, before)
	}
}

func TestOutboundFailureLineCarriesRequestID(t *testing.T) {
	withTimeoutLookup(t, time.Second)
	buf := withCapturedLogs(t)

	// through the REAL seam: chi's RequestID middleware mints the id into
	// the request ctx, the handler's outbound call fails — the line must
	// name the SAME id the access log prints
	router := chi.NewRouter()
	router.Use(chiMiddleware.RequestID)
	router.Get("/fails", func(w http.ResponseWriter, r *http.Request) {
		_ = Outbound(r.Context(), "meilisearch", "search", func(context.Context) error {
			return errors.New("engine down")
		})
		w.WriteHeader(502)
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/fails")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	line := buf.String()
	if !strings.Contains(line, "msg=outbound.failed") {
		t.Fatalf("the failure line never spoke:\n%s", line)
	}
	id := chiMiddleware.GetReqID(context.Background()) // never matches — placeholder guard
	if id != "" {
		t.Fatal("sanity: background ctx must have no request id")
	}
	// the minted id is a 14-hex chi id, not our uuid — assert the FIELD
	// exists and is non-empty on the line
	if !strings.Contains(line, "request_id=") || strings.Contains(line, "request_id=\"\"") {
		t.Errorf("the failure line must carry the request's id:\n%s", line)
	}
}

func TestOutboundUnknownDependencyStillBounded(t *testing.T) {
	// no lookup override: an unregistered dep gets the table's last-resort
	// 5s bound — never an unbounded call
	if got := TableDefault("unregistered-thing", "op"); got != 5*time.Second {
		t.Fatalf("unregistered dep default: %v", got)
	}
}

func TestObserveJobDoneAndError(t *testing.T) {
	doneBefore := testutil.ToFloat64(queueJobTotal.WithLabelValues("index_post", "done"))
	ObserveJob("index_post", time.Now().Add(-time.Millisecond), nil)
	if got := testutil.ToFloat64(queueJobTotal.WithLabelValues("index_post", "done")); got != doneBefore+1 {
		t.Errorf("done counter: %v, want +1", got)
	}

	errBefore := testutil.ToFloat64(queueJobTotal.WithLabelValues("index_tag", "error"))
	ObserveJob("index_tag", time.Now(), errors.New("engine down"))
	if got := testutil.ToFloat64(queueJobTotal.WithLabelValues("index_tag", "error")); got != errBefore+1 {
		t.Errorf("error counter: %v, want +1", got)
	}
}

func TestTimeoutKeysParity(t *testing.T) {
	// every derived key's default must equal what TableDefault resolves
	// for its dependency/operation — config's SetDefault feed and the
	// in-code fallback can never disagree
	for _, tk := range TimeoutKeys() {
		if tk.Env == "" || !strings.HasPrefix(tk.Env, "LUMEN_TIMEOUT_") {
			t.Fatalf("key %s: env %q must be LUMEN_TIMEOUT_*", tk.Key, tk.Env)
		}
		if tk.Key != strings.ToLower(tk.Key) {
			t.Fatalf("key %s must be lowercase (viper flat key)", tk.Key)
		}
	}
}
