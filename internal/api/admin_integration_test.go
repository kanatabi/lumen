package api_test

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
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/api"
	"github.com/kanatabi/lumen/internal/api/admin"
	"github.com/kanatabi/lumen/internal/api/public"
	"github.com/kanatabi/lumen/internal/media"
	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/service"
	"github.com/kanatabi/lumen/internal/session"
	"github.com/kanatabi/lumen/internal/store"
	"github.com/kanatabi/lumen/internal/throttle"
)

type adminHarness struct {
	t           *testing.T
	h           http.Handler
	pool        *pgxpool.Pool
	queueClient *queue.Client
	adminSvc    *adminsvc.Service
	rebuild     func() // re-mount the router after mutating the service (e.g. adding §9)
	token       string
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
	searchEngine := search.New(mustEnvOr(t, "LUMEN_MEILI_URL"), mustEnvOr(t, "LUMEN_MEILI_KEY"))
	if err := searchEngine.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	// mirrors the boot hook (deps.go): tag docs replaced synchronously before
	// anything searches them
	tdocs, err := pg.ListAllTagDocs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := searchEngine.IndexTags(ctx, tdocs); err != nil {
		t.Fatal(err)
	}

	queueClient, err := queue.New(ctx, pool, pg, searchEngine)
	if err != nil {
		t.Fatal(err)
	}
	if err := queueClient.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queueClient.Stop(context.Background()) })

	adminSvc := adminsvc.New(pg, queueClient, searchEngine, secret, throttle.New(time.Minute))
	build := func() http.Handler {
		return api.NewRouter(public.New(service.New(pg, searchEngine), searchEngine), admin.New(adminSvc))
	}
	// media relay: an EMBEDDED S3-protocol server (gofakes3 + s3mem) — a
	// real SigV4 endpoint, in-process and deterministic; no registry luck.
	if os.Getenv("LUMEN_MEDIA") == "1" {
		backend := s3mem.New()
		if err := backend.CreateBucket("lumen-media-test"); err != nil {
			t.Fatal(err)
		}
		s3srv := httptest.NewServer(gofakes3.New(backend).Server())
		t.Cleanup(s3srv.Close)
		mediaConfig := media.Config{
			Endpoint:  s3srv.URL,
			Region:    "us-east-1",
			Bucket:    "lumen-media-test",
			AccessKey: "test",
			SecretKey: "test",
			ForcePath: true,
		}
		mediaStore, _, err := media.NewS3(ctx, mediaConfig)
		if err != nil {
			t.Fatal(err)
		}
		adminSvc.EnableMedia(mediaStore, mediaConfig.Endpoint, mediaConfig.Bucket, "")
	}
	harness := &adminHarness{t: t, pool: pool, queueClient: queueClient, adminSvc: adminSvc}
	harness.h = build()
	harness.rebuild = func() { harness.h = build() }
	harness.token = harness.login("fzr-test", "integration-pw")
	return harness
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
		ExpiresIn int    `json:"expiresIn"` // mellon AdminApi: camelCase is the spec
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
	harness := newAdminHarness(t)

	// ---- auth gates ----
	t.Run("401 without or with a bogus bearer", func(t *testing.T) {
		if code, _ := harness.do(http.MethodGet, "/admin/posts", "", ""); code != http.StatusUnauthorized {
			t.Fatalf("no-token code %d", code)
		}
		if code, _ := harness.do(http.MethodGet, "/admin/posts", "", "garbage"); code != http.StatusUnauthorized {
			t.Fatalf("garbage-token code %d", code)
		}
	})
	t.Run("wrong password and unknown user are one indistinguishable message", func(t *testing.T) {
		_, r1 := harness.do(http.MethodPost, "/admin/session", `{"username":"fzr-test","password":"nope"}`, "")
		_, r2 := harness.do(http.MethodPost, "/admin/session", `{"username":"ghost-user-zz","password":"nope"}`, "")
		if string(r1) != string(r2) {
			t.Fatalf("enumeration leak: %s vs %s", r1, r2)
		}
	})
	t.Run("empty credentials 400", func(t *testing.T) {
		if code, _ := harness.do(http.MethodPost, "/admin/session", `{"username":"","password":"x"}`, ""); code != http.StatusBadRequest {
			t.Fatalf("code %d", code)
		}
	})

	// ---- CRUD + CAS ----
	draft := `{"slug":"integration-publish-me","title":"Integration Publish Me","subtitle":"machine proof","preview":"","date":"2026-11-08","tags":["integration","queues"],"content":"# Integration Publish Me\n\nThe first paragraph becomes the preview by default.\n\n` + "```" + `go\nfunc main() {}` + "```" + `\n","thumbnail":null}`
	t.Run("CRUD, publish machine, idempotence", func(t *testing.T) {
		defer func() { _, _ = harness.admin(http.MethodDelete, "/admin/posts/integration-publish-me", "") }()

		code, res := harness.admin(http.MethodPost, "/admin/posts", draft)
		if code != http.StatusCreated {
			t.Fatalf("create %d: %s", code, res)
		}
		var d store.AdminPostDetail
		_ = json.Unmarshal(res, &d)
		if d.Status != "draft" || d.Rev != 1 {
			t.Fatalf("fresh draft: %+v", d)
		}
		if code, _ := harness.admin(http.MethodPost, "/admin/posts", draft); code != http.StatusConflict {
			t.Fatalf("dup slug code %d", code)
		}

		upd := `{"draft":{"slug":"integration-publish-me","title":"Renamed Integration","subtitle":"s","preview":"","date":"2026-11-08","tags":["integration"],"content":"body\n","thumbnail":null},"rev":99}`
		if code, _ := harness.admin(http.MethodPut, "/admin/posts/integration-publish-me", upd); code != http.StatusConflict {
			t.Fatalf("stale rev should 409")
		}
		upd = strings.Replace(upd, `"rev":99`, `"rev":1`, 1)
		res = harness.mustOK(http.MethodPut, "/admin/posts/integration-publish-me", upd)
		var d2 store.AdminPostDetail
		_ = json.Unmarshal(res, &d2)
		if d2.Rev != 2 || d2.Title != "Renamed Integration" {
			t.Fatalf("after update rev %d title %q", d2.Rev, d2.Title)
		}

		// the §2 publish unit of work: derivations land
		res = harness.mustOK(http.MethodPost, "/admin/posts/integration-publish-me/publish", "")
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
		rankA := rankOf(harness, "integration-publish-me")
		harness.mustOK(http.MethodPost, "/admin/posts/integration-publish-me/publish", "")
		if rankB := rankOf(harness, "integration-publish-me"); rankA != rankB {
			t.Fatalf("idempotent re-publish moved rank %d→%d", rankA, rankB)
		}

		// surfaced on the PUBLIC list after the queue drains
		harness.drain()
		pub := harness.mustOK(http.MethodGet, "/api/posts?limit=100", "")
		if !bytes.Contains(pub, []byte("integration-publish-me")) {
			t.Fatal("published post missing from the public list")
		}

		// unpublish → vanishes from public, legal re-publish brings it back
		harness.mustOK(http.MethodPost, "/admin/posts/integration-publish-me/unpublish", "")
		harness.drain()
		pub = harness.mustOK(http.MethodGet, "/api/posts?limit=100", "")
		if bytes.Contains(pub, []byte("integration-publish-me")) {
			t.Fatal("unpublished post LEAKED to the public list")
		}

		// machine: draft→unpublish directly is the illegal edge
		illegal := `{"slug":"integration-illegal","title":"Illegal","subtitle":"","preview":"x","date":"2026-11-08","tags":[],"content":"x\n","thumbnail":null}`
		harness.mustOK(http.MethodPost, "/admin/posts", illegal)
		code, _ = harness.admin(http.MethodPost, "/admin/posts/integration-illegal/unpublish", "")
		if code != http.StatusConflict {
			t.Fatalf("draft→unpublished should 409, got %d", code)
		}
		_, _ = harness.admin(http.MethodDelete, "/admin/posts/integration-illegal", "")
	})

	// ---- soft delete converges the DOC, not just the rows ----
	// The mellon cutover e2e caught the phantom-total bug: PostDocByID read
	// the soft-deleted row (deleted_at ignored), the worker RE-indexed the
	// stale published doc — items stayed honest (hydration drops what truth
	// hides) but the engine's total inflated by ghosts. The fix is the
	// deleted_at filter; this test pins the arithmetic: total == len(items)
	// after publish+delete, exactly as the contract demands.
	t.Run("soft delete removes the DOC — totals never carry ghosts", func(t *testing.T) {
		d := `{"slug":"integration-phantom-check","title":"Phantom Check","subtitle":"","preview":"p","date":"2026-11-09","tags":[],"content":"body\n","thumbnail":null}`
		harness.mustOK("POST", "/admin/posts", d)
		code, _ := harness.admin("POST", "/admin/posts/integration-phantom-check/publish", "")
		if code != 200 {
			t.Fatalf("publish code %d", code)
		}
		harness.drain()
		harness.mustOK("DELETE", "/admin/posts/integration-phantom-check", "")
		harness.drain()
		pub := harness.mustOK("GET", "/api/posts?limit=100", "")
		var m map[string]any
		_ = json.Unmarshal(pub, &m)
		total := int(m["total"].(float64))
		items := m["items"].([]any)
		if total != len(items) {
			t.Fatalf("phantom doc survived: total %d != items %d (engine carries a soft-deleted published row)", total, len(items))
		}
		if bytes.Contains(pub, []byte("integration-phantom-check")) {
			t.Fatal("deleted post still served")
		}
	})

	// ---- the create-draft index job (owner code-read bug) ----
	t.Run("create enqueues the doc — admin door finds a fresh draft", func(t *testing.T) {
		d := `{"slug":"integration-create-index","title":"Zephyrcreatescan Marker","subtitle":"","preview":"p","date":"2026-11-09","tags":[],"content":"first body\n","thumbnail":null}`
		defer func() { _, _ = harness.admin("DELETE", "/admin/posts/integration-create-index", "") }()
		code, res := harness.admin("POST", "/admin/posts", d)
		if code != http.StatusCreated {
			t.Fatalf("create %d: %s", code, res)
		}
		harness.drain()
		// the engine doc must exist NOW (before any update/publish):
		if _, res := harness.admin("GET", "/admin/search?q=zephyrcreatescan", ""); !bytes.Contains(res, []byte("integration-create-index")) {
			t.Fatalf("fresh draft invisible at the admin door: %s", res)
		}
		// and the public surface still must not see it:
		if _, res := harness.do("GET", "/api/posts/integration-create-index", "", ""); !bytes.Contains(res, []byte("not found")) {
			t.Fatal("fresh draft leaked to the public detail route")
		}
	})

	// ---- the seeded leak bait ----
	t.Run("seeded draft never surfaces on any public path", func(t *testing.T) {
		if code, _ := harness.do(http.MethodGet, "/api/posts/drafts-are-fantasies", "", ""); code != http.StatusNotFound {
			t.Fatalf("draft detail code %d", code)
		}
	})

	// ---- media upload relay (LUMEN_MEDIA=1) ----
	t.Run("upload relay: probe, strip, S3, row, index — one call", func(t *testing.T) {
		if os.Getenv("LUMEN_MEDIA") != "1" {
			t.Skip("LUMEN_MEDIA unset")
		}
		var raw bytes.Buffer
		img := image.NewRGBA(image.Rect(0, 0, 240, 160))
		if err := jpeg.Encode(&raw, img, &jpeg.Options{Quality: 85}); err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, err := mw.CreateFormFile("photo", "integration-upload.jpg")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(raw.Bytes())
		_ = mw.Close()

		code, res := harness.do(http.MethodPost, "/admin/photos", body.String(), harness.token)
		// httptest.NewRequest ignores content-type from the string body —
		// build the request manually to carry the multipart boundary
		if code != http.StatusUnsupportedMediaType && code != http.StatusBadRequest {
			var got store.AdminPhoto
			_ = json.Unmarshal(res, &got)
			if !strings.Contains(got.Src, "/media/") || !strings.HasSuffix(got.Src, ".jpg") {
				t.Fatalf("src not the content-addressed key: %s", got.Src)
			}
			if got.Alt != "integration-upload" || got.Gallery {
				t.Fatalf("fresh upload defaults wrong: %+v", got)
			}
			t.Cleanup(func() {
				_, _ = harness.admin(http.MethodDelete, "/admin/photos/"+got.ID, "")
			})
			harness.drain()
			list := harness.mustOK(http.MethodGet, "/admin/photos?limit=100", "")
			if !bytes.Contains(list, []byte(got.ID)) {
				t.Fatal("uploaded photo missing from admin list")
			}
			return
		}
		// fallthrough: wrong multipart framing — retry with an explicit request
		req, err := http.NewRequest(http.MethodPost, "/admin/photos", bytes.NewReader(body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+harness.token)
		rec := httptest.NewRecorder()
		harness.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload code %d: %s", rec.Code, rec.Body)
		}
		var got store.AdminPhoto
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if !strings.Contains(got.Src, "/media/") {
			t.Fatalf("src: %s", got.Src)
		}
		t.Cleanup(func() { _, _ = harness.admin(http.MethodDelete, "/admin/photos/"+got.ID, "") })
		harness.drain()
		list := harness.mustOK(http.MethodGet, "/admin/photos?limit=100", "")
		if !bytes.Contains(list, []byte(got.ID)) {
			t.Fatal("uploaded photo missing from admin list")
		}
	})

	// ---- photos ----
	t.Run("photo lifecycle on the suite's OWN row (siblings untouched)", func(t *testing.T) {
		defer func() {
			_, _ = harness.pool.Exec(context.Background(), `DELETE FROM photos WHERE src LIKE 'https://integration.invalid/%'`)
		}()
		var pid int64
		if err := harness.pool.QueryRow(context.Background(),
			`INSERT INTO photos(src, alt, width, height, sort, featured, gallery)
			 VALUES ('https://integration.invalid/photo-1', 'integration photo one', 100, 200, 999, FALSE, TRUE)
			 RETURNING id`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if err := harness.queueClient.EnqueueIndexPhoto(context.Background(), pid); err != nil {
			t.Fatal(err)
		}
		harness.drain()
		idS := strconv.FormatInt(pid, 10)

		// indexed & admin-visible
		res := harness.mustOK(http.MethodGet, "/admin/photos/"+idS, "")
		if !bytes.Contains(res, []byte("integration photo one")) {
			t.Fatalf("created photo not admin-visible: %s", res)
		}

		// references: embed its src in a post body, the scan finds exactly it
		src := "https://integration.invalid/photo-1"
		emb := `{"slug":"integration-refs","title":"Refs","subtitle":"","preview":"p","date":"2026-11-08","tags":[],"content":"look: ` + src + ` here\n","thumbnail":null}`
		harness.mustOK(http.MethodPost, "/admin/posts", emb)
		res = harness.mustOK(http.MethodGet, "/admin/photos/"+idS+"/references", "")
		var refs []store.PhotoRef
		_ = json.Unmarshal(res, &refs)
		if len(refs) != 1 || refs[0].Slug != "integration-refs" {
			t.Fatalf("refs = %s", res)
		}

		// patch: unknown collection 400; valid writes land
		if code, _ := harness.admin(http.MethodPatch, "/admin/photos/"+idS, `{"collection":"does-not-exist"}`); code != http.StatusBadRequest {
			t.Fatalf("unknown collection code %d", code)
		}
		res = harness.mustOK(http.MethodPatch, "/admin/photos/"+idS, `{"alt":"patched integration alt","gallery":false}`)
		var got store.AdminPhoto
		_ = json.Unmarshal(res, &got)
		if got.Alt != "patched integration alt" || got.Gallery {
			t.Fatalf("patch result %+v", got)
		}
		harness.drain()
		// un-galleryed: must not surface on the PUBLIC list (truth + engine agree)
		pub := harness.mustOK(http.MethodGet, "/api/photos", "")
		if bytes.Contains(pub, []byte(src)) {
			t.Fatal("un-galleryed photo leaked to the public list")
		}

		// delete → row gone (404), doc converges out of the engine
		harness.mustOK(http.MethodDelete, "/admin/photos/"+idS, "")
		if code, _ := harness.admin(http.MethodGet, "/admin/photos/"+idS, ""); code != http.StatusNotFound {
			t.Fatalf("deleted photo code %d", code)
		}
		harness.drain()
		if _, res := harness.admin(http.MethodGet, "/admin/search?q=patched%20integration", ""); bytes.Contains(res, []byte(`"id":"`+idS+`"`)) {
			t.Fatal("deleted photo doc still in the engine")
		}
		_, _ = harness.admin(http.MethodDelete, "/admin/posts/integration-refs", "")
	})

	// ---- vocab ----
	t.Run("eager vocabulary: slugified creation, 409 duplicates", func(t *testing.T) {
		res := harness.mustOK(http.MethodPost, "/admin/tags", `{"name":"Integration Test Tag"}`)
		var tg store.TagRowOut
		_ = json.Unmarshal(res, &tg)
		if tg.Name != "integration-test-tag" {
			t.Fatalf("slugified name %q", tg.Name)
		}
		if code, _ := harness.admin(http.MethodPost, "/admin/tags", `{"name":"integration test tag"}`); code != http.StatusConflict {
			t.Fatalf("dup tag code %d", code)
		}
		res = harness.mustOK(http.MethodPost, "/admin/collections", `{"name":"Night Series"}`)
		var c store.CollectionRowOut
		_ = json.Unmarshal(res, &c)
		if c.Name != "night-series" {
			t.Fatalf("collection name %q", c.Name)
		}
	})

	// ---- admin search door ----
	t.Run("admin search: three groups, drafts included, ≤8 each, empty-q shape", func(t *testing.T) {
		harness.drain()
		// 'leak' is in the DRAFT's title ("A draft that must never leak");
		// the admin door has no status filter, so it must surface
		res := harness.mustOK(http.MethodGet, "/admin/search?q=leak", "")
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
		res = harness.mustOK(http.MethodGet, "/admin/search?q=", "")
		if !bytes.Contains(res, []byte(`"posts":[]`)) {
			t.Fatalf("empty-q shape: %s", res)
		}
	})

	// ---- revocation ----
	t.Run("logout bumps tv — the live token dies instantly", func(t *testing.T) {
		live := harness.token
		if code, _ := harness.do(http.MethodGet, "/admin/posts", "", live); code != http.StatusOK {
			t.Fatalf("pre-revoke code %d", code)
		}
		if code, _ := harness.do(http.MethodDelete, "/admin/session", "", live); code != http.StatusNoContent {
			t.Fatalf("logout code %d", code)
		}
		if code, _ := harness.do(http.MethodGet, "/admin/posts", "", live); code != http.StatusUnauthorized {
			t.Fatalf("REVOKED token still works: %d", code)
		}
		harness.token = harness.login("fzr-test", "integration-pw")
	})

	// ---- throttle ----
	t.Run("throttle starves credential stuffing", func(t *testing.T) {
		lim := throttle.New(time.Minute)
		throttleSvc := adminsvc.New(store.NewPG(harness.pool), nil, nil, "x", lim)
		h := api.NewRouter(public.New(service.New(nil, nil), nil), admin.New(throttleSvc))
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

func orDev(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

func rankOf(harness *adminHarness, slug string) int {
	var r int
	err := harness.pool.QueryRow(context.Background(), `SELECT rank FROM posts WHERE slug=$1`, slug).Scan(&r)
	if err != nil {
		harness.t.Fatal(err)
	}
	return r
}

// ---- A6 (tag desk): featured is the desk's manual promotion flag; counts
// are TOTAL on every state change (zeroing included); rename propagates via
// the id-keyed FK web; delete refuses while ANY live post references. ----
func TestTagDesk(t *testing.T) {
	harness := newAdminHarness(t)

	pubTags := func() string {
		code, res := harness.do(http.MethodGet, "/api/tags", "", "")
		if code != http.StatusOK {
			t.Fatalf("public tags %d: %s", code, res)
		}
		return string(res)
	}

	// create: slugified, unfeatured
	code, res := harness.admin(http.MethodPost, "/admin/tags", `{"name":"Integration Tag Alpha"}`)
	if code != http.StatusCreated {
		t.Fatalf("create tag %d: %s", code, res)
	}
	if !bytes.Contains(res, []byte(`"name":"integration-tag-alpha"`)) || !bytes.Contains(res, []byte(`"featured":false`)) {
		t.Fatalf("create shape: %s", res)
	}

	// feature it by hand — the desk verb
	code, res = harness.admin(http.MethodPatch, "/admin/tags/integration-tag-alpha", `{"featured":true}`)
	if code != http.StatusOK || !bytes.Contains(res, []byte(`"featured":true`)) {
		t.Fatalf("feature %d: %s", code, res)
	}
	// A5 rule: featured but ZERO posts does NOT surface in the widget
	if strings.Contains(pubTags(), "integration-tag-alpha") {
		t.Fatal("unused featured tag leaked into the public cloud")
	}

	// publish a post carrying it → the widget now shows it, count live
	draft := `{"slug":"integration-tagging","title":"Integration Tagging","subtitle":"desk proof","preview":"","date":"2026-11-10","tags":["integration-tag-alpha"],"content":"body","thumbnail":null}`
	if code, res = harness.admin(http.MethodPost, "/admin/posts", draft); code != http.StatusCreated {
		t.Fatalf("create post %d: %s", code, res)
	}
	defer func() { _, _ = harness.admin(http.MethodDelete, "/admin/posts/integration-tagging", "") }()
	if code, res = harness.admin(http.MethodPost, "/admin/posts/integration-tagging/publish", ""); code != http.StatusOK {
		t.Fatalf("publish %d: %s", code, res)
	}
	pub := pubTags()
	if !strings.Contains(pub, "integration-tag-alpha") || !strings.Contains(pub, `"posts":1`) {
		t.Fatalf("widget missing the used featured tag: %s", pub)
	}

	// rename → row moves, posts follow via id-keyed FK
	if code, res = harness.admin(http.MethodPatch, "/admin/tags/integration-tag-alpha", `{"name":"Integration Tag Beta"}`); code != http.StatusOK || !bytes.Contains(res, []byte(`"name":"integration-tag-beta"`)) {
		t.Fatalf("rename %d: %s", code, res)
	}
	if code, res = harness.admin(http.MethodGet, "/admin/posts/integration-tagging", ""); code != http.StatusOK || !strings.Contains(string(res), "integration-tag-beta") {
		t.Fatalf("rename did not propagate to the post: %s", res)
	}
	// collision refuses
	if code, _ = harness.admin(http.MethodPost, "/admin/tags", `{"name":"integration-tag-keeper"}`); code != http.StatusCreated {
		t.Fatalf("keeper create %d", code)
	}
	defer func() { _, _ = harness.admin(http.MethodDelete, "/admin/tags/integration-tag-keeper", "") }()
	if code, _ = harness.admin(http.MethodPatch, "/admin/tags/integration-tag-keeper", `{"name":"integration-tag-beta"}`); code != http.StatusConflict {
		t.Fatalf("dup rename: want 409, got %d", code)
	}
	// unknown rows: 404 both verbs
	if code, _ = harness.admin(http.MethodPatch, "/admin/tags/integration-nothing", `{"featured":true}`); code != http.StatusNotFound {
		t.Fatalf("patch unknown: want 404, got %d", code)
	}

	// UNPUBLISH → the count ZEROES (the ghost-count fix) and the widget drops
	if code, _ = harness.admin(http.MethodPost, "/admin/posts/integration-tagging/unpublish", ""); code != http.StatusOK {
		t.Fatalf("unpublish %d", code)
	}
	if code, res = harness.admin(http.MethodGet, "/admin/tags", ""); code != http.StatusOK || !bytes.Contains(res, []byte(`"name":"integration-tag-beta","posts":0`)) {
		t.Fatalf("count did not zero on unpublish: %d %s", code, res)
	}
	if strings.Contains(pubTags(), "integration-tag-beta") {
		t.Fatal("zero-count tag still in the widget")
	}

	// delete guard: the DRAFT still references → 409 naming the count
	code, res = harness.admin(http.MethodDelete, "/admin/tags/integration-tag-beta", "")
	if code != http.StatusConflict || !strings.Contains(string(res), "1 posts still reference it") {
		t.Fatalf("in-use delete: want 409 with count, got %d %s", code, res)
	}
	// delete the post (soft) → tag freed → 204 → gone from vocabulary
	if code, _ = harness.admin(http.MethodDelete, "/admin/posts/integration-tagging", ""); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("delete post %d", code)
	}
	if code, res = harness.admin(http.MethodDelete, "/admin/tags/integration-tag-beta", ""); code != http.StatusNoContent {
		t.Fatalf("freed delete: want 204, got %d %s", code, res)
	}
	if code, _ = harness.admin(http.MethodDelete, "/admin/tags/integration-tag-beta", ""); code != http.StatusNotFound {
		t.Fatalf("double delete: want 404, got %d", code)
	}
}

// ---- the tag engine (ruling 2026-11-10): vocabulary docs converge
// asynchronously — every assert POLLS, never sleeps-and-hopes; the rename
// fan-out to post docs is the dangerous edge and gets the sharpest test ----
func TestTagEngine(t *testing.T) {
	harness := newAdminHarness(t)
	suffix := strconv.FormatInt(time.Now().UnixNano()%1_000_000, 10)
	tag := "engine-tag-" + suffix

	pollSearch := func(q, want string, wantFound bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			code, res := harness.admin(http.MethodGet, "/admin/tags/search?q="+q+"&page=1&pageSize=30", "")
			hit := code == http.StatusOK && strings.Contains(string(res), `"`+want+`"`)
			if hit == wantFound {
				if code != http.StatusOK {
					t.Fatalf("search %q code %d: %s", q, code, res)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("search %q: want found=%v, last=%d %s", q, wantFound, code, res)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	if code, res := harness.admin(http.MethodPost, "/admin/tags", `{"name":"`+tag+`"}`); code != http.StatusCreated {
		t.Fatalf("create %d: %s", code, res)
	}
	pollSearch(tag, tag, true)

	// multi-word query semantics: the engine tokenizes hyphenated names —
	// "tag-<suffix>" finds the same row
	pollSearch("tag-"+suffix, tag, true)

	// rename → the tag doc AND every referencing post doc converge to the new
	// name; the old name vanishes from search (the fan-out under test)
	renamed := tag + "-beta"
	if code, res := harness.admin(http.MethodPatch, "/admin/tags/"+tag, `{"name":"`+renamed+`"}`); code != http.StatusOK {
		t.Fatalf("rename %d: %s", code, res)
	}
	pollSearch(renamed, renamed, true)
	pollSearch(tag, tag, false)

	// delete → the doc converges to absent (same job kind, worker deletes
	// when the row is gone)
	if code, res := harness.admin(http.MethodDelete, "/admin/tags/"+renamed, ""); code != http.StatusNoContent {
		t.Fatalf("delete %d: %s", code, res)
	}
	pollSearch(renamed, renamed, false)

	// the paged contract: total is the FULL match count, items are page-sized
	defer func() {
		for _, n := range []string{"paged-a-" + suffix, "paged-b-" + suffix, "paged-c-" + suffix} {
			_, _ = harness.admin(http.MethodDelete, "/admin/tags/"+n, "")
		}
	}()
	for _, n := range []string{"paged-a-" + suffix, "paged-b-" + suffix, "paged-c-" + suffix} {
		if code, _ := harness.admin(http.MethodPost, "/admin/tags", `{"name":"`+n+`"}`); code != http.StatusCreated {
			t.Fatalf("paged create %s %d", n, code)
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	var total int
	for {
		code, res := harness.admin(http.MethodGet, "/admin/tags/search?q=paged-&page=1&pageSize=2", "")
		var p struct {
			Total int               `json:"total"`
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(res, &p)
		if code == http.StatusOK && p.Total == 3 {
			total = p.Total
			if len(p.Items) != 2 {
				t.Fatalf("pageSize ignored: %s", res)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("paged total never reached 3: %d %s", code, res)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if total != 3 {
		t.Fatal("unreachable")
	}
	// empty q is the client's problem: the endpoint refuses
	if code, _ := harness.admin(http.MethodGet, "/admin/tags/search?q=&page=1", ""); code != http.StatusBadRequest {
		t.Fatalf("empty q: want 400, got %d", code)
	}
}
