package store

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Slugify mirrors mellon's slug.ts EXACTLY (mirror ritual — drift is a bug
// in whichever moved): lowercase, NFKD, combining marks stripped, runs of
// non-alphanumerics → single hyphen, trimmed, 80 cap.
func Slugify(in string) string {
	s := strings.ToLower(norm.NFKD.String(in))
	s = combiningRe.ReplaceAllString(s, "")
	s = nonAlnumRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

var (
	combiningRe = regexp.MustCompile("[\u0300-\u036f]")
	nonAlnumRe  = regexp.MustCompile(`[^a-z0-9]+`)
)
