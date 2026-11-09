package adminsvc

import (
	"strings"
	"testing"
)

// The §2 publish derivations, unit-tested WITHOUT a database — the logic
// this exact test could not exist while it lived inside store.PublishAdminPost.

func TestWordCountAndReadTime(t *testing.T) {
	if got := WordCount("one two  three\nfour"); got != 4 {
		t.Fatalf("words = %d", got)
	}
	if got := ReadTime(0); got != 1 {
		t.Fatalf("floor must be 1 minute, got %d", got)
	}
	if got := ReadTime(201); got != 2 { // ~200 wpm, ceiling
		t.Fatalf("201 words = %d minutes", got)
	}
	if got := ReadTime(400); got != 2 {
		t.Fatalf("400 words = %d", got)
	}
	if got := ReadTime(401); got != 3 {
		t.Fatalf("401 words = %d", got)
	}
}

func TestFirstParagraphSkipsStructure(t *testing.T) {
	md := "# Heading\n\n```go\nfunc main() {}\n```\n\nThe real opening paragraph, spoken plainly.\n\n| table |\n|---|\n"
	if got := FirstParagraph(md); got != "The real opening paragraph, spoken plainly." {
		t.Fatalf("picked %q", got)
	}
	// joins lines within one block, collapses to a single display line
	if got := FirstParagraph("line one\nline two"); got != "line one line two" {
		t.Fatalf("join: %q", got)
	}
}

func TestFirstParagraphClampsAtWordBoundary(t *testing.T) {
	long := strings.Repeat("word ", 200) // ~1000 chars
	got := FirstParagraph(long)
	if len(got) > 282 { // 280 + ellipsis bytes accounted below
		if !strings.HasSuffix(got, "…") || len([]rune(got)) > 281 {
			t.Fatalf("clamp failed: len %d", len(got))
		}
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), "wor") {
		t.Fatal("clamped mid-word — display text, not a truncation bug")
	}
}

func TestDerivePreviewExplicitWins(t *testing.T) {
	if got := DerivePreview("authored", "# h\n\ncontent"); got != "authored" {
		t.Fatal("explicit preview must not be overwritten")
	}
	if got := DerivePreview("   ", "content here"); got != "content here" {
		t.Fatalf("blank preview must derive, got %q", got)
	}
}

func TestContentHashStableAndDistinct(t *testing.T) {
	a, b := ContentHash("same"), ContentHash("same")
	if a != b || len(a) != 64 {
		t.Fatal("hash must be deterministic hex")
	}
	if ContentHash("same ") == a {
		t.Fatal("one trailing byte must change the hash (idempotence depends on it)")
	}
}

func TestPublishDerivationWiresTheParts(t *testing.T) {
	content := "Intro sentence here.\n\n# Heading\n\nbody"
	d := PublishDerivation(content, "")
	// words span the WHOLE document (fields, headings included) — the
	// preview may be the first paragraph; the count is not
	if d.WordCount != 6 || d.ReadTime != 1 {
		t.Fatalf("%+v", d)
	}
	if d.Preview != "Intro sentence here." {
		t.Fatalf("preview: %q", d.Preview)
	}
	if d.Hash != ContentHash(content) {
		t.Fatal("derivation must hash the FINAL content")
	}
}
