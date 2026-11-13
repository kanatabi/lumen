// config_keys_test.go — the bindings surface is machine-readable BY
// CONTRACT (the epoch-agent P3 parses this output), so its exact lines
// are golden: role truth on one hand, the full LUMEN_* family on the
// other. If a binding table changes, this test changes — deliberately,
// in the same commit, and the diff IS the review signal.
package main

import (
	"strings"
	"testing"
)

func TestConfigKeysGoldenAdmin(t *testing.T) {
	out, err := configKeysOutput("admin")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"LUMEN_BAO_ADDR",
		"LUMEN_BAO_ROLE_ID",
		"LUMEN_BAO_SECRET_ID",
		"LUMEN_CONFIG",
		"LUMEN_DB_URL",
		"LUMEN_PORT",
		"LUMEN_ROLE",
		"LUMEN_SESSION_JWT_SECRET",
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("admin keys drifted:\ngot:\n%s\nwant:\n%s", out, want)
	}
}

func TestConfigKeysGoldenCMS(t *testing.T) {
	out, err := configKeysOutput("cms")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"LUMEN_ADMIN_URL",
		"LUMEN_CONFIG",
		"LUMEN_DB_URL",
		"LUMEN_MEDIA_ACCESS_KEY",
		"LUMEN_MEDIA_BUCKET",
		"LUMEN_MEDIA_CDN_BASE",
		"LUMEN_MEDIA_CREATE_BUCKET",
		"LUMEN_MEDIA_ENDPOINT",
		"LUMEN_MEDIA_FORCE_PATH",
		"LUMEN_MEDIA_PREFIX",
		"LUMEN_MEDIA_REGION",
		"LUMEN_MEDIA_SECRET_KEY",
		"LUMEN_MEILI_INDEX_PHOTOS",
		"LUMEN_MEILI_INDEX_POSTS",
		"LUMEN_MEILI_KEY",
		"LUMEN_MEILI_URL",
		"LUMEN_ORIGIN",
		"LUMEN_PORT",
		"LUMEN_ROLE",
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("cms keys drifted:\ngot:\n%s", out)
	}
}

func TestConfigKeysAllIsUnion(t *testing.T) {
	a, _ := configKeysOutput("admin")
	c, _ := configKeysOutput("cms")
	x, err := configKeysOutput("all")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range strings.Split(a+c, "\n") {
		if k != "" && !strings.Contains(x, k+"\n") {
			t.Fatalf("all-plane output missing %s", k)
		}
	}
}

func TestConfigKeysRejectsBadRole(t *testing.T) {
	if _, err := configKeysOutput("gateway"); err == nil {
		t.Fatal("bad role must be an error, not a shrug")
	}
}
