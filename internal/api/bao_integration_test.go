package api_test

// bao_integration_test.go — §9 against the REAL plane: compose runs
// OpenBao (ghcr-pinned 2.5.3) here too, so this battery executes against
// the production product, not a stand-in. Bootstraps
// the exact §5.6 layout — kv mount, policy that sees ONLY kv/*, an approle
// with no sys rights — then drives the relay: create via cas:0, conflicts
// as 409, version pins, metadata, and the REDACTION proof: every secret
// value written/read while a capturing slog handler is attached, then the
// log is demanded clean of them.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	vault "github.com/hashicorp/vault/api"

	"github.com/kanatabi/lumen/internal/bao"
)

func vaultAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("LUMEN_BAO_ADDR")
	if addr == "" {
		t.Skip("LUMEN_BAO_ADDR unset (compose bao service: http://localhost:8200)")
	}
	return addr
}

// bootstrapPlane builds the §5.6-shaped plane once per run and returns
// (roleID, secretID) for the relay's identity.
func bootstrapPlane(t *testing.T) (string, string) {
	t.Helper()
	addr := vaultAddr(t)
	root := os.Getenv("LUMEN_BAO_DEV_ROOT")
	if root == "" {
		root = "dev-root-token"
	}
	cfg := vault.DefaultConfig()
	cfg.Address = addr
	vaultAdmin, err := vault.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	vaultAdmin.SetToken(root)
	ctx := context.Background()

	// kv-v2 at kv/ (idempotent)
	if err := vaultAdmin.Sys().MountWithContext(ctx, "kv", &vault.MountInput{
		Type: "kv", Options: map[string]string{"version": "2"},
	}); err != nil && !strings.Contains(err.Error(), "already in use") {
		t.Fatal(err)
	}
	policy := `
path "kv/data/*"      { capabilities = ["read", "list", "create", "update"] }
path "kv/metadata/*"  { capabilities = ["read", "list", "patch"] }
`
	if err := vaultAdmin.Sys().PutPolicyWithContext(ctx, "fzr-portal", policy); err != nil {
		t.Fatal(err)
	}
	if err := vaultAdmin.Sys().EnableAuthWithOptionsWithContext(ctx, "approle", &vault.EnableAuthOptions{Type: "approle"}); err != nil {
		if !strings.Contains(err.Error(), "already in use") {
			t.Fatal(err)
		}
	}
	if _, err := vaultAdmin.Logical().WriteWithContext(ctx, "auth/approle/role/fzr-portal", map[string]any{
		"token_policies":  "fzr-portal",
		"token_ttl":       "5m", // exercises the relay's re-login path quickly
		"bound_cidr_list": "",
		"secret_id_ttl":   "0",
		"token_type":      "service",
	}); err != nil {
		t.Fatal(err)
	}
	// start clean: the dev server persists across runs (single container
	// lifetime); the battery owns ONLY its itest prefix (C5 ruling: the
	// relay test used to purge/reseed lumen/dev — the very path a human
	// browses in the portal — and a stale-worker collision made that real).
	// Probes stay test-side: the `probe` app exists for no one else.
	for _, p := range []string{"kv/metadata/itest/relay", "kv/metadata/probe/test"} {
		if _, err := vaultAdmin.Logical().DeleteWithContext(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	roleID, err := vaultAdmin.Logical().ReadWithContext(ctx, "auth/approle/role/fzr-portal/role-id")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := vaultAdmin.Logical().WriteWithContext(ctx, "auth/approle/role/fzr-portal/secret-id", nil)
	if err != nil {
		t.Fatal(err)
	}
	return roleID.Data["role_id"].(string), secret.Data["secret_id"].(string)
}

func TestBaoRelay(t *testing.T) {
	roleID, secretID := bootstrapPlane(t)
	t.Setenv("LUMEN_BAO_ROLE_ID", roleID)
	t.Setenv("LUMEN_BAO_SECRET_ID", secretID)

	harness := newAdminHarness(t)
	if !bao.Enabled() {
		t.Skip("bao not enabled after setenv")
	}
	baoClient, err := bao.New()
	if err != nil {
		t.Fatal(err)
	}
	harness.adminSvc.EnableBao(baoClient) // §9 surface mounts only when the plane is configured
	harness.rebuild()

	// ---- create via cas:0 ----
	body := `{"data":{"PUBLIC_API_BASE":"http://lumen.http-server:8080","SECRET_MARKER":"s3cr3t-v4lue-MUST-NOT-BE-LOGGED"},"cas":0}`
	code, res := harness.admin(http.MethodPut, "/admin/bao/itest/relay", body)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create %d: %s", code, res)
	}
	var wr struct{ Version int }
	_ = json.Unmarshal(res, &wr)
	if wr.Version != 1 {
		t.Fatalf("first version = %d", wr.Version)
	}

	// ---- redaction proof: capture slog while reading & writing ----
	var logbuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, nil)))
	defer slog.SetDefault(prev)

	code, res = harness.admin(http.MethodGet, "/admin/bao/itest/relay", "")
	if code != http.StatusOK {
		t.Fatalf("read %d: %s", code, res)
	}
	if !bytes.Contains(res, []byte("s3cr3t-v4lue-MUST-NOT-BE-LOGGED")) {
		t.Fatal("relay response lost the value (the UI needs it to mask!)")
	}
	_, _ = harness.admin(http.MethodPut, "/admin/bao/itest/relay", `{"data":{"SECRET_MARKER":"s3cr3t-v4lue-MUST-NOT-BE-LOGGED","A":"B"},"cas":1}`)
	slog.SetDefault(prev)
	if strings.Contains(logbuf.String(), "s3cr3t-v4lue-MUST-NOT-BE-LOGGED") {
		t.Fatalf("secret value appeared in lumen logs:\n%s", logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "bao.write") || !strings.Contains(logbuf.String(), "bao.read") {
		t.Fatalf("expected op-level audit lines, got:\n%s", logbuf.String())
	}

	// ---- stale cas is a 409, never a silent clobber ----
	if code, _ := harness.admin(http.MethodPut, "/admin/bao/itest/relay", `{"data":{"X":"Y"},"cas":0}`); code != http.StatusConflict {
		t.Fatalf("stale cas code %d, want 409", code)
	}

	// ---- version pin + metadata ----
	code, res = harness.admin(http.MethodGet, "/admin/bao/itest/relay?version=1", "")
	if code != http.StatusOK || !bytes.Contains(res, []byte("PUBLIC_API_BASE")) || bytes.Contains(res, []byte(`"A":"B"`)) {
		t.Fatalf("version pin: %d %s", code, res)
	}
	// metadata via the ?meta flag — presence-based, mirroring mellon's relay
	code, res = harness.admin(http.MethodGet, "/admin/bao/itest/relay?meta", "")
	if code != http.StatusOK {
		t.Fatalf("meta %d: %s", code, res)
	}
	var m bao.Meta
	_ = json.Unmarshal(res, &m)
	if m.CurrentVersion != 2 || len(m.Versions) != 2 {
		t.Fatalf("meta %+v", m)
	}

	// ---- paths enumeration ----
	code, res = harness.admin(http.MethodGet, "/admin/bao/paths", "")
	if code != http.StatusOK || !bytes.Contains(res, []byte(`"lumen"`)) {
		t.Fatalf("paths %d: %s", code, res)
	}

	// NOTE on containment: the RELAY exposes no sys/ path by construction —
	// there is literally no route that reaches one; the policy side is the
	// §5.6 bootstrap's job in the real plane (this dev OpenBao mirrors it).
}

// ---- A5: the control plane against the REAL engine (PLAN §3) ----

func purgePaths(t *testing.T, paths ...string) {
	t.Helper()
	addr := vaultAddr(t)
	root := os.Getenv("LUMEN_BAO_DEV_ROOT")
	if root == "" {
		root = "dev-root-token"
	}
	cfg := vault.DefaultConfig()
	cfg.Address = addr
	v, err := vault.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	v.SetToken(root)
	for _, p := range paths {
		if _, err := v.Logical().DeleteWithContext(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
}

func readPayload(t *testing.T, harness *adminHarness, app, env string) (map[string]string, int) {
	t.Helper()
	code, res := harness.admin(http.MethodGet, "/admin/bao/"+app+"/"+env, "")
	if code != http.StatusOK {
		t.Fatalf("read %s/%s: %d %s", app, env, code, res)
	}
	var p struct {
		Data    map[string]string `json:"data"`
		Version int               `json:"version"`
	}
	if err := json.Unmarshal(res, &p); err != nil {
		t.Fatal(err)
	}
	return p.Data, p.Version
}

func metaOf(t *testing.T, harness *adminHarness, app, env string) (int, *int) {
	t.Helper()
	code, res := harness.admin(http.MethodGet, "/admin/bao/"+app+"/"+env+"?meta", "")
	if code != http.StatusOK {
		t.Fatalf("meta %s/%s: %d %s", app, env, code, res)
	}
	var m struct {
		CurrentVersion int  `json:"currentVersion"`
		PinnedVersion  *int `json:"pinnedVersion"`
	}
	if err := json.Unmarshal(res, &m); err != nil {
		t.Fatal(err)
	}
	return m.CurrentVersion, m.PinnedVersion
}

func TestBaoControlPlane(t *testing.T) {
	roleID, secretID := bootstrapPlane(t)
	t.Setenv("LUMEN_BAO_ROLE_ID", roleID)
	t.Setenv("LUMEN_BAO_SECRET_ID", secretID)

	// run-to-run hygiene: the plane persists; own the itest-app prefix
	purgePaths(t,
		"kv/metadata/itest-app/dev", "kv/metadata/itest-app/staging",
		"kv/metadata/itest-app/production", "kv/metadata/itest-app/lab")

	harness := newAdminHarness(t)
	if !bao.Enabled() {
		t.Skip("bao not enabled after setenv")
	}
	baoClient, err := bao.New()
	if err != nil {
		t.Fatal(err)
	}
	harness.adminSvc.EnableBao(baoClient)
	harness.rebuild()

	// ---- onboard: trio arrives, refusals refuse ----
	code, res := harness.admin(http.MethodPost, "/admin/bao/apps", `{"app":"itest-app"}`)
	if code != http.StatusOK {
		t.Fatalf("onboard %d: %s", code, res)
	}
	var ob struct {
		Envs []string `json:"envs"`
	}
	_ = json.Unmarshal(res, &ob)
	if len(ob.Envs) != 3 {
		t.Fatalf("trio not defaulted: %s", res)
	}
	if code, _ := harness.admin(http.MethodPost, "/admin/bao/apps", `{"app":"itest-app"}`); code != http.StatusConflict {
		t.Fatalf("duplicate onboard: want 409, got %d", code)
	}
	for _, bad := range []string{`{"app":"itest_bad"}`, `{"app":"ITest"}`} {
		if code, _ := harness.admin(http.MethodPost, "/admin/bao/apps", bad); code != http.StatusBadRequest {
			t.Fatalf("slug %s: want 400, got %d", bad, code)
		}
	}

	// Every PUT below CASes against a META READ, not a remembered number:
	// ANY structural save mirrors to the siblings (bumping THEM), so the
	// naive "trio v1, I wrote once" script 409'd against the relay's own
	// correctness — exactly the invariant, proven by its own collision.
	headCAS := func(env string) int { cv, _ := metaOf(t, harness, "itest-app", env); return cv }
	write := func(env string, data map[string]string) int {
		b, _ := json.Marshal(map[string]any{"data": data, "cas": headCAS(env)})
		code, res := harness.admin(http.MethodPut, "/admin/bao/itest-app/"+env, string(b))
		if code != http.StatusOK {
			t.Fatalf("write %s %d: %s", env, code, res)
		}
		var wr struct{ Version int }
		_ = json.Unmarshal(res, &wr)
		return wr.Version
	}

	// a distinctive sibling value for the clobber check
	stagingSeed := "postgres://sibling-MUST-SURVIVE"
	write("staging", map[string]string{"DB_URL": stagingSeed}) // v2; mirrors DB_URL:"" into dev+production

	// ---- structural write: mirror lands on BOTH siblings ----
	saved := map[string]string{"DB_URL": "postgres://dev", "NEW": "n"}
	write("dev", saved)
	if data, v := readPayload(t, harness, "itest-app", "staging"); v != 3 || data["NEW"] != "" || data["DB_URL"] != stagingSeed {
		t.Fatalf("mirror lost sibling value/shape: %v v%d", data, v)
	}
	if data, v := readPayload(t, harness, "itest-app", "production"); v != 3 || len(data) != 2 || data["DB_URL"] != "" || data["NEW"] != "" {
		t.Fatalf("production mirror wrong: %v v%d", data, v)
	}

	// ---- value-only: siblings untouched, NO version bumps ----
	devV := write("dev", map[string]string{"DB_URL": "postgres://dev2", "NEW": "n"})
	if cv, _ := metaOf(t, harness, "itest-app", "staging"); cv != 3 {
		t.Fatalf("value-only edit bumped staging (%d)", cv)
	}
	if cv, _ := metaOf(t, harness, "itest-app", "production"); cv != 3 {
		t.Fatalf("value-only edit bumped production (%d)", cv)
	}

	// ---- removal propagates ----
	devV = write("dev", map[string]string{"DB_URL": "postgres://dev2"})
	for _, env := range []string{"staging", "production"} {
		data, _ := readPayload(t, harness, "itest-app", env)
		if _, gone := data["NEW"]; gone {
			t.Fatalf("%s kept the removed key: %v", env, data)
		}
	}

	// ---- add-env: union seed, collisions, typos ----
	if code, _ := harness.admin(http.MethodPost, "/admin/bao/itest-app/envs", `{"env":"lab"}`); code != http.StatusOK {
		t.Fatalf("add-env %d", code)
	}
	if data, v := readPayload(t, harness, "itest-app", "lab"); v != 1 || len(data) != 1 || data["DB_URL"] != "" {
		t.Fatalf("lab not seeded to the canonical union: %v", data)
	}
	if code, _ := harness.admin(http.MethodPost, "/admin/bao/itest-app/envs", `{"env":"lab"}`); code != http.StatusConflict {
		t.Fatalf("duplicate env: want 409, got %d", code)
	}
	if code, _ := harness.admin(http.MethodPost, "/admin/bao/itest-app/envs", `{"env":"Dev"}`); code != http.StatusBadRequest {
		t.Fatalf("bad env slug: want 400, got %d", code)
	}
	if code, _ := harness.admin(http.MethodPost, "/admin/bao/ghost-app/envs", `{"env":"dev"}`); code != http.StatusNotFound {
		t.Fatalf("env on unknown app: want 404, got %d", code)
	}

	// ---- pin = intent (D1): recorded, surfaced, NEVER a routing change ----
	if code, res := harness.admin(http.MethodPut, "/admin/bao/itest-app/dev/pin", `{"version":2}`); code != http.StatusOK {
		t.Fatalf("pin %d: %s", code, res)
	}
	if _, pin := metaOf(t, harness, "itest-app", "dev"); pin == nil || *pin != 2 {
		t.Fatalf("pin absent from meta: %v", pin)
	}
	if _, v := readPayload(t, harness, "itest-app", "dev"); v != devV {
		t.Fatalf("D1 violated — read routed to the pin (version %d, head is %d)", v, devV)
	}
	if code, _ := harness.admin(http.MethodPut, "/admin/bao/itest-app/dev/pin", `{"version":99}`); code != http.StatusNotFound {
		t.Fatalf("pin to unknown version: want 404, got %d", code)
	}
	if code, _ := harness.admin(http.MethodPut, "/admin/bao/itest-app/dev/pin", `{"version":0}`); code != http.StatusNotFound {
		t.Fatalf("pin version 0: want 404, got %d", code)
	}
	if code, res := harness.admin(http.MethodDelete, "/admin/bao/itest-app/dev/pin", ""); code != http.StatusNoContent {
		t.Fatalf("unpin %d: %s", code, res)
	}
	if _, pin := metaOf(t, harness, "itest-app", "dev"); pin != nil {
		t.Fatalf("unpin left intent behind: %v", pin)
	}
	if code, _ := harness.admin(http.MethodPut, "/admin/bao/ghost-app/dev/pin", `{"version":1}`); code != http.StatusNotFound {
		t.Fatalf("pin on missing path: want 404, got %d", code)
	}

	// ---- the namespace listing shows the whole grown tree ----
	code, res = harness.admin(http.MethodGet, "/admin/bao/paths", "")
	if code != http.StatusOK {
		t.Fatalf("paths %d", code)
	}
	var paths []bao.Path
	if err := json.Unmarshal(res, &paths); err != nil {
		t.Fatal(err)
	}
	mine := 0
	for _, p := range paths {
		if p.App == "itest-app" {
			mine++
		}
	}
	if mine != 4 {
		t.Fatalf("itest-app envs = %d, want 4 (dev/staging/production/lab)", mine)
	}
}
