package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/kanatabi/lumen/internal/store"
)

// TestTagCountsFromSearch — the count pipeline's integration proof (design
// amendment 2026-11-15): the `tags.posts` counter column DIED (0008); the
// published count per tag derives from the engine's facet distribution,
// merged in the service. What must hold, against the REAL engine:
//
//	publish (tagged)     → the desk's row reads 1
//	save with fewer tags → the count follows IMMEDIATELY (the old counter
//	                       lagged exactly here: the update path never
//	                       recomputed — the tag-desk zero of 2026-11-15)
//	unpublish            → back to 0
//	the widget's filter  → a count-0 or unfeatured tag never reaches
//	                       /api/tags (the leak rule + display rule)
func TestTagCountsFromSearch(t *testing.T) {
	h := newAdminHarness(t)
	h.t = t
	h.token = h.login("fzr-test", "integration-pw")

	const slug = "integration-facet-counts"
	const tag = "integration-facet"
	defer func() { _, _ = h.admin(http.MethodDelete, "/admin/posts/"+slug, "") }()

	draft := `{"slug":"` + slug + `","title":"Facet Counts","subtitle":"","preview":"","date":"2026-11-15","tags":["` + tag + `"],"content":"body\n","thumbnail":null}`
	if code, res := h.admin(http.MethodPost, "/admin/posts", draft); code != http.StatusCreated {
		t.Fatalf("create %d: %s", code, res)
	}

	// the desk row BEFORE publish: the junction links a DRAFT — the
	// published count is 0 (a draft must never count; the facet only sees
	// published docs)
	if posts := tagDeskCount(t, h, tag); posts != 0 {
		t.Fatalf("draft must not count, got %d", posts)
	}

	h.mustOK(http.MethodPost, "/admin/posts/"+slug+"/publish", "")
	h.drain()
	if posts := tagDeskCount(t, h, tag); posts != 1 {
		t.Fatalf("after publish want 1, got %d", posts)
	}

	// NOTE the drain before EVERY facet assertion: the count is
	// EVENTUALLY consistent (the index job's queue latency — sub-second,
	// but real); the old counter column was transactional. That is the
	// design amendment's honest trade: correctness by construction, at
	// the queue's latency.

	// THE REGRESSION: a save that CHANGES tags on a published post — the
	// old counter column lagged exactly here (the update path never
	// recomputed; the tag-desk zero of 2026-11-15). The facet derives
	// from the re-enqueued index doc, so the count follows the save.
	// The PUT's rev is the CURRENT rev (optimistic CAS) — read it from
	// the create's response, never hardcode.
	var cur store.AdminPostDetail
	if err := json.Unmarshal(h.mustOK(http.MethodGet, "/admin/posts/"+slug, ""), &cur); err != nil {
		t.Fatalf("detail decode: %v", err)
	}
	upd := `{"draft":{"slug":"` + slug + `","title":"Facet Counts","subtitle":"","preview":"","date":"2026-11-15","tags":[],"content":"body\n","thumbnail":null},"rev":` + strconv.Itoa(cur.Rev) + `}`
	if err := json.Unmarshal(h.mustOK(http.MethodPut, "/admin/posts/"+slug, upd), &cur); err != nil {
		t.Fatalf("put decode: %v", err)
	}
	h.drain()
	if posts := tagDeskCount(t, h, tag); posts != 0 {
		t.Fatalf("after tag-drop save want 0, got %d (the counter-column bug would say 1)", posts)
	}

	// restore the tag on the published post — the count returns WITHOUT a
	// re-publish (the exact flow that was broken tonight)
	upd = `{"draft":{"slug":"` + slug + `","title":"Facet Counts","subtitle":"","preview":"","date":"2026-11-15","tags":["` + tag + `"],"content":"body\n","thumbnail":null},"rev":` + strconv.Itoa(cur.Rev) + `}`
	if err := json.Unmarshal(h.mustOK(http.MethodPut, "/admin/posts/"+slug, upd), &cur); err != nil {
		t.Fatalf("put decode: %v", err)
	}
	h.drain()
	if posts := tagDeskCount(t, h, tag); posts != 1 {
		t.Fatalf("after tag-add save want 1, got %d", posts)
	}

	// the widget's rules: /api/tags serves FEATURED tags only, count > 0 —
	// this tag is not featured, so it must not leak even at count 1
	pub := h.mustOK(http.MethodGet, "/api/tags", "")
	if bytes.Contains(pub, []byte(tag)) {
		t.Fatalf("unfeatured tag leaked to /api/tags: %s", pub)
	}
}

func tagDeskCount(t *testing.T, h *adminHarness, tag string) int {
	t.Helper()
	res := h.mustOK(http.MethodGet, "/admin/tags", "")
	var rows []store.TagRowOut // the desk's list is a BARE array
	if err := json.Unmarshal(res, &rows); err != nil {
		t.Fatalf("desk decode: %v", err)
	}
	for _, row := range rows {
		if row.Name == tag {
			return row.Posts
		}
	}
	return -1 // the tag vanished — a finding, not a count
}
