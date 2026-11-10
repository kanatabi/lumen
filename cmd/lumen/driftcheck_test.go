package main

import (
	"context"
	"testing"

	"github.com/kanatabi/lumen/internal/bao"
)

// fakeMeta serves canned Meta per app/env — the C2 seam: classification must
// be testable without a live plane, and it must never touch values.
type fakeMeta struct {
	metas map[string]*bao.Meta // key "app/env"
	errs  map[string]bool
}

func (f fakeMeta) Meta(_ context.Context, app, env string) (*bao.Meta, error) {
	k := app + "/" + env
	if f.errs[k] {
		return nil, context.DeadlineExceeded
	}
	return f.metas[k], nil // missing => nil,nil (no metadata = unpinned reality)
}

func ptr(v int) *int { return &v }

func TestCollectPinDrift(t *testing.T) {
	intest := []bao.Path{
		{App: "lumen", Env: "staging"},
		{App: "lumen", Env: "production"},
		{App: "lumen", Env: "dev"}, // dev is out of §5.5 scope — never swept
		{App: "earendil", Env: "production"},
	}
	f := fakeMeta{
		metas: map[string]*bao.Meta{
			// held below head: THE signal
			"lumen/staging":       {CurrentVersion: 5, PinnedVersion: ptr(3)},
			"lumen/production":    {CurrentVersion: 5, PinnedVersion: ptr(5)}, // intent == head: aligned
			"earendil/production": {CurrentVersion: 2, PinnedVersion: nil},    // unpinned: not drift
		},
	}
	got, err := collectPinDrift(context.Background(), f, intest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 pin drift, got %+v", got)
	}
	d := got[0]
	if d.App != "lumen" || d.Env != "staging" || d.Head != 5 || d.Pinned != 3 {
		t.Fatalf("wrong report: %+v", d)
	}
}

func TestCollectPinDriftPlaneErrorIsLoud(t *testing.T) {
	f := fakeMeta{
		metas: map[string]*bao.Meta{"lumen/staging": {CurrentVersion: 1}},
		errs:  map[string]bool{"lumen/production": true},
	}
	_, err := collectPinDrift(context.Background(), f, []bao.Path{{App: "lumen", Env: "production"}})
	if err == nil {
		t.Fatal("a plane error must abort the sweep (half-truth report is worse)")
	}
}
