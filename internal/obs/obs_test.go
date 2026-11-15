// The inbound RED pair's contract (2026-11-15): the counter keys on the
// ROUTE TEMPLATE (never the raw path — cardinality is flat by
// construction), the probes skip (they would flood every rate() with
// noise), and the pool gauges render the store's snapshot honestly.
package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newObsRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Use(Middleware)
	r.Get("/posts/{slug}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	r.Post("/posts/{slug}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201) })
	// the probes exist as real handlers on lumen's routers — the unit
	// test router mirrors that so the skip assertion is honest (200
	// served, series NOT created)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	return r
}

func TestMiddlewareCountsRouteTemplateNotRawPath(t *testing.T) {
	router := newObsRouter()
	key := func() float64 {
		return testutil.ToFloat64(httpRequestTotal.WithLabelValues("GET", "/posts/{slug}", "200"))
	}
	before := key()
	for _, slug := range []string{"one", "two", "observability-is-a-contract"} {
		res := httptest.NewRecorder()
		router.ServeHTTP(res, httptest.NewRequest("GET", "/posts/"+slug, nil))
		if res.Code != 200 {
			t.Fatalf("route broken: %d", res.Code)
		}
	}
	if got := key(); got != before+3 {
		t.Errorf("three DIFFERENT slugs must collapse into ONE route series: %v, want %v", got, before+3)
	}

	// an unmatched path is its own honest series — not a fake route
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest("GET", "/no-such-route", nil))
	if res.Code != 404 {
		t.Fatalf("unmatched should 404: %d", res.Code)
	}
	if got := testutil.ToFloat64(httpRequestTotal.WithLabelValues("GET", "unmatched", "404")); got < 1 {
		t.Errorf("the unmatched series must exist (flat cardinality, one label): %v", got)
	}
}

func TestMiddlewareSkipsProbes(t *testing.T) {
	router := newObsRouter()
	get := func(route, status string) float64 {
		return testutil.ToFloat64(httpRequestTotal.WithLabelValues("GET", route, status))
	}
	beforeHealth, beforeReady := get("/healthz", "200"), get("/readyz", "200")
	for _, probe := range []string{"/healthz", "/readyz"} {
		res := httptest.NewRecorder()
		router.ServeHTTP(res, httptest.NewRequest("GET", probe, nil))
		if res.Code != 200 {
			t.Fatalf("%s should still serve: %d", probe, res.Code)
		}
	}
	if after := get("/healthz", "200"); after != beforeHealth {
		t.Errorf("/healthz must NOT count (it would flood every rate()): %v vs %v", beforeHealth, after)
	}
	if after := get("/readyz", "200"); after != beforeReady {
		t.Errorf("/readyz must NOT count: %v vs %v", beforeReady, after)
	}
}

func TestMiddlewareDurationHistogramObserved(t *testing.T) {
	router := newObsRouter()
	samples := func() int {
		mfs, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, mf := range mfs {
			if mf.GetName() != "http_request_duration_seconds" {
				continue
			}
			for _, m := range mf.GetMetric() {
				labels := map[string]string{}
				for _, l := range m.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				if labels["method"] == "POST" && labels["route"] == "/posts/{slug}" &&
					m.GetHistogram().GetSampleCount() > 0 {
					n++
				}
			}
		}
		return n
	}
	before := samples()
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest("POST", "/posts/hello", nil))
	if after := samples(); after <= before {
		t.Errorf("the duration histogram must observe per request: %d -> %d", before, after)
	}
}

func TestRegisterPoolSamplerRendersSnapshot(t *testing.T) {
	t.Cleanup(func() { RegisterPoolSampler(nil) })
	RegisterPoolSampler(func() PoolSample {
		return PoolSample{Acquired: 3, Idle: 5, Max: 10, EmptyAcquires: 7}
	})

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"db_pool_acquired":            3,
		"db_pool_idle":                5,
		"db_pool_max":                 10,
		"db_pool_acquire_empty_total": 7,
	}
	seen := map[string]float64{}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; !ok {
			continue
		}
		for _, m := range mf.GetMetric() {
			// the empty-acquire series is a COUNTER (cumulative — rate it
			// for saturation); the rest are gauges
			if strings.HasSuffix(mf.GetName(), "_total") {
				seen[mf.GetName()] = m.GetCounter().GetValue()
			} else {
				seen[mf.GetName()] = m.GetGauge().GetValue()
			}
		}
	}
	for name, v := range want {
		got, ok := seen[name]
		if !ok {
			t.Errorf("gauge %s missing from the registry", name)
			continue
		}
		if got != v {
			t.Errorf("%s: %v, want %v", name, got, v)
		}
	}

	// nil fn = the gauges ABSENT (honest, like every unconfigured surface)
	RegisterPoolSampler(nil)
	mfs, err = prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "db_pool_max" {
			t.Error("pool gauges must vanish when no sampler is wired")
		}
	}
}

func TestHandlerServesTheFamilies(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		t.Fatalf("metrics handler: %d", res.StatusCode)
	}
}
