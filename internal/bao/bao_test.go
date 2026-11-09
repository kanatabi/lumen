package bao

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// A2 acceptance (PLAN-SECRETS-CONTROL §3): the WIRE SHAPES parse through
// fixture JSON — no server, no network. The Meta op reads exactly this
// kv/metadata GET shape (UseNumber like the SDK's decoder).

func metaFixture(t *testing.T, raw string) map[string]any {
	t.Helper()
	var d map[string]any
	dec := json.NewDecoder(errors_reader(raw))
	dec.UseNumber()
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("fixture decode: %v", err)
	}
	return d
}

// errors_reader is a tiny io.Reader over a string (json.Decoder input).
func errors_reader(s string) *stringReader { return &stringReader{s: s} }

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, errEOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

var errEOF = errors.New("EOF")

func TestDecodeMetaFullShape(t *testing.T) {
	d := metaFixture(t, `{
		"current_version": 4,
		"custom_metadata": {"pinned_version": 2},
		"versions": {
			"1": {"created_time": "2026-11-09T10:00:00.123456Z", "destroyed": false},
			"4": {"created_time": "2026-11-09T13:02:42.322043Z", "destroyed": false},
			"2": {"created_time": "2026-11-09T11:00:00Z", "destroyed": true}
		}
	}`)
	m := decodeMeta(d)
	if m.CurrentVersion != 4 {
		t.Fatalf("CurrentVersion = %d, want 4", m.CurrentVersion)
	}
	if m.PinnedVersion == nil || *m.PinnedVersion != 2 {
		t.Fatalf("PinnedVersion = %v, want 2", m.PinnedVersion)
	}
	want := []VersionInfo{
		{1, "2026-11-09T10:00:00.123456Z", false},
		{2, "2026-11-09T11:00:00Z", true},
		{4, "2026-11-09T13:02:42.322043Z", false},
	}
	if !reflect.DeepEqual(m.Versions, want) {
		t.Fatalf("Versions = %+v, want %+v (sorted ascending)", m.Versions, want)
	}
}

func TestDecodeMetaPinAbsenceIsExplicitNil(t *testing.T) {
	// unpinned MUST serialize as null, never 0 — mellon's badge renders
	// from nil-vs-set, a fake pin of 0 would be a lie
	cases := map[string]string{
		"no custom_metadata":              `{"current_version": 1, "versions": {}}`,
		"empty custom":                    `{"current_version": 1, "versions": {}, "custom_metadata": {}}`,
		"null pin (unpinned via RFC7396)": `{"current_version": 1, "versions": {}, "custom_metadata": {"pinned_version": null}}`,
		"foreign junk pin":                `{"current_version": 1, "versions": {}, "custom_metadata": {"pinned_version": "two"}}`,
		"zero pin (illegal)":              `{"current_version": 1, "versions": {}, "custom_metadata": {"pinned_version": 0}}`,
		"negative pin (illegal)":          `{"current_version": 1, "versions": {}, "custom_metadata": {"pinned_version": -3}}`,
		"custom_metadata wrong type":      `{"current_version": 1, "versions": {}, "custom_metadata": "oops"}`,
	}
	for name, raw := range cases {
		if m := decodeMeta(metaFixture(t, raw)); m.PinnedVersion != nil {
			t.Errorf("%s: PinnedVersion = %v, want nil", name, *m.PinnedVersion)
		}
	}
	// and a real pin round-trips through the JSON tag name
	m := decodeMeta(metaFixture(t, `{"current_version":3,"versions":{},"custom_metadata":{"pinned_version":3}}`))
	if m.PinnedVersion == nil || *m.PinnedVersion != 3 {
		t.Fatalf("valid pin lost: %v", m.PinnedVersion)
	}
}

func TestDecodeMetaEmptyVersionsIsNotNullSlice(t *testing.T) {
	// JSON contract: "versions":[] not null — mellon maps straight to a list
	m := decodeMeta(metaFixture(t, `{"current_version": 1}`))
	if m.Versions == nil {
		t.Fatal("Versions must marshal as [], not null")
	}
}

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		msg  string
		kind ErrorKind
	}{
		{"error performing token check: permission denied for kv/metadata/lumen/dev", ErrPermission},
		{"check-and-set required param `version` was provided with value `2` and current version was `3`", ErrCheckAndSet},
		{"Error making API request. Cas mismatch", ErrCheckAndSet},
	}
	for _, c := range cases {
		err := classifyErr(errors.New(c.msg))
		var be *Error
		if !errors.As(err, &be) || be.Kind != c.kind {
			t.Errorf("classifyErr(%q) = %v, want kind %d", c.msg, err, c.kind)
		}
	}
	// untouched errors pass through as-is (transport blips are not kinds)
	plain := errors.New("dial tcp: connection refused")
	if got := classifyErr(plain); got != plain {
		t.Errorf("classifyErr passthrough broken: %v", got)
	}
}

func TestRemapExists(t *testing.T) {
	cas := &Error{Kind: ErrCheckAndSet, Err: errors.New("cas mismatch")}
	got := remapExists(cas)
	var be *Error
	if !errors.As(got, &be) || be.Kind != ErrAlreadyExists {
		t.Fatalf("cas-failure on a create must re-story as ErrAlreadyExists, got %v", got)
	}
	if remapExists(nil) != nil {
		t.Fatal("nil stays nil")
	}
	perm := &Error{Kind: ErrPermission, Err: errors.New("permission denied")}
	if r := remapExists(perm); r != perm {
		t.Fatalf("permission failure is NOT existence, got %v", r)
	}
}
