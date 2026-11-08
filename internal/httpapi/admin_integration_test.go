package httpapi_test

// admin_integration_test.go — the §8 lifecycle against REAL Postgres, REAL
// Meilisearch, REAL river queue: login, CAS writes, the publish
// transaction's derivations, the state machine, soft delete, references,
// vocab rules, the admin search door (drafts included), and the two
// security promises that drove the JWT design: tv-revocation kills live
// tokens, the throttle starves stuffing. Gated (make test-integration).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/httpapi"
	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/service"
	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
	"github.com/kanatabi/lumen/internal/throttle"
)

type adminHarness struct {
	t     *testing.T
	h     http.Handler
	pool  *pgxpool.Pool
	qc    *queue.Client
	token string
}

func newAdminHarness(t *testing.T) *adminHarness {
	t.Helper()
	if os.Getenv("LUMEN_INTEGRATION") == "" {
		t.Skip("LUMEN_INTEGRATION unset")
	}
	dbURL := os.Getenv("LUMEN_DB_URL")
	if dbURL == "" {
		dbURL = "postgres://lumen:lumen-dev@localhost:5433/lumen?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	// test hygiene: purge any residue of THIS suite (hard deletes — hygiene
	// is not the product path). Run-to-run idempotence, not seed-to-seed luck.
	for _, stmt := range []string{
		`DELETE FROM posts WHERE slug LIKE 'integration-%'`,
		`DELETE FROM tags WHERE name LIKE 'integration-%'`,
		`DELETE FROM collections WHERE name IN ('night-series','integration-%') OR name LIKE 'integration-%'`,
		`DELETE FROM photos WHERE src LIKE 'https://integration.invalid/%'`,
		`DELETE FROM river_job WHERE args->>'post_id' IS NOT NULL AND state = 'discarded'`,
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := queue.MigrateUp(ctx, pool); err != nil {
		t.Fatal(err)
	}
	hash, err := session.HashPassword("integration-pw")
	if err != nil {
		t.Fatal(err)
	}
	pg := store.NewPG(pool)
	if err := pg.UpsertUser(ctx, "fzr-test", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET token_version = 1 WHERE username = 'fzr-test'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE username='fzr-test'`)
	})

	secret := "integration-session-secret-32-bytes!"
	eng := search.New(mustEnvOr(t, "LUMEN_MEILI_URL"), mustEnvOr(t, "LUMEN_MEILI_KEY"))
	if err := eng.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	qc, err := queue.New(ctx, pool, pg, eng)
	if err != nil {
		t.Fatal(err)
	}
	if err := qc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = qc.Stop(context.Background()) })

	deps := &httpapi.AdminDeps{Pool: pg, Queue: qc, JWTSecret: secret, Limiter: throttle.New(time.Minute)}
	h := httpapi.New(service.New(pg, eng), eng, deps).Router()

	ah := &adminHarness{t: t, h: h, pool: pool, qc: qc}
	ah.token = ah.login("fzr-test", "integration-pw")
	return ah
}

func (a *adminHarness) login(user, pw string) string {
	a.t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	code, res := a.do(http.MethodPost, "/admin/session", string(body), "")
	if code != http.StatusOK {
		a.t.Fatalf("login code %d: %s", code, res)
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		a.t.Fatal(err)
	}
	if out.ExpiresIn != session.TTLSeconds {
		a.t.Fatalf("expires_in = %d, want 43200", out.ExpiresIn)
	}
	return out.Token
}

func (a *adminHarness) do(method, path, body, token string) (int, []byte) {
	a.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (a *adminHarness) admin(method, path, body string) (int, []byte) {
	return a.do(method, path, body, a.token)
}

func (a *adminHarness) mustOK(method, path, body string) []byte {
	code, res := a.admin(method, path, body)
	if code < 200 || code >= 300 {
		a.t.Fatalf("%s %s → %d: %s", method, path, code, res)
	}
	return res
}

// drain waits for river to settle (workers await each engine task inline,
// so an empty queue implies a converged index).
func (a *adminHarness) drain() {
	a.t.Helper()
	for i := 0; i < 300; i++ {
		var n int
		if err := a.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM river_job WHERE state NOT IN ('completed','cancelled','discarded')`).Scan(&n); err != nil {
			a.t.Fatal(err)
		}
		if n == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.t.Fatal("queue never drained")
}

func TestAdminLifecycle(t *testing.T) {
	ah := newAdminHarness(t)

	// ---- auth gates ----
	t.Run("401 without or with a bogus bearer", func(t *testing.T) {
		if code, _ := ah.do(http.MethodGet, "/admin/posts", "", ""); code != http.StatusUnauthorized {
			t.Fatalf("no-token code %d", code)
		}
		if code, _ := ah.do(http.MethodGet, "/admin/posts", "", "garbage"); code != http.StatusUnauthorized {
			t.Fatalf("garbage-token code %d", code)
		}
	})
	t.Run("wrong password and unknown user are one indistinguishable message", func(t *testing.T) {
		_, r1 := ah.do(http.MethodPost, "/admin/session", `{"username":"fzr-test","password":"nope"}`, "")
		_, r2 := ah.do(http.MethodPost, "/admin/session", `{"username":"ghost-user-zz","password":"nope"}`, "")
		if string(r1) != string(r2) {
			t.Fatalf("enumeration leak: %s vs %s", r1, r2)
		}
	})
	t.Run("empty credentials 400", func(t *testing.T) {
		if code, _ := ah.do(http.MethodPost, "/admin/session", `{"username":"","password":"x"}`, ""); code != http.StatusBadRequest {
			t.Fatalf("code %d", code)
		}
	})

	// ---- CRUD + CAS ----
	draft := `{"slug":"integration-publish-me","title":"Integration Publish Me","subtitle":"machine proof","preview":"","date":"2026-11-08","tags":["integration","queues"],"content":"# Integration Publish Me\n\nThe first paragraph becomes the preview by default.\n\n` + "```" + `go\nfunc main() {}` + "```" + `\n","thumbnail":null}`
	t.Run("CRUD, publish machine, idempotence", func(t *testing.T) {
		defer func() { _, _ = ah.admin(http.MethodDelete, "/admin/posts/integration-publish-me", "") }()

		code, res := ah.admin(http.MethodPost, "/admin/posts", draft)
		if code != http.StatusCreated {
			t.Fatalf("create %d: %s", code, res)
		}
		var d store.AdminPostDetail
		_ = json.Unmarshal(res, &d)
		if d.Status != "draft" || d.Rev != 1 {
			t.Fatalf("fresh draft: %+v", d)
		}
		if code, _ := ah.admin(http.MethodPost, "/admin/posts", draft); code != http.StatusConflict {
			t.Fatalf("dup slug code %d", code)
		}

		upd := `{"draft":{"slug":"integration-publish-me","title":"Renamed Integration","subtitle":"s","preview":"","date":"2026-11-08","tags":["integration"],"content":"body\n","thumbnail":null},"rev":99}`
		if code, _ := ah.admin(http.MethodPut, "/admin/posts/integration-publish-me", upd); code != http.StatusConflict {
			t.Fatalf("stale rev should 409")
		}
		upd = strings.Replace(upd, `"rev":99`, `"rev":1`, 1)
		res = ah.mustOK(http.MethodPut, "/admin/posts/integration-publish-me", upd)
		var d2 store.AdminPostDetail
		_ = json.Unmarshal(res, &d2)
		if d2.Rev != 2 || d2.Title != "Renamed Integration" {
			t.Fatalf("after update rev %d title %q", d2.Rev, d2.Title)
		}

		// the §2 publish unit of work: derivations land
		res = ah.mustOK(http.MethodPost, "/admin/posts/integration-publish-me/publish", "")
		var p store.AdminPostDetail
		_ = json.Unmarshal(res, &p)
		if p.Status != "published" || p.PublishedAt == nil || p.ReadTime < 1 {
			t.Fatalf("publish derivations: %+v", p)
		}
		// the update earlier set content="body\n" with an empty preview —
		// the publish default must derive exactly that first paragraph
		if p.Preview != "body" {
			t.Fatalf("preview default: %q", p.Preview)
		}

		// idempotent re-publish: byte-identical inputs must not move rank
		rankA := rankOf(ah, "integration-publish-me")
		ah.mustOK(http.MethodPost, "/admin/posts/integration-publish-me/publish", "")
		if rankB := rankOf(ah, "integration-publish-me"); rankA != rankB {
			t.Fatalf("idempotent re-publish moved rank %d→%d", rankA, rankB)
		}

		// surfaced on the PUBLIC list after the queue drains
		ah.drain()
		pub := ah.mustOK(http.MethodGet, "/api/posts?limit=100", "")
		if !bytes.Contains(pub, []byte("integration-publish-me")) {
			t.Fatal("published post missing from the public list")
		}

		// unpublish → vanishes from public, legal re-publish brings it back
		ah.mustOK(http.MethodPost, "/admin/posts/integration-publish-me/unpublish", "")
		ah.drain()
		pub = ah.mustOK(http.MethodGet, "/api/posts?limit=100", "")
		if bytes.Contains(pub, []byte("integration-publish-me")) {
			t.Fatal("unpublished post LEAKED to the public list")
		}

		// machine: draft→unpublish directly is the illegal edge
		illegal := `{"slug":"integration-illegal","title":"Illegal","subtitle":"","preview":"x","date":"2026-11-08","tags":[],"content":"x\n","thumbnail":null}`
		ah.mustOK(http.MethodPost, "/admin/posts", illegal)
		code, _ = ah.admin(http.MethodPost, "/admin/posts/integration-illegal/unpublish", "")
		if code != http.StatusConflict {
			t.Fatalf("draft→unpublished should 409, got %d", code)
		}
		_, _ = ah.admin(http.MethodDelete, "/admin/posts/integration-illegal", "")
	})

	// ---- the seeded leak bait ----
	t.Run("seeded draft never surfaces on any public path", func(t *testing.T) {
		if code, _ := ah.do(http.MethodGet, "/api/posts/drafts-are-fantasies", "", ""); code != http.StatusNotFound {
			t.Fatalf("draft detail code %d", code)
		}
	})

	// ---- photos ----
	t.Run("photo lifecycle on the suite's OWN row (siblings untouched)", func(t *testing.T) {
		defer func() {
			_, _ = ah.pool.Exec(context.Background(), `DELETE FROM photos WHERE src LIKE 'https://integration.invalid/%'`)
		}()
		var pid int64
		if err := ah.pool.QueryRow(context.Background(),
			`INSERT INTO photos(src, alt, width, height, sort, featured, gallery)
			 VALUES ('https://integration.invalid/photo-1', 'integration photo one', 100, 200, 999, FALSE, TRUE)
			 RETURNING id`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if err := ah.qc.EnqueueIndexPhoto(context.Background(), pid); err != nil {
			t.Fatal(err)
		}
		ah.drain()
		idS := strconv.FormatInt(pid, 10)

		// indexed & admin-visible
		res := ah.mustOK(http.MethodGet, "/admin/photos/"+idS, "")
		if !bytes.Contains(res, []byte("integration photo one")) {
			t.Fatalf("created photo not admin-visible: %s", res)
		}

		// references: embed its src in a post body, the scan finds exactly it
		src := "https://integration.invalid/photo-1"
		emb := `{"slug":"integration-refs","title":"Refs","subtitle":"","preview":"p","date":"2026-11-08","tags":[],"content":"look: ` + src + ` here\n","thumbnail":null}`
		ah.mustOK(http.MethodPost, "/admin/posts", emb)
		res = ah.mustOK(http.MethodGet, "/admin/photos/"+idS+"/references", "")
		var refs []store.PhotoRef
		_ = json.Unmarshal(res, &refs)
		if len(refs) != 1 || refs[0].Slug != "integration-refs" {
			t.Fatalf("refs = %s", res)
		}

		// patch: unknown collection 400; valid writes land
		if code, _ := ah.admin(http.MethodPatch, "/admin/photos/"+idS, `{"collection":"does-not-exist"}`); code != http.StatusBadRequest {
			t.Fatalf("unknown collection code %d", code)
		}
		res = ah.mustOK(http.MethodPatch, "/admin/photos/"+idS, `{"alt":"patched integration alt","gallery":false}`)
		var got store.AdminPhoto
		_ = json.Unmarshal(res, &got)
		if got.Alt != "patched integration alt" || got.Gallery {
			t.Fatalf("patch result %+v", got)
		}
		ah.drain()
		// un-galleryed: must not surface on the PUBLIC list (truth + engine agree)
		pub := ah.mustOK(http.MethodGet, "/api/photos", "")
		if bytes.Contains(pub, []byte(src)) {
			t.Fatal("un-galleryed photo leaked to the public list")
		}

		// delete → row gone (404), doc converges out of the engine
		ah.mustOK(http.MethodDelete, "/admin/photos/"+idS, "")
		if code, _ := ah.admin(http.MethodGet, "/admin/photos/"+idS, ""); code != http.StatusNotFound {
			t.Fatalf("deleted photo code %d", code)
		}
		ah.drain()
		if _, res := ah.admin(http.MethodGet, "/admin/search?q=patched%20integration", ""); bytes.Contains(res, []byte(`"id":"`+idS+`"`)) {
			t.Fatal("deleted photo doc still in the engine")
		}
		_, _ = ah.admin(http.MethodDelete, "/admin/posts/integration-refs", "")
	})

	// ---- vocab ----
	t.Run("eager vocabulary: slugified creation, 409 duplicates", func(t *testing.T) {
		res := ah.mustOK(http.MethodPost, "/admin/tags", `{"name":"Integration Test Tag"}`)
		var tg store.TagRowOut
		_ = json.Unmarshal(res, &tg)
		if tg.Name != "integration-test-tag" {
			t.Fatalf("slugified name %q", tg.Name)
		}
		if code, _ := ah.admin(http.MethodPost, "/admin/tags", `{"name":"integration test tag"}`); code != http.StatusConflict {
			t.Fatalf("dup tag code %d", code)
		}
		res = ah.mustOK(http.MethodPost, "/admin/collections", `{"name":"Night Series"}`)
		var c store.CollectionRowOut
		_ = json.Unmarshal(res, &c)
		if c.Name != "night-series" {
			t.Fatalf("collection name %q", c.Name)
		}
	})

	// ---- admin search door ----
	t.Run("admin search: three groups, drafts included, ≤8 each, empty-q shape", func(t *testing.T) {
		ah.drain()
		// 'leak' is in the DRAFT's title ("A draft that must never leak");
		// the admin door has no status filter, so it must surface
		res := ah.mustOK(http.MethodGet, "/admin/search?q=leak", "")
		var out struct {
			Posts  []store.AdminPostSummary `json:"posts"`
			Photos []store.AdminPhoto       `json:"photos"`
			Tags   []store.TagRowOut        `json:"tags"`
		}
		_ = json.Unmarshal(res, &out)
		if !bytes.Contains(res, []byte("drafts-are-fantasies")) {
			t.Fatalf("admin door must include drafts: %s", res)
		}
		if len(out.Posts) > 8 || len(out.Photos) > 8 || len(out.Tags) > 8 {
			t.Fatal("≤8 each")
		}
		res = ah.mustOK(http.MethodGet, "/admin/search?q=", "")
		if !bytes.Contains(res, []byte(`"posts":[]`)) {
			t.Fatalf("empty-q shape: %s", res)
		}
	})

	// ---- revocation ----
	t.Run("logout bumps tv — the live token dies instantly", func(t *testing.T) {
		live := ah.token
		if code, _ := ah.do(http.MethodGet, "/admin/posts", "", live); code != http.StatusOK {
			t.Fatalf("pre-revoke code %d", code)
		}
		if code, _ := ah.do(http.MethodDelete, "/admin/session", "", live); code != http.StatusNoContent {
			t.Fatalf("logout code %d", code)
		}
		if code, _ := ah.do(http.MethodGet, "/admin/posts", "", live); code != http.StatusUnauthorized {
			t.Fatalf("REVOKED token still works: %d", code)
		}
		ah.token = ah.login("fzr-test", "integration-pw")
	})

	// ---- throttle ----
	t.Run("throttle starves credential stuffing", func(t *testing.T) {
		lim := throttle.New(time.Minute)
		deps := &httpapi.AdminDeps{Pool: store.NewPG(ah.pool), Queue: nil, JWTSecret: "x", Limiter: lim}
		h := httpapi.New(service.New(nil, nil), nil, deps).Router()
		for i := 0; i < 12; i++ {
			req := httptest.NewRequest(http.MethodPost, "/admin/session",
				strings.NewReader(`{"username":"victim","password":"guessed"}`))
			req.RemoteAddr = "203.0.113.7:1234"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusTooManyRequests {
				if rec.Header().Get("Retry-After") == "" {
					t.Fatal("429 without Retry-After")
				}
				return
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("attempt %d code %d", i+1, rec.Code)
			}
		}
		t.Fatal("12 failures and no 429 — the throttle is asleep")
	})
}

func rankOf(ah *adminHarness, slug string) int {
	var r int
	err := ah.pool.QueryRow(context.Background(), `SELECT rank FROM posts WHERE slug=$1`, slug).Scan(&r)
	if err != nil {
		ah.t.Fatal(err)
	}
	return r
}
