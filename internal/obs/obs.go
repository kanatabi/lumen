// Package obs is the observability machinery (2026-11-15): ONE Prometheus
// registry with a uniform metric vocabulary across the estate — the
// outbound RED pair for every outside dependency (outbound_request_*),
// the inbound RED pair for lumen's own HTTP surface (http_request_*),
// the queue's job counters, and the Postgres pool gauges. /metrics serves
// on the UNROUTED admin port (default 9090) per the no-sidecar contract:
// Traefik never routes it, the pod annotations opt the scrape in.
//
// Label doctrine (owner ruling, 2026-11-15): the label axes are
// `dependency` (which outside system), `operation` (an ENUMERATED verb —
// never a slug, id, or query value), and `result` (ok|error|timeout).
// Cardinality is flat by construction. The static `service` label makes
// cross-service panels a sum-by instead of pod-name regexes.
//
// The count doctrine is untouched: metrics live in-process, nothing lands
// in Postgres (the facet-at-read ruling's scope was DATA, not ops).
package obs

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServiceName is the static `service` label value — one constant per
// codebase (lumen here; earendil and mellon carry their own). The scrape
// target already identifies the pod; this label exists so cross-service
// panels can `sum by (service, dependency)` without pod regexes.
const ServiceName = "lumen"

var labelsService = prometheus.Labels{"service": ServiceName}

var (
	outboundDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "outbound_request_duration_seconds",
		Help:        "Latency of outbound calls, by dependency and operation.",
		Buckets:     []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		ConstLabels: labelsService,
	}, []string{"dependency", "operation"})
	outboundTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "outbound_request_total",
		Help:        "Outbound call outcomes (result=ok|error|timeout).",
		ConstLabels: labelsService,
	}, []string{"dependency", "operation", "result"})
	outboundInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "outbound_inflight",
		Help:        "Outbound calls currently in flight, by dependency.",
		ConstLabels: labelsService,
	}, []string{"dependency"})

	httpRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "http_request_total",
		Help:        "Inbound requests by route TEMPLATE (chi's pattern — never raw paths).",
		ConstLabels: labelsService,
	}, []string{"method", "route", "status"})
	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "http_request_duration_seconds",
		Help:        "Inbound request latency by route template.",
		Buckets:     []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		ConstLabels: labelsService,
	}, []string{"method", "route"})

	queueJobTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "queue_job_total",
		Help:        "Index-job attempt outcomes (result=done|error; river retries an error).",
		ConstLabels: labelsService,
	}, []string{"kind", "result"})
	queueJobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "queue_job_duration_seconds",
		Help:        "Index-job attempt latency by kind.",
		Buckets:     []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		ConstLabels: labelsService,
	}, []string{"kind"})
)

func init() {
	prometheus.MustRegister(
		outboundDuration, outboundTotal, outboundInflight,
		httpRequestTotal, httpRequestDuration,
		queueJobTotal, queueJobDuration,
	)
}

// Handler is /metrics — mounted on the unrouted admin listener.
func Handler() http.Handler {
	return promhttp.Handler()
}

// ObserveJob records one queue-job attempt (the mirror of the worker's
// job.done/job.failed log lines). err != nil means river will retry.
func ObserveJob(kind string, started time.Time, err error) {
	queueJobDuration.WithLabelValues(kind).Observe(time.Since(started).Seconds())
	if err != nil {
		queueJobTotal.WithLabelValues(kind, "error").Inc()
		return
	}
	queueJobTotal.WithLabelValues(kind, "done").Inc()
}

// PoolSample is the store's pool snapshot — decoupled from pgx so this
// package stays testable without a database. EmptyAcquires is CUMULATIVE
// (pgxpool exposes no live waiter count): rate it for saturation.
type PoolSample struct {
	Acquired      float64
	Idle          float64
	Max           float64
	EmptyAcquires float64
}

type poolCollector struct {
	fn           func() PoolSample
	acquiredDesc *prometheus.Desc
	idleDesc     *prometheus.Desc
	maxDesc      *prometheus.Desc
	emptyDesc    *prometheus.Desc
}

// RegisterPoolSampler wires the Postgres pool gauges (cmd/lumen's
// composition root calls this once per process with the store's Stat fn).
// Nil fn = the gauges absent (honest, like every unconfigured surface).
func RegisterPoolSampler(fn func() PoolSample) {
	// Unregister BEFORE clearing: a typed-nil *poolCollector in the
	// Collector interface panics the registry's Describe path — the
	// previous instance must be non-nil to unregister
	if poolCollectorInstance != nil {
		prometheus.Unregister(poolCollectorInstance)
	}
	if fn == nil {
		poolCollectorInstance = nil
		return
	}
	poolCollectorInstance = &poolCollector{
		fn: fn,
		acquiredDesc: prometheus.NewDesc("db_pool_acquired",
			"Connections currently checked out of the pool.", nil, labelsService),
		idleDesc: prometheus.NewDesc("db_pool_idle",
			"Idle connections sitting in the pool.", nil, labelsService),
		maxDesc: prometheus.NewDesc("db_pool_max",
			"The pool's configured connection ceiling.", nil, labelsService),
		emptyDesc: prometheus.NewDesc("db_pool_acquire_empty_total",
			"Cumulative acquires that found no idle connection (rate = saturation).", nil, labelsService),
	}
	prometheus.MustRegister(poolCollectorInstance)
}

var poolCollectorInstance *poolCollector

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.acquiredDesc
	ch <- c.idleDesc
	ch <- c.maxDesc
	ch <- c.emptyDesc
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.fn()
	ch <- prometheus.MustNewConstMetric(c.acquiredDesc, prometheus.GaugeValue, s.Acquired)
	ch <- prometheus.MustNewConstMetric(c.idleDesc, prometheus.GaugeValue, s.Idle)
	ch <- prometheus.MustNewConstMetric(c.maxDesc, prometheus.GaugeValue, s.Max)
	ch <- prometheus.MustNewConstMetric(c.emptyDesc, prometheus.CounterValue, s.EmptyAcquires)
}

// Middleware is the inbound RED pair: duration histogram + status counter,
// keyed on the ROUTE TEMPLATE (chi's pattern — /posts/{slug}, never the
// raw path: cardinality is flat by construction). The probes skip — same
// doctrine as the access log: they would flood every rate() with noise.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		httpRequestDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
		httpRequestTotal.WithLabelValues(r.Method, route, strconv.Itoa(ww.Status())).Inc()
	})
}
