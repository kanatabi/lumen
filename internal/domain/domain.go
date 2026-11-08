// Package domain is the §1 contract in types and the read semantics in pure
// functions — a direct port of earendil's mock (matches/sorted/score/suggest)
// so the Postgres store and the in-memory twin cannot drift from each other OR
// from the contract. No I/O here; the ported table tests run offline.
package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ---- contract shapes (keys ALWAYS present — FE types are non-optional) ----

type Summary struct {
	Slug      string  `json:"slug"`
	Title     string  `json:"title"`
	Subtitle  string  `json:"subtitle"`
	Preview   string  `json:"preview"`
	Date      string  `json:"date"` // YYYY-MM-DD (ISO 8601, the editorial date)
	ReadTime  int     `json:"readTime"`
	Thumbnail *string `json:"thumbnail"` // null ⇒ listings render text-only
}

type Detail struct {
	Summary
	Tags    []string `json:"tags"`
	Content string   `json:"content"`
}

type Photo struct {
	// ID is hydration glue (engine ids ↔ truth rows), never on the wire;
	// curation fields below are BE-side facts the §1 shape trims at the
	// service edge (hydration zeroes them before encoding).
	ID         int64   `json:"-"`
	Src        string  `json:"src"`
	Alt        string  `json:"alt"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Gallery    bool    `json:"gallery,omitempty"`
	Featured   bool    `json:"featured,omitempty"`
	Sort       int     `json:"sort,omitempty"`
	Collection *string `json:"collection,omitempty"`
}

type Tag struct {
	Name  string `json:"name"`
	Posts int    `json:"posts"`
}

type ListResult struct {
	Total int       `json:"total"`
	Items []Summary `json:"items"`
}

type PhotoList struct {
	Total int     `json:"total"`
	Items []Photo `json:"items"`
}

type SuggestResult struct {
	Results []Summary `json:"results"`
}

// ---- query parsing (§1 semantics, validated at the edge) ----

type ListQuery struct {
	Offset  int
	Limit   int
	Tags    []string // OR-matched (ANY)
	From    string   // inclusive YYYY-MM-DD on date
	To      string
	ReadMin *int
	ReadMax *int
	Sort    string // whitelisted; "" = curation/relevance order (absent = BE curation)
	Q       string // search/suggest needle
}

const (
	// Absent limit serves ALL (mock parity for the contract); explicit
	// limits are capped so no request unbounds a page on the 2 GB node.
	MaxListLimit      = 100
	DefaultSuggestLim = 5 // earendil client default
	SuggestHardCap    = 10
)

var sortWhitelist = map[string]bool{
	"newest": true, "oldest": true, "quickest": true, "longest": true, "relevance": true,
}

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ErrParam surfaces as HTTP 400 at the handler edge.
type ErrParam struct{ Msg string }

func (e ErrParam) Error() string { return e.Msg }

func parseInt(name, raw string, def int) (int, error) {
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil {
		return 0, ErrParam{Msg: name + " must be an integer"}
	}
	if v < 0 {
		return 0, ErrParam{Msg: name + " must be >= 0"}
	}
	return v, nil
}

func parseOptInt(name, raw string) (*int, error) {
	if raw == "" {
		return nil, nil
	}
	v, err := parseInt(name, raw, 0)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Normalize clamps limit/offset. Unknown sort: reject loudly at the edge —
// silently falling back to another order would let a typo masquerade as
// curation (the mock tolerates it; a server shouldn't).
func (q *ListQuery) Normalize() error {
	// Limit semantics (contract-faithful): ABSENT (0) = the full corpus, like
	// the mock's getPosts(); an explicit limit is capped to bound the page.
	if q.Limit > MaxListLimit {
		q.Limit = MaxListLimit
	}
	if q.Sort != "" && !sortWhitelist[q.Sort] {
		return ErrParam{Msg: "sort must be one of newest|oldest|quickest|longest|relevance"}
	}
	for _, d := range []struct{ name, val string }{{"from", q.From}, {"to", q.To}} {
		if d.val != "" && !dateRE.MatchString(d.val) {
			return ErrParam{Msg: d.name + " must be YYYY-MM-DD"}
		}
	}
	return nil
}

// ---- the ported semantics (mirror of earendil mock.ts — keep in sync) ----

// Matches is the shared facet predicate: OR across tags, inclusive date and
// read-time windows. (Port of `matches()`.)
func Matches(d Detail, q ListQuery) bool {
	if len(q.Tags) > 0 {
		hit := false
		for _, t := range d.Tags {
			for _, want := range q.Tags {
				if t == want {
					hit = true
				}
			}
		}
		if !hit {
			return false
		}
	}
	day := d.Date
	if q.From != "" && day < q.From {
		return false
	}
	if q.To != "" && day > q.To {
		return false
	}
	if q.ReadMin != nil && d.ReadTime < *q.ReadMin {
		return false
	}
	if q.ReadMax != nil && d.ReadTime > *q.ReadMax {
		return false
	}
	return true
}

// SortInPlace applies the explicit whitelist over the given order (stable),
// leaving it intact for "" / "relevance" — mirroring the mock's sorted(): the
// caller decides the base order (curation rank, or relevance).
func SortInPlace(items []Summary, sortMode string) {
	switch sortMode {
	case "newest", "oldest":
		desc := sortMode == "newest"
		sort.SliceStable(items, func(i, j int) bool {
			return compareStrings(items[i].Date, items[j].Date, desc) < 0
		})
	case "quickest", "longest":
		desc := sortMode == "longest"
		sort.SliceStable(items, func(i, j int) bool {
			return compareInts(items[i].ReadTime, items[j].ReadTime, desc) < 0
		})
	}
}

func compareStrings(a, b string, desc bool) int {
	if a == b {
		return 0
	}
	if desc { // descending
		if a > b {
			return -1
		}
		return 1
	}
	if a < b {
		return -1
	}
	return 1
}

func compareInts(a, b int, desc bool) int {
	if a == b {
		return 0
	}
	if desc {
		if a > b {
			return -1
		}
		return 1
	}
	if a < b {
		return -1
	}
	return 1
}

// Score is the mock's relevance: occurrences of the (lowercased) needle in
// "title subtitle preview tags" — JS `split(needle).length - 1` ≡ count of
// occurrences.
func Score(d Detail, needle string) int {
	if needle == "" {
		return 0
	}
	hay := strings.ToLower(strings.Join([]string{
		d.Title, d.Subtitle, d.Preview, strings.Join(d.Tags, " "),
	}, " "))
	return strings.Count(hay, needle)
}

// SuggestMatch is the launcher predicate: case-insensitive contains over
// "title preview" ONLY (the mock's narrower field set — suggest ≠ search).
func SuggestMatch(d Detail, needle string) bool {
	hay := strings.ToLower(d.Title + " " + d.Preview)
	return strings.Contains(hay, needle)
}
