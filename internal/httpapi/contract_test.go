// contract_test.go — api.test.ts, ported. The earendil mock's vitest suite IS
// the spec (workspace rule: lumen read endpoints serve BlogApi's exact shapes
// and the FE suite run against real lumen is the acceptance test). Every
// `it()` here maps 1:1 to a JS expectation; the SAME suite runs against the
// in-memory twin (offline, always) and the Postgres-backed server (integration
// tag, after seed). If this file and earendil's api.test.ts disagree, whichever
// moved is the bug — the mirror ritual applies to tests too.
package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kanatabi/lumen/internal/httpapi"
	"github.com/kanatabi/lumen/internal/service"
	"github.com/kanatabi/lumen/internal/store"
)

var linkRE = regexp.MustCompile(`\[.+\]\(.+\)`)

func round2(f float64) float64 { return math.Round(f*100) / 100 }

const fixtureRel = "../../testdata/earendil-fixture.json"

func memRouter(tb testing.TB) http.Handler {
	tb.Helper()
	p, err := filepath.Abs(fixtureRel)
	if err != nil {
		tb.Fatal(err)
	}
	src, err := store.Mem(p)
	if err != nil {
		tb.Fatal(err)
	}
	return httpapi.New(service.New(src)).Router()
}

// runContractSuite is THE harness: one body, both stores.
func runContractSuite(t *testing.T, h http.Handler) {
	t.Helper()

	get := func(path string) (int, []byte) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Body)
		return rec.Code, body
	}
	mustGet := func(path string) map[string]any {
		code, body := get(path)
		if code != 200 {
			t.Fatalf("GET %s: status %d", path, code)
		}
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return out
	}
	slugs := func(m map[string]any) []string {
		raw, _ := m["items"].([]any)
		var out []string
		for _, it := range raw {
			out = append(out, it.(map[string]any)["slug"].(string))
		}
		return out
	}

	// ---- getPosts ----
	t.Run("returns all posts with correct total (absent limit = the corpus)", func(t *testing.T) {
		m := mustGet("/api/posts")
		if int(m["total"].(float64)) != 12 {
			t.Errorf("total = %v, want 12", m["total"])
		}
		got := slugs(m)
		want := []string{"observability-is-a-contract", "shape-of-a-useful-queue",
			"when-latency-becomes-a-product-decision", "designing-for-the-failure-you-can-see",
			"a-small-field-guide-to-idempotency"}
		if !slices.Equal(got[:5], want) {
			t.Errorf("first five = %v, want %v", got[:5], want)
		}
	})

	t.Run("honors offset and limit", func(t *testing.T) {
		m := mustGet("/api/posts?offset=1&limit=2")
		if int(m["total"].(float64)) != 12 {
			t.Errorf("total = %v, want 12 (total = full filtered count)", m["total"])
		}
		want := []string{"shape-of-a-useful-queue", "when-latency-becomes-a-product-decision"}
		if !slices.Equal(slugs(m), want) {
			t.Errorf("slugs = %v, want %v", slugs(m), want)
		}
	})

	t.Run("summaries do not carry detail fields", func(t *testing.T) {
		m := mustGet("/api/posts?limit=1")
		item := m["items"].([]any)[0].(map[string]any)
		for _, banned := range []string{"content", "tags"} {
			if _, ok := item[banned]; ok {
				t.Errorf("summary carries %q", banned)
			}
		}
		// and the seven §1 keys are ALWAYS present (FE types are non-optional)
		for _, need := range []string{"slug", "title", "subtitle", "preview", "date", "readTime", "thumbnail"} {
			if _, ok := item[need]; !ok {
				t.Errorf("summary missing key %q", need)
			}
		}
	})

	t.Run("filters by tag(s), OR-matched", func(t *testing.T) {
		m := mustGet("/api/posts?tag=queues")
		if int(m["total"].(float64)) != 1 || slugs(m)[0] != "shape-of-a-useful-queue" {
			t.Errorf("tag=queues → %v", slugs(m))
		}
		m = mustGet("/api/posts?tag=queues&tag=latency")
		want := []string{"shape-of-a-useful-queue", "when-latency-becomes-a-product-decision"}
		if !slices.Equal(slugs(m), want) {
			t.Errorf("OR union = %v want %v", slugs(m), want)
		}
		m = mustGet("/api/posts?tag=nope")
		if len(slugs(m)) != 0 {
			t.Errorf("unknown tag should be empty, got %v", slugs(m))
		}
	})

	t.Run("filters by inclusive date window", func(t *testing.T) {
		m := mustGet("/api/posts?from=2024-01-09&to=2024-06-02")
		if int(m["total"].(float64)) != 5 {
			t.Errorf("window total = %v want 5", m["total"])
		}
	})

	t.Run("combines tags and dates", func(t *testing.T) {
		m := mustGet("/api/posts?tag=distributed-systems&from=2023-12-01")
		want := []string{"shape-of-a-useful-queue", "the-day-the-clock-jumped"}
		if !slices.Equal(slugs(m), want) {
			t.Errorf("tag+from = %v want %v", slugs(m), want)
		}
	})

	t.Run("sorts explicitly when asked, keeps curation otherwise", func(t *testing.T) {
		m := mustGet("/api/posts?limit=100")
		if slugs(m)[0] != "observability-is-a-contract" {
			t.Error("default order must be curation, not sorted")
		}
		m = mustGet("/api/posts?sort=quickest&limit=100")
		items := m["items"].([]any)
		prev := -1
		for _, it := range items {
			rt := int(it.(map[string]any)["readTime"].(float64))
			if rt < prev {
				t.Fatalf("quickest not ascending at %d", rt)
			}
			prev = rt
		}
		m = mustGet("/api/posts?sort=oldest&limit=100")
		var dates []string
		for _, it := range m["items"].([]any) {
			dates = append(dates, it.(map[string]any)["date"].(string))
		}
		if !slices.IsSorted(dates) {
			t.Error("oldest must be date-ascending")
		}
	})

	t.Run("rejects unknown sort — a typo must not pose as curation", func(t *testing.T) {
		code, _ := get("/api/posts?sort=shuffled")
		if code != http.StatusBadRequest {
			t.Errorf("bad sort status = %d want 400", code)
		}
	})

	t.Run("filters by inclusive read-time window", func(t *testing.T) {
		m := mustGet("/api/posts?readMin=10&readMax=12&limit=100")
		for _, it := range m["items"].([]any) {
			rt := int(it.(map[string]any)["readTime"].(float64))
			if rt < 10 || rt > 12 {
				t.Fatalf("read window leak: %d", rt)
			}
		}
		if int(m["total"].(float64)) != len(m["items"].([]any)) {
			t.Error("total must equal page items when limit covers all")
		}
	})

	// ---- search ----
	t.Run("searches with ranking, totals, and pagination", func(t *testing.T) {
		m := mustGet("/api/search?q=queue")
		if int(m["total"].(float64)) < 1 || slugs(m)[0] != "shape-of-a-useful-queue" {
			t.Errorf("q=queue → %v", slugs(m))
		}
		m = mustGet("/api/search?q=zzzz-nothing")
		if len(slugs(m)) != 0 {
			t.Error("no-match search must be empty")
		}
		m = mustGet("/api/search?q=a&offset=1&limit=1")
		if len(slugs(m)) != 1 {
			t.Error("paged search must carry exactly limit")
		}
	})

	t.Run("search honors sort override over relevance", func(t *testing.T) {
		rel := mustGet("/api/search?q=observability")
		newest := mustGet("/api/search?q=observability&sort=newest")
		if rel["total"] != newest["total"] {
			t.Errorf("override changed total: %v vs %v", rel["total"], newest["total"])
		}
		var dates []string
		for _, it := range newest["items"].([]any) {
			dates = append(dates, it.(map[string]any)["date"].(string))
		}
		sortedCopy := append([]string{}, dates...)
		slices.Reverse(sortedCopy)
		if !slices.IsSorted(sortedCopy) { // dates themselves descending
			t.Errorf("sort=newest dates not descending: %v", dates)
		}
	})

	t.Run("narrows search results with tag facets", func(t *testing.T) {
		all := mustGet("/api/search?q=observability")
		scoped := mustGet("/api/search?q=observability&tag=logging")
		if !(int(scoped["total"].(float64)) > 0 && int(scoped["total"].(float64)) <= int(all["total"].(float64))) {
			t.Error("facet did not narrow")
		}
	})

	t.Run("search default page is capped (mock parity: 10), list is not", func(t *testing.T) {
		m := mustGet("/api/search?q=a")
		if len(m["items"].([]any)) > 10 {
			t.Errorf("search default limit: %d items, want <=10", len(m["items"].([]any)))
		}
		if int(m["total"].(float64)) < 12 {
			t.Errorf("total must reflect ALL hits, got %v", m["total"])
		}
		if len(mustGet("/api/posts")["items"].([]any)) != 12 {
			t.Error("list must serve the whole corpus when limit absent")
		}
	})

	t.Run("empty q is total 0, not everything", func(t *testing.T) {
		m := mustGet("/api/search?q=")
		if int(m["total"].(float64)) != 0 {
			t.Error("empty-q search must return nothing, not the corpus")
		}
	})

	// ---- getPost ----
	t.Run("returns the queue article with sections and code fences", func(t *testing.T) {
		m := mustGet("/api/posts/shape-of-a-useful-queue")
		if m["title"] != "The shape of a useful queue" {
			t.Errorf("title = %v", m["title"])
		}
		if int(m["readTime"].(float64)) != 8 {
			t.Error("readTime mismatch")
		}
		if len(m["tags"].([]any)) == 0 {
			t.Error("detail must carry tags")
		}
		content, _ := m["content"].(string)
		for _, needle := range []string{"## The queue is a promise", "> A queue buys time",
			"```go", "```bash"} {
			if !strings.Contains(content, needle) {
				t.Errorf("content missing %q", needle)
			}
		}
		if !linkRE.MatchString(content) {
			t.Error("content should carry a markdown link")
		}
	})

	t.Run("unknown slug is 404", func(t *testing.T) {
		code, _ := get("/api/posts/no-such-post")
		if code != http.StatusNotFound {
			t.Errorf("status %d want 404", code)
		}
	})

	t.Run("DRAFT NEVER LEAKS — the §1 hard rule, tested at the boundary", func(t *testing.T) {
		// seeded into the DB (leak-guard bait); every surface must 404/absent it
		code, _ := get("/api/posts/drafts-are-fantasies")
		if code != http.StatusNotFound {
			t.Errorf("draft detail leaked: %d", code)
		}
		m := mustGet("/api/posts?limit=100")
		if slices.Contains(slugs(m), "drafts-are-fantasies") {
			t.Error("draft leaked in list")
		}
		m = mustGet("/api/search?q=fantasies")
		if slices.Contains(slugs(m), "drafts-are-fantasies") {
			t.Error("draft leaked in search")
		}
		s := mustGet("/api/search/suggest?q=draft")
		for _, it := range s["results"].([]any) {
			if it.(map[string]any)["slug"] == "drafts-are-fantasies" {
				t.Error("draft leaked in suggest")
			}
		}
	})

	// ---- suggest ----
	t.Run("suggest finds the queue post first", func(t *testing.T) {
		m := mustGet("/api/search/suggest?q=queue")
		res := m["results"].([]any)
		if len(res) == 0 || res[0].(map[string]any)["slug"] != "shape-of-a-useful-queue" {
			t.Errorf("suggest → %v", res)
		}
	})
	t.Run("suggest is case-insensitive over title and preview", func(t *testing.T) {
		m := mustGet("/api/search/suggest?q=BACKPRESSURE")
		var found bool
		for _, it := range m["results"].([]any) {
			if it.(map[string]any)["slug"] == "shape-of-a-useful-queue" {
				found = true
			}
		}
		if !found {
			t.Error("case-insensitive suggest failed")
		}
	})
	t.Run("suggest empty and limit", func(t *testing.T) {
		if len(mustGet("/api/search/suggest?q=zzz")["results"].([]any)) != 0 {
			t.Error("zzz should be empty")
		}
		if len(mustGet("/api/search/suggest?q=a&limit=2")["results"].([]any)) > 2 {
			t.Error("limit not honored")
		}
	})

	// ---- photos ----
	t.Run("photos non-empty with consistent total", func(t *testing.T) {
		m := mustGet("/api/photos")
		items := m["items"].([]any)
		if len(items) == 0 || int(m["total"].(float64)) != len(items) {
			t.Error("photos total mismatch")
		}
		for _, it := range items {
			p := it.(map[string]any)
			if int(p["width"].(float64)) <= 0 || int(p["height"].(float64)) <= 0 {
				t.Fatal("photo dims")
			}
			for _, banned := range []string{"gallery", "featured", "collection", "sort"} {
				if _, ok := p[banned]; ok {
					t.Errorf("public photo carries curation field %q", banned)
				}
			}
		}
		ratios := map[float64]bool{}
		for _, it := range items {
			p := it.(map[string]any)
			ratios[round2(p["width"].(float64)/p["height"].(float64))] = true
		}
		if len(ratios) <= 3 {
			t.Errorf("expected varied aspect ratios, got %d", len(ratios))
		}
	})
	t.Run("photos limit keeps full total", func(t *testing.T) {
		m := mustGet("/api/photos?limit=6")
		if len(m["items"].([]any)) != 6 || int(m["total"].(float64)) <= 6 {
			t.Error("photos limit/total")
		}
	})
	t.Run("featured photo answers with src and alt", func(t *testing.T) {
		m := mustGet("/api/photos/featured")
		src, _ := m["src"].(string)
		if !strings.HasPrefix(src, "https://") || m["alt"] == "" {
			t.Errorf("featured = %v", m)
		}
	})

	// ---- tags ----
	t.Run("tag cloud count-sorted with sane entries", func(t *testing.T) {
		code, body := get("/api/tags") // bare array (§1: Tag[])
		if code != 200 {
			t.Fatalf("tags status %d", code)
		}
		var tags []map[string]any
		if err := json.Unmarshal(body, &tags); err != nil {
			t.Fatal(err)
		}
		if len(tags) == 0 {
			t.Fatal("empty cloud")
		}
		prev := 1 << 30
		for _, tg := range tags {
			n := int(tg["posts"].(float64))
			name, _ := tg["name"].(string)
			if name == "" || n < 1 {
				t.Errorf("bad tag %v", tg)
			}
			if n > prev {
				t.Errorf("cloud not count-desc: %d after %d", n, prev)
			}
			prev = n
		}
	})
}

// ---- the integration twin: same suite, real Postgres ----

func TestContract_Offline_MemoryTwin(t *testing.T) {
	runContractSuite(t, memRouter(t))
}

func TestContract_Integration_Postgres(t *testing.T) {
	if os.Getenv("LUMEN_INTEGRATION") == "" {
		t.Skip("set LUMEN_INTEGRATION=1 with a seeded dev postgres (make test-integration)")
	}
	url := os.Getenv("LUMEN_DB_URL")
	if url == "" {
		url = "postgres://lumen:lumen-dev@localhost:5433/lumen?sslmode=disable"
	}
	pool, err := store.NewPool(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := httpapi.New(service.New(store.NewPG(pool))).Router()
	runContractSuite(t, h)
}
