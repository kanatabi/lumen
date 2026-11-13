// rolesplit_integration_test.go — the role split END-TO-END against real
// Postgres (LUMEN_INTEGRATION=1, per the existing battery gate):
//
//  1. fresh cms database ⇒ ZERO users table (the ruling made a test);
//     fresh admin database ⇒ no content tables either way.
//  2. the admin plane serves the mint + verify door: claim (TOFU),
//     login, verify (wire shape asserted EXACTLY — the mellon contract),
//     revoke, verify 401.
//  3. a cms router INTROSPECTS that live admin plane: desk 200 while the
//     token lives, desk 401 after logout once the (shortened) cache
//     window rolls, mint-shaped routes ABSENT from the cms router (404
//     chi-default = honest nonexistence), and an admin plane DOWN turns
//     desk requests into 503 — fail closed, observed not asserted.
package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/api"
	"github.com/kanatabi/lumen/internal/api/admin"
	"github.com/kanatabi/lumen/internal/api/public"
	"github.com/kanatabi/lumen/internal/authz"
	"github.com/kanatabi/lumen/internal/role"
	"github.com/kanatabi/lumen/internal/service"
	"github.com/kanatabi/lumen/internal/store"
	"github.com/kanatabi/lumen/internal/throttle"
)

func integrationDB(t *testing.T) (base string) {
	t.Helper()
	if os.Getenv("LUMEN_INTEGRATION") == "" {
		t.Skip("LUMEN_INTEGRATION unset")
	}
	base = os.Getenv("LUMEN_DB_URL")
	if base == "" {
		base = "postgres://lumen:lumen-dev@localhost:5433/lumen?sslmode=disable"
	}
	return base
}

// freshSchema gives a test an isolated, empty schema in the dev cluster
// (same trick as the wizard battery) and a pool bound to it.
func freshSchema(t *testing.T, base, tag string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	maint, err := store.NewPool(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(maint.Close)
	schema := fmt.Sprintf("rolesplit_%s_%d", tag, time.Now().UnixNano()%1_000_000_000)
	if _, err := maint.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = maint.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	dsn := base
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + schema
	} else {
		dsn += "?search_path=" + schema
	}
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := maint.Exec(ctx, `GRANT ALL ON SCHEMA `+schema+` TO CURRENT_USER`); err != nil {
		t.Log("grant skipped: ", err)
	}
	return pool
}

func hasTable(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestMigrateForRole_FreshDatabases(t *testing.T) {
	base := integrationDB(t)
	ctx := context.Background()

	t.Run("cms subset yields ZERO users table", func(t *testing.T) {
		pool := freshSchema(t, base, "cms")
		n, err := store.MigrateForRole(ctx, pool, role.CMS)
		if err != nil {
			t.Fatal(err)
		}
		if n != 5 { // 0001,0003,0004,0005,0006
			t.Fatalf("cms subset size = %d, want 5", n)
		}
		if hasTable(t, pool, "users") {
			t.Fatal("RULING VIOLATION: a fresh cms database grew a users table")
		}
		for _, want := range []string{"posts", "photos", "tags", "collections", "media_assets"} {
			if !hasTable(t, pool, want) {
				t.Fatalf("cms database missing %s", want)
			}
		}
	})

	t.Run("admin subset yields credentials only", func(t *testing.T) {
		adminPool := freshSchema(t, base, "admin")
		n, err := store.MigrateForRole(ctx, adminPool, role.Admin)
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 { // 0002 + 0007
			t.Fatalf("admin subset size = %d, want 2", n)
		}
		if !hasTable(t, adminPool, "users") {
			t.Fatal("admin database lost the users table")
		}
		for _, forbidden := range []string{"posts", "photos", "tags", "collections", "media_assets", "river_job"} {
			if hasTable(t, adminPool, forbidden) {
				t.Fatalf("admin database grew %s", forbidden)
			}
		}
	})
}

func callJSON(t *testing.T, url, method, bearer string, body any) (int, map[string]any, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	_ = json.Unmarshal(data, &out)
	return res.StatusCode, out, string(data)
}

func TestRoleSplit_VerifyDoorAndCMSPlane(t *testing.T) {
	base := integrationDB(t)
	ctx := context.Background()
	secret := "rolesplit-session-secret-32-bytes!!"

	// ---- the ADMIN plane (its own database, schema-isolated) ----
	adminPool := freshSchema(t, base, "admp")
	adminPG := store.NewPG(adminPool)
	if _, err := store.MigrateForRole(ctx, adminPool, role.Admin); err != nil {
		t.Fatal(err)
	}
	adminSvc := adminsvc.New(adminPG, nil, nil, secret, throttle.New(time.Minute))
	adminMux := chi.NewRouter()
	admin.New(adminSvc).Mount(adminMux, role.PlaneAdmin)
	adminSrv := httptest.NewServer(adminMux)
	defer adminSrv.Close()

	// TOFU claim — the wizard is admin's, and it is HERE
	code, res, raw := callJSON(t, adminSrv.URL+"/admin/setup/register", http.MethodPost, "",
		map[string]string{"username": "rolesplit-owner", "password": "the-owners-choice-1"})
	if code != http.StatusCreated || res["token"] == nil {
		t.Fatalf("register: %d %s", code, raw)
	}
	token := res["token"].(string)

	// verify — THE WIRE SHAPE mellon/agents must match, asserted verbatim
	code, res, raw = callJSON(t, adminSrv.URL+"/admin/session/verify", http.MethodPost, "Bearer "+token, nil)
	if code != http.StatusOK {
		t.Fatalf("verify live token: %d %s", code, raw)
	}
	if res["username"] != "rolesplit-owner" || res["role"] != "admin" || res["mustChange"] != false || len(res) != 3 {
		t.Fatalf("verify 200 shape drift: %s", raw)
	}

	// the admin plane has NO content surface (honest absence)
	if code, _, _ := callJSON(t, adminSrv.URL+"/admin/posts", http.MethodGet, "Bearer "+token, nil); code != http.StatusNotFound {
		t.Fatalf("admin plane served a content desk: %d", code)
	}
	if code, _, _ := callJSON(t, adminSrv.URL+"/api/posts", http.MethodGet, "", nil); code != http.StatusNotFound {
		t.Fatalf("admin plane served §1 reads: %d", code)
	}

	// ---- the CMS plane (content database, introspecting the door) ----
	cmsPool := freshSchema(t, base, "cmsp")
	cmsPG := store.NewPG(cmsPool)
	if _, err := store.MigrateForRole(ctx, cmsPool, role.CMS); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, cmsPool, "users") {
		t.Fatal("cms harness database grew a users table — abort")
	}
	cmsSvc := adminsvc.New(cmsPG, nil, nil, "", throttle.New(time.Minute))
	cmsRouter := api.NewRouter(
		public.New(service.New(nil, nil), nil), // engine-less: q="" desk lists are truth-direct
		admin.NewCMS(cmsSvc, authz.NewRemote(adminSrv.URL).WithTTL(100*time.Millisecond)),
		role.CMS.Planes(),
	)
	cmsSrv := httptest.NewServer(cmsRouter)
	defer cmsSrv.Close()

	// desk answers while the session lives…
	if code, _, raw := callJSON(t, cmsSrv.URL+"/admin/posts", http.MethodGet, "Bearer "+token, nil); code != http.StatusOK {
		t.Fatalf("cms desk with valid session: %d %s", code, raw)
	}
	// …and the mint is NOT on this plane (404 = absence, not theater)
	if code, _, _ := callJSON(t, cmsSrv.URL+"/admin/session", http.MethodPost, "", map[string]string{}); code != http.StatusNotFound {
		t.Fatalf("cms plane can MINT sessions: %d", code)
	}
	// dead token straight to the door → 401 at the desk
	if code, _, _ := callJSON(t, cmsSrv.URL+"/admin/posts", http.MethodGet, "Bearer garbage.jwt.here", nil); code != http.StatusUnauthorized {
		t.Fatalf("cms desk accepted a forged bearer: %d", code)
	}

	// revocation propagates within the cache window (TTL shortened for
	// the test; production bound is the documented 30 s)
	if code, _, _ := callJSON(t, cmsSrv.URL+"/admin/session", http.MethodDelete, "Bearer "+token, nil); code != http.StatusNoContent {
		t.Fatalf("logout at admin: %d", code)
	}
	time.Sleep(150 * time.Millisecond)
	if code, _, _ := callJSON(t, cmsSrv.URL+"/admin/posts", http.MethodGet, "Bearer "+token, nil); code != http.StatusUnauthorized {
		t.Fatalf("REVOKED session still serves the cms desk after the cache window: %d", code)
	}

	// ---- FAIL CLOSED: take the door away, the desk must 503 ----
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close() // refuse like a dead service: connection refused from here on
	cmsDown := api.NewRouter(
		public.New(service.New(nil, nil), nil),
		admin.NewCMS(cmsSvc, authz.NewRemote(gone.URL)),
		role.CMS.Planes(),
	)
	downSrv := httptest.NewServer(cmsDown)
	defer downSrv.Close()
	code, res, _ = callJSON(t, downSrv.URL+"/admin/posts", http.MethodGet, "Bearer anything", nil)
	if code != http.StatusServiceUnavailable || res["error"] != "admin plane unreachable" {
		t.Fatalf("admin unreachable must FAIL CLOSED: %d %v", code, res)
	}
}
