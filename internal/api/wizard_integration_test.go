package api_test

// TestSetupWizardE2E — the multi-user + first-boot lifecycle, end to end, on
// a SCRATCH SCHEMA of the integration database (search_path trick): the users
// table there is genuinely empty, so the TOFU registration is tested as the
// product behaves — without ever touching the shared 'fzr-test' battery user
// or any real account. DROP SCHEMA CASCADE at cleanup; the maintenance
// connection only creates/drops the schema.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/api/admin"
	"github.com/kanatabi/lumen/internal/store"
	"github.com/kanatabi/lumen/internal/throttle"
)

func wizCall(t *testing.T, url, method, token string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	_ = json.Unmarshal(data, &out) // some responses (204) are empty: fine
	return res.StatusCode, out
}

func TestSetupWizardE2E(t *testing.T) {
	if os.Getenv("LUMEN_INTEGRATION") == "" {
		t.Skip("LUMEN_INTEGRATION unset")
	}
	base := os.Getenv("LUMEN_DB_URL")
	if base == "" {
		base = "postgres://lumen:lumen-dev@localhost:5433/lumen?sslmode=disable"
	}
	ctx := context.Background()
	maint, err := store.NewPool(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer maint.Close()
	schema := fmt.Sprintf("itest_wiz_%d", time.Now().UnixNano()%1_000_000_000)
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
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	svc := adminsvc.New(store.NewPG(pool), nil, nil, "wizard-session-secret-32-bytes!!", throttle.New(time.Minute))
	r := chi.NewRouter()
	admin.New(svc).Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	U := srv.URL

	// 1. the estate reads as UNCLAIMED
	code, res := wizCall(t, U+"/admin/setup", "GET", "", nil)
	if code != 200 || res["unclaimed"] != true {
		t.Fatalf("setup status: %d %v", code, res)
	}

	// 2. first admin registers — auto-login, admin role, no forced change
	code, res = wizCall(t, U+"/admin/setup/register", "POST", "",
		map[string]string{"username": "itest-admin", "password": "the-owners-choice-1"})
	if code != 201 || res["token"] == "" || res["role"] != "admin" || res["mustChange"] != false {
		t.Fatalf("register: %d %v", code, res)
	}
	adminTok, _ := res["token"].(string)

	// 3. TOFU is terminal: a second registration is Gone forever, 410
	code, _ = wizCall(t, U+"/admin/setup/register", "POST", "",
		map[string]string{"username": "squatter", "password": "not-going-to-happen-1"})
	if code != http.StatusGone {
		t.Fatalf("second register must be 410, got %d", code)
	}
	code, res = wizCall(t, U+"/admin/setup", "GET", "", nil)
	if res["unclaimed"] != false {
		t.Fatalf("claimed estate must report unclaimed=false: %v", res)
	}

	// 4. login carries role + mustChange for routing
	code, res = wizCall(t, U+"/admin/session", "POST", "",
		map[string]string{"username": "itest-admin", "password": "the-owners-choice-1"})
	if code != 200 || res["role"] != "admin" || res["mustChange"] != false {
		t.Fatalf("login shape: %d %v", code, res)
	}

	// 5. admin creates an editor: server temp password, once, well-formed
	code, res = wizCall(t, U+"/admin/users", "POST", adminTok,
		map[string]string{"username": "itest-ed", "role": "editor"})
	if code != 201 {
		t.Fatalf("create user: %d %v", code, res)
	}
	temp, _ := res["tempPassword"].(string)
	if !regexp.MustCompile(`^(?:[a-z]{4}-){4}[0-9]{4}$`).MatchString(temp) {
		t.Fatalf("temp password shape: %q", temp)
	}

	// 6. editor first login: mustChange TRUE; 428 gates everything but escape doors
	code, res = wizCall(t, U+"/admin/session", "POST", "",
		map[string]string{"username": "itest-ed", "password": temp})
	if code != 200 || res["mustChange"] != false {
		// mustChange comes from the row: it is TRUE here — assert via whoami below
		t.Logf("editor login: %d role=%v mc=%v", code, res["role"], res["mustChange"])
	}
	if res["mustChange"] != true {
		t.Fatalf("editor must arrive in forced-change state: %v", res)
	}
	edTok, _ := res["token"].(string)
	if code, _ = wizCall(t, U+"/admin/posts", "GET", edTok, nil); code != http.StatusPreconditionRequired {
		t.Fatalf("428 gate: got %d", code)
	}
	// whoami is an escape door (the portal needs it to know WHERE to send them)
	if code, _ = wizCall(t, U+"/admin/session", "GET", edTok, nil); code != 200 {
		t.Fatalf("whoami during forced change: %d", code)
	}

	// 7. change password: wrong current 401; right current 200; old token DIES
	if code, _ = wizCall(t, U+"/admin/session/password", "POST", edTok,
		map[string]string{"current": "wrong-one-123", "next": "editor-chosen-1"}); code != 401 {
		t.Fatalf("wrong current must 401, got %d", code)
	}
	if code, _ = wizCall(t, U+"/admin/session/password", "POST", edTok,
		map[string]string{"current": temp, "next": "editor-chosen-1"}); code != 200 {
		t.Fatalf("change password: %d", code)
	}
	if code, _ = wizCall(t, U+"/admin/posts", "GET", edTok, nil); code != http.StatusUnauthorized {
		t.Fatalf("pre-change session must be revoked by the bump: %d", code)
	}

	// 8. post-change: clean login, mustChange gone, posts reachable; user-admin 403
	code, res = wizCall(t, U+"/admin/session", "POST", "",
		map[string]string{"username": "itest-ed", "password": "editor-chosen-1"})
	if code != 200 || res["mustChange"] != false {
		t.Fatalf("post-change login: %d %v", code, res)
	}
	edTok2, _ := res["token"].(string)
	if code, _ = wizCall(t, U+"/admin/posts", "GET", edTok2, nil); code != 200 {
		t.Fatalf("editor reading posts: %d", code)
	}
	if code, _ = wizCall(t, U+"/admin/users", "GET", edTok2, nil); code != http.StatusForbidden {
		t.Fatalf("editor on user-admin must 403, got %d", code)
	}
	code, res = wizCall(t, U+"/admin/users", "GET", adminTok, nil)
	if code != 200 {
		t.Fatalf("admin list users: %d %v", code, res)
	}

	// 9. reset: new temp differs, must_change rearms, previous session dies
	code, res = wizCall(t, U+"/admin/users/itest-ed/reset-password", "POST", adminTok, map[string]string{})
	if code != 200 {
		t.Fatalf("reset: %d %v", code, res)
	}
	temp2, _ := res["tempPassword"].(string)
	if temp2 == "" || temp2 == temp {
		t.Fatalf("reset must produce a fresh temp: %q vs %q", temp2, temp)
	}
	if code, _ = wizCall(t, U+"/admin/posts", "GET", edTok2, nil); code != http.StatusUnauthorized {
		t.Fatalf("reset must revoke live sessions: %d", code)
	}
}
