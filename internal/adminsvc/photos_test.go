package adminsvc

import (
	"encoding/json"
	"strings"
	"testing"
)

// The EXIF allowlist — the privacy promise made structural (Round B,
// 2026-11-22): lumen stores ONLY the curated keys; GPS cannot pass
// because it is not a key (allowlist, never a denylist).
func TestSanitizePhotoExif(t *testing.T) {
	t.Run("curated keys with the right shapes survive", func(t *testing.T) {
		raw := []byte(`{"takenAt":"2026:11:22 10:00:00","camera":"Fujifilm X100VI","lens":"23mm f2","shutter":"1/250","iso":640,"aperture":2.8,"GPSLatitude":46.5,"GPSLongitude":6.6}`)
		out, err := SanitizePhotoExif(raw)
		if err != nil {
			t.Fatal(err)
		}
		if out["camera"] != "Fujifilm X100VI" || out["iso"] != float64(640) || out["aperture"] != 2.8 {
			t.Fatalf("%+v", out)
		}
		if len(out) != 6 {
			t.Fatalf("expected the full curated set, got %d keys: %+v", len(out), out)
		}
		for k := range out {
			if strings.HasPrefix(strings.ToLower(k), "gps") {
				t.Fatalf("GPS key %q leaked — the promise is broken", k)
			}
		}
	})

	t.Run("unknown keys and wrong types drop silently", func(t *testing.T) {
		raw := []byte(`{"camera":42,"iso":"high","Orientation":6,"Software":"darktable","lens":"x"}`)
		out, err := SanitizePhotoExif(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out["lens"] != "x" {
			t.Fatalf("%+v", out)
		}
	})

	t.Run("empty object stores NULL", func(t *testing.T) {
		out, err := SanitizePhotoExif([]byte(`{}`))
		if err != nil || out != nil {
			t.Fatalf("%+v, %v", out, err)
		}
	})

	t.Run("absent field stores NULL", func(t *testing.T) {
		out, err := SanitizePhotoExif(nil)
		if err != nil || out != nil {
			t.Fatalf("%+v, %v", out, err)
		}
	})

	t.Run("malformed JSON is a 400 story, not a swallow", func(t *testing.T) {
		if _, err := SanitizePhotoExif([]byte(`{"camera": `)); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("round-trips through jsonb's own codec", func(t *testing.T) {
		raw := []byte(`{"camera":"X100VI","iso":640}`)
		out, err := SanitizePhotoExif(raw)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(out)
		if err != nil || !strings.Contains(string(b), `"iso":640`) {
			t.Fatalf("%s, %v", b, err)
		}
	})
}

// The delete's object-GC decision table (owner call 2026-11-22 — the eager
// best-effort object delete amends §8's keep-until-GC line). PURE: no DB,
// no S3 — the store resolution and the object call ride the integration
// walk (TestAdminLifecycle's GC step).
func TestGCDecide(t *testing.T) {
	tcs := []struct {
		name       string
		mediaWired bool
		path       string
		siblings   int
		want       gcAction
	}{
		{"no store wired — dev/pre-M5 row-only", false, "media/aa/x.jpg", 0, gcRowOnly},
		{"no store wired wins over everything", false, "", 3, gcRowOnly},
		{"unresolved ledger — hand-inserted or pre-ledger row", true, "", 0, gcUnresolved},
		{"sibling photo cites the same object — it stays", true, "media/aa/x.jpg", 1, gcShared},
		{"sole owner — delete", true, "media/aa/x.jpg", 0, gcDelete},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			if got := gcDecide(tc.mediaWired, tc.path, tc.siblings); got != tc.want {
				t.Fatalf("gcDecide(wired=%v, path=%q, sib=%d) = %v, want %v",
					tc.mediaWired, tc.path, tc.siblings, got, tc.want)
			}
		})
	}
}
