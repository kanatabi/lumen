// The outbound registry — the ONE file (owner ruling, 2026-11-15): every
// outside dependency's name, every operation, and every timeout DEFAULT
// lives here. internal/config derives its LUMEN_TIMEOUT_* bindings from
// TimeoutKeys(); adding a dependency (or an operation) is ONE edit in this
// file plus its adapter's call site.
//
// Timeout precedence (the user's directive): LUMEN_TIMEOUT_<DEP>_<OP>
// overrides LUMEN_TIMEOUT_<DEP>, which overrides the in-code default — and
// viper's default AllowEmptyEnv=false makes an EMPTY env value fall through
// to the default, so "missing key or empty value → default" is native.
package obs

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// The dependency names — the first label axis. Bounded set, one value per
// outside system lumen dials.
const (
	DepMeilisearch = "meilisearch"
	DepR2          = "cloudflare-r2"
	DepBao         = "openbao"
	// DepAdminPlane is the cms plane's bearer introspection edge (cms dials
	// the admin plane's /admin/session/verify).
	DepAdminPlane = "lumen-admin"
)

// The operation names — the second label axis. ENUMERATED verbs only:
// never a slug, id, or query value (cardinality is flat by construction).
const (
	OpMeiliSearch      = "search"
	OpMeiliFacets      = "facets"
	OpMeiliIndexUpsert = "index.upsert"
	OpMeiliIndexDelete = "index.delete"
	OpMeiliEnsure      = "ensure"
	OpMeiliReady       = "ready"
	OpR2Put            = "put"
	OpR2EnsureBucket   = "ensure_bucket"
	OpBaoLogin         = "login"
	OpBaoList          = "list"
	OpBaoRead          = "read"
	OpBaoWrite         = "write"
	OpIntrospect       = "introspect"
)

// TimeoutEntry is one dependency's budget: a dep-wide default plus optional
// per-op overrides. Defaults exist ONLY here (the single-file rule).
type TimeoutEntry struct {
	Dependency string
	Default    time.Duration
	// OpOverrides: operation → budget. Absent op = the dep default.
	OpOverrides map[string]time.Duration
}

// timeoutTable — the defaults ARE today's behavior where a budget already
// exists (the introspection client's 5 s carries over unchanged); where
// none existed (meili, r2, bao) this table becomes the first hard bound.
var timeoutTable = []TimeoutEntry{
	{
		Dependency: DepMeilisearch,
		Default:    2 * time.Second,
		OpOverrides: map[string]time.Duration{
			OpMeiliIndexUpsert: 10 * time.Second, // multi-doc upserts + task waits
			OpMeiliEnsure:      30 * time.Second, // boot-time settings application
		},
	},
	{
		Dependency: DepR2,
		Default:    10 * time.Second,
		OpOverrides: map[string]time.Duration{
			OpR2Put: 30 * time.Second, // multi-MB photo uploads are slow by nature
		},
	},
	{Dependency: DepBao, Default: 5 * time.Second},
	{Dependency: DepAdminPlane, Default: 5 * time.Second}, // matches authz's own budget
}

// TimeoutKey is one config binding derived from the table: the viper key,
// its env name, and the fallback default. The viper keys are FLAT with
// underscores (timeout_meilisearch, timeout_meilisearch_index_upsert) —
// dotted keys would collide in viper's traversal (a dep-default scalar and
// its op-override map cannot share one path in a YAML doc).
type TimeoutKey struct {
	Key     string // "timeout_meilisearch" | "timeout_meilisearch_index_upsert"
	Env     string // LUMEN_TIMEOUT_MEILISEARCH | LUMEN_TIMEOUT_MEILISEARCH_INDEX_UPSERT
	Default time.Duration
}

// TimeoutKeys derives every binding from the table — the machine-readable
// half of the registry (internal/config's Bindings rows point at these).
func TimeoutKeys() []TimeoutKey {
	var out []TimeoutKey
	for _, e := range timeoutTable {
		out = append(out, TimeoutKey{
			Key:     "timeout_" + e.Dependency,
			Env:     "LUMEN_TIMEOUT_" + envPiece(e.Dependency),
			Default: e.Default,
		})
		ops := make([]string, 0, len(e.OpOverrides))
		for op := range e.OpOverrides {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			out = append(out, TimeoutKey{
				Key:     "timeout_" + e.Dependency + "_" + KeyPiece(op),
				Env:     "LUMEN_TIMEOUT_" + envPiece(e.Dependency) + "_" + envPiece(op),
				Default: e.OpOverrides[op],
			})
		}
	}
	return out
}

// KeyPiece is the registry's op-name → key piece ("index.upsert" →
// "index_upsert") — exported so the config layer's Timeout() derives its
// flat keys with the SAME normalization (one spelling rule, two consumers).
func KeyPiece(s string) string {
	return strings.ReplaceAll(s, ".", "_")
}

func envPiece(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, ".", "_"))
}

// TimeoutLookup resolves a call's budget at call time: op override first,
// dep default second, and the registry's own table last (before the
// composition root wires config in — tests and unwired paths behave).
type TimeoutLookup func(dependency, operation string) time.Duration

var timeoutLookup TimeoutLookup = tableLookup

// SetTimeoutLookup is the composition root's one wiring call
// (cmd/lumen: obs.SetTimeoutLookup(cfg.Timeout)) — never per-request.
func SetTimeoutLookup(fn TimeoutLookup) {
	if fn != nil {
		timeoutLookup = fn
	}
}

// TableDefault is the registry's own fallback for a dependency/operation —
// what config.Timeout lands on when no key is bound (exported so the
// config layer's fallback and this table can never disagree).
func TableDefault(dependency, operation string) time.Duration {
	return tableLookup(dependency, operation)
}

func tableLookup(dependency, operation string) time.Duration {
	for _, e := range timeoutTable {
		if e.Dependency != dependency {
			continue
		}
		if d, ok := e.OpOverrides[operation]; ok {
			return d
		}
		return e.Default
	}
	return 5 * time.Second // an unregistered dep still gets a sane bound
}

// Outbound instruments ONE outside call — the wrapper every adapter
// (search, media, bao, authz) wraps its dial in: the deadline comes from
// the timeout table, the latency histogram + the outcome counter + the
// in-flight gauge record the call, and the wrapped fn receives the BOUNDED
// context so a hung dependency dies at the deadline instead of hanging the
// request (the hung-upstream-hangs-SSR class dies here).
//
// Every failure SPEAKS (the user's directive #2): the result counter above
// is guaranteed, and one compact log line fires — result=timeout is a
// DIFFERENT problem from result=error and gets its own series. The rich
// site logs (media.put.failed with bucket+key, the deny reasons) stay:
// the generic line is the alert's grep, the rich line is the human's.
func Outbound(ctx context.Context, dependency, operation string, run func(ctx context.Context) error) error {
	cctx, cancel := context.WithTimeout(ctx, timeoutLookup(dependency, operation))
	defer cancel()

	start := time.Now()
	inflightGauge := outboundInflight.WithLabelValues(dependency)
	inflightGauge.Inc()
	err := run(cctx)
	inflightGauge.Dec()
	outboundDuration.WithLabelValues(dependency, operation).Observe(time.Since(start).Seconds())

	result := "ok"
	switch {
	case err == nil:
		// completed — even past budget (a callee that ignores its bounded
		// ctx and still returns data DELIVERED; the latency breach shows
		// in the histogram, the caller got its result)
	case errors.Is(err, context.DeadlineExceeded):
		// the hung story: the bounded ctx expired and the callee (or its
		// transport) surfaced it — result=timeout is its OWN series, a
		// different problem from result=error
		result = "timeout"
	default:
		result = "error"
	}
	outboundTotal.WithLabelValues(dependency, operation, result).Inc()
	if err != nil {
		// the request-id rides the ctx when a request exists (workers get
		// the plain default logger — their job_id/request_id live in their
		// own lines). Direct chi read, not httpx.LoggerCtx: httpx imports
		// search, and obs must stay cycle-free beneath it.
		logger := slog.Default()
		if requestID := chiMiddleware.GetReqID(ctx); requestID != "" {
			logger = logger.With("request_id", requestID)
		}
		logger.Error("outbound.failed",
			"dependency", dependency,
			"operation", operation,
			"result", result,
			"duration_ms", time.Since(start).Milliseconds(),
			"err", err,
		)
	}
	return err
}
