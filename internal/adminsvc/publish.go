package adminsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/kanatabi/lumen/internal/store"
)

// ---- the PURE heart of the §2 unit of work: no row, no tx, unit-testable ----

func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func WordCount(md string) int { return len(strings.Fields(md)) }

// ReadTime: ~200 wpm, floor 1 — derived at publish from FINAL content.
func ReadTime(words int) int { return max(1, (words+199)/200) }

// FirstParagraph: the authorless preview default (§2) — headings, fences,
// tables, and blockquotes are skipped; clamps at a word boundary because
// preview is display text, not a truncation bug.
func FirstParagraph(md string) string {
	for _, block := range strings.Split(md, "\n\n") {
		b := strings.TrimSpace(block)
		if b == "" || strings.HasPrefix(b, "#") || strings.HasPrefix(b, "```") ||
			strings.HasPrefix(b, "|") || strings.HasPrefix(b, ">") {
			continue
		}
		b = strings.ReplaceAll(b, "\n", " ")
		if len(b) > 280 {
			b = b[:280]
			if i := strings.LastIndexByte(b, ' '); i > 200 {
				b = b[:i]
			}
			b += "…"
		}
		return b
	}
	return ""
}

// DerivePreview: explicit preview wins; empty means derive from content.
func DerivePreview(preview, content string) string {
	if strings.TrimSpace(preview) == "" {
		return FirstParagraph(content)
	}
	return preview
}

// PublishDerivation computes every stored-field consequence of a publish.
func PublishDerivation(content, preview string) store.PublishDerivation {
	words := WordCount(content)
	return store.PublishDerivation{
		WordCount: words,
		ReadTime:  ReadTime(words),
		Preview:   DerivePreview(preview, content),
		Hash:      ContentHash(content),
	}
}

// ---- the service orchestration ----

// Publish: read the row facts, decide (pure), derive (pure), persist (store
// transaction with the index job inside). Idempotence and the state machine
// read the decision, not the SQL.
func (s *Service) Publish(ctx context.Context, slug string) (store.AdminPostDetail, error) {
	src, err := s.store.PublishSource(ctx, slug)
	if err != nil {
		return store.AdminPostDetail{}, err
	}
	if src.Status == "published" && src.StoredHash != "" &&
		src.StoredHash == ContentHash(src.Content) {
		// byte-identical re-publish: NO-OP (rank unmoved — deterministic
		// caches depend on it); the decision was made right here, in logic,
		// not in SQL
		return s.store.GetAdminPost(ctx, slug)
	}
	// machine: draft→published and unpublished→published are the legal
	// edges; published→CHANGED content is the §2 re-publish
	deriv := PublishDerivation(src.Content, src.Preview)
	return s.store.ApplyPublish(ctx, slug, src, deriv, s.queue)
}
