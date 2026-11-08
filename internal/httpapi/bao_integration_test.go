package httpapi_test

// bao_integration_test.go — §9 against a REAL KV-v2 + AppRole server
// (compose's dev vault; production is OpenBao, same wire API). Bootstraps
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

// bootstrapVault builds the §5.6-shaped plane once per run and returns
// (roleID, secretID) for the relay's identity.
func bootstrapVault(t *testing.T) (string, string) {
	t.Helper()
	addr := vaultAddr(t)
	root := os.Getenv("LUMEN_BAO_DEV_ROOT")
	if root == "" {
		root = "dev-root-token"
	}
	cfg := vault.DefaultConfig()
	cfg.Address = addr
	vc, err := vault.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	vc.SetToken(root)
	ctx := context.Background()

	// kv-v2 at kv/ (idempotent)
	if err := vc.Sys().MountWithContext(ctx, "kv", &vault.MountInput{
		Type: "kv", Options: map[string]string{"version": "2"},
	}); err != nil && !strings.Contains(err.Error(), "already in use") {
		t.Fatal(err)
	}
	policy := `
path "kv/data/*"      { capabilities = ["read", "list", "create", "update"] }
path "kv/metadata/*"  { capabilities = ["read", "list"] }
`
	if err := vc.Sys().PutPolicyWithContext(ctx, "fzr-portal", policy); err != nil {
		t.Fatal(err)
	}
	if err := vc.Sys().EnableAuthWithOptionsWithContext(ctx, "approle", &vault.EnableAuthOptions{Type: "approle"}); err != nil {
		if !strings.Contains(err.Error(), "already in use") {
			t.Fatal(err)
		}
	}
	if _, err := vc.Logical().WriteWithContext(ctx, "auth/approle/role/fzr-portal", map[string]any{
		"token_policies":  "fzr-portal",
		"token_ttl":       "5m", // exercises the relay's re-login path quickly
		"bound_cidr_list": "",
		"secret_id_ttl":   "0",
		"token_type":      "service",
	}); err != nil {
		t.Fatal(err)
	}
	// start clean: dev vault persists across runs; the relay test owns
	// lumen/dev and probes, so purge its paths (hard-delete all versions)
	for _, p := range []string{"kv/metadata/lumen/dev", "kv/metadata/probe/test"} {
		if _, err := vc.Logical().DeleteWithContext(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	roleID, err := vc.Logical().ReadWithContext(ctx, "auth/approle/role/fzr-portal/role-id")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := vc.Logical().WriteWithContext(ctx, "auth/approle/role/fzr-portal/secret-id", nil)
	if err != nil {
		t.Fatal(err)
	}
	return roleID.Data["role_id"].(string), secret.Data["secret_id"].(string)
}

func TestBaoRelay(t *testing.T) {
	roleID, secretID := bootstrapVault(t)
	t.Setenv("LUMEN_BAO_ROLE_ID", roleID)
	t.Setenv("LUMEN_BAO_SECRET_ID", secretID)

	ah := newAdminHarness(t)
	if !bao.Enabled() {
		t.Skip("bao not enabled after setenv")
	}
	bc, err := bao.New()
	if err != nil {
		t.Fatal(err)
	}
	ah.deps.Bao = bc // §9 surface mounts only when the plane is configured
	ah.rebuild()

	// ---- create via cas:0 ----
	body := `{"data":{"PUBLIC_API_BASE":"http://lumen.http-server:8080","SECRET_MARKER":"s3cr3t-v4lue-MUST-NOT-BE-LOGGED"},"cas":0}`
	code, res := ah.admin(http.MethodPut, "/admin/bao/lumen/dev", body)
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

	code, res = ah.admin(http.MethodGet, "/admin/bao/lumen/dev", "")
	if code != http.StatusOK {
		t.Fatalf("read %d: %s", code, res)
	}
	if !bytes.Contains(res, []byte("s3cr3t-v4lue-MUST-NOT-BE-LOGGED")) {
		t.Fatal("relay response lost the value (the UI needs it to mask!)")
	}
	_, _ = ah.admin(http.MethodPut, "/admin/bao/lumen/dev", `{"data":{"SECRET_MARKER":"s3cr3t-v4lue-MUST-NOT-BE-LOGGED","A":"B"},"cas":1}`)
	slog.SetDefault(prev)
	if strings.Contains(logbuf.String(), "s3cr3t-v4lue-MUST-NOT-BE-LOGGED") {
		t.Fatalf("secret value appeared in lumen logs:\n%s", logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "bao.write") || !strings.Contains(logbuf.String(), "bao.read") {
		t.Fatalf("expected op-level audit lines, got:\n%s", logbuf.String())
	}

	// ---- stale cas is a 409, never a silent clobber ----
	if code, _ := ah.admin(http.MethodPut, "/admin/bao/lumen/dev", `{"data":{"X":"Y"},"cas":0}`); code != http.StatusConflict {
		t.Fatalf("stale cas code %d, want 409", code)
	}

	// ---- version pin + metadata ----
	code, res = ah.admin(http.MethodGet, "/admin/bao/lumen/dev?version=1", "")
	if code != http.StatusOK || !bytes.Contains(res, []byte("PUBLIC_API_BASE")) || bytes.Contains(res, []byte(`"A":"B"`)) {
		t.Fatalf("version pin: %d %s", code, res)
	}
	// metadata via the ?meta flag — presence-based, mirroring mellon's relay
	code, res = ah.admin(http.MethodGet, "/admin/bao/lumen/dev?meta", "")
	if code != http.StatusOK {
		t.Fatalf("meta %d: %s", code, res)
	}
	var m bao.Meta
	_ = json.Unmarshal(res, &m)
	if m.CurrentVersion != 2 || len(m.Versions) != 2 {
		t.Fatalf("meta %+v", m)
	}

	// ---- paths enumeration ----
	code, res = ah.admin(http.MethodGet, "/admin/bao/paths", "")
	if code != http.StatusOK || !bytes.Contains(res, []byte(`"lumen"`)) {
		t.Fatalf("paths %d: %s", code, res)
	}

	// NOTE on containment: the RELAY exposes no sys/ path by construction —
	// there is literally no route that reaches one; the policy side is the
	// §5.6 bootstrap's job in the real plane (mirrored by this dev vault).
}
