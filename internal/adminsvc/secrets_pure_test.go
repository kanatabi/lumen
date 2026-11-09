package adminsvc

import (
	"reflect"
	"testing"
)

// A1 acceptance (PLAN-SECRETS-CONTROL §3): the key-set invariant, proven
// offline. No network, no Bao, no DB — pure data, table-driven.

func TestValidAppSlug(t *testing.T) {
	cases := []struct {
		slug string
		ok   bool
	}{
		{"lumen", true}, {"e2e-test-app", true}, {"mellon", true},
		{"a1", true}, // min length 2
		{"abcdefghijklmnopqrstuvwxyz012345", true}, // exactly 32
		{"a", false}, // too short
		{"abcdefghijklmnopqrstuvwxyz0123456", false}, // 33 — too long
		{"Lumen", false},  // uppercase
		{"1app", false},   // digit lead
		{"-app", false},   // dash lead
		{"app_x", false},  // underscore banned outright
		{"_pins", false},  // reserved root is also an illegal shape
		{"app/x", false},  // path trickery
		{"app..x", false}, // dots out of shape
		{"", false},       // empty
	}
	for _, c := range cases {
		if got := ValidAppSlug(c.slug); got != c.ok {
			t.Errorf("ValidAppSlug(%q) = %v, want %v", c.slug, got, c.ok)
		}
	}
}

func TestValidEnvSlug(t *testing.T) {
	for _, ok := range []string{"dev", "staging", "production", "lab", "x"} {
		if !ValidEnvSlug(ok) {
			t.Errorf("ValidEnvSlug(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"Dev", "_x", "1x", "", "eu west-1", "x/y"} {
		if ValidEnvSlug(bad) {
			t.Errorf("ValidEnvSlug(%q) = true, want false", bad)
		}
	}
}

func TestDefaultEnvsAreTheTrio(t *testing.T) {
	want := []string{"dev", "staging", "production"}
	if !reflect.DeepEqual(DefaultEnvs, want) {
		t.Fatalf("DefaultEnvs = %v, want %v (order is part of the UX contract)", DefaultEnvs, want)
	}
}

func TestKeyDelta(t *testing.T) {
	cases := []struct {
		name          string
		old, saved    map[string]string
		adds, removes []string
	}{
		{"no change", map[string]string{"A": "1", "B": "2"}, map[string]string{"A": "9", "B": "2"}, nil, nil},
		{"add onto empty env head", map[string]string{}, map[string]string{"K": ""}, []string{"K"}, nil},
		{"remove every key (all-out)", map[string]string{"A": "1", "B": "2"}, map[string]string{}, []string{}, []string{"A", "B"}},
		{"mixed add and remove", map[string]string{"OLD": "x", "KEEP": "y"}, map[string]string{"KEEP": "y", "NEW": "z"}, []string{"NEW"}, []string{"OLD"}},
		{"value-only edit is shape-stable", map[string]string{"T": "old"}, map[string]string{"T": "new"}, nil, nil},
	}
	for _, c := range cases {
		adds, removes := KeyDelta(c.old, c.saved)
		if !reflect.DeepEqual(adds, c.adds) && !(len(adds) == 0 && len(c.adds) == 0) {
			t.Errorf("%s: adds = %v, want %v", c.name, adds, c.adds)
		}
		if !reflect.DeepEqual(removes, c.removes) && !(len(removes) == 0 && len(c.removes) == 0) {
			t.Errorf("%s: removes = %v, want %v", c.name, removes, c.removes)
		}
	}
}

func TestApplyDelta(t *testing.T) {
	t.Run("add lands empty, existing values survive", func(t *testing.T) {
		next, changed := ApplyDelta(map[string]string{"DB_URL": "postgres://x", "K": "keep"}, []string{"NEW"}, nil)
		if !changed {
			t.Fatal("changed = false, want true")
		}
		want := map[string]string{"DB_URL": "postgres://x", "K": "keep", "NEW": ""}
		if !reflect.DeepEqual(next, want) {
			t.Fatalf("next = %v, want %v", next, want)
		}
	})
	t.Run("remove drops; foreign remove is NOT a change", func(t *testing.T) {
		if _, changed := ApplyDelta(map[string]string{"A": "1"}, nil, []string{"A"}); !changed {
			t.Error("dropping a present key must report changed")
		}
		next, changed := ApplyDelta(map[string]string{"A": "1"}, nil, []string{"GHOST"})
		if changed {
			t.Error("removing a key the sibling never had must NOT write (no version bump, no fake CAS)")
		}
		if !reflect.DeepEqual(next, map[string]string{"A": "1"}) {
			t.Errorf("next = %v, want untouched", next)
		}
	})
	t.Run("add-after-remove race: sibling already re-added — value survives, write skipped", func(t *testing.T) {
		// The §7 two-tabs hazard as pure data: tab1 saves the add, tab2's
		// mirror reaches a sibling that already carries the key WITH a value.
		next, changed := ApplyDelta(map[string]string{"SHARED": "value-tab2-set"}, []string{"SHARED"}, nil)
		if changed {
			t.Fatal("changed = true: the race would clobber tab2's value")
		}
		if next["SHARED"] != "value-tab2-set" {
			t.Fatalf("value was clobbered: %q", next["SHARED"])
		}
	})
	t.Run("mixed: one side no-op, other real ⇒ changed with exact result", func(t *testing.T) {
		next, changed := ApplyDelta(map[string]string{"OLD": "1", "GONE": "2"}, []string{"OLD"}, []string{"GONE"})
		if !changed {
			t.Fatal("GONE was present — must report changed")
		}
		if !reflect.DeepEqual(next, map[string]string{"OLD": "1"}) { // OLD already there (kept), GONE dropped, no empty re-add
			t.Fatalf("next = %v, want {OLD:1}", next)
		}
	})
	t.Run("input map is never mutated", func(t *testing.T) {
		src := map[string]string{"A": "1"}
		next, _ := ApplyDelta(src, []string{"B"}, nil)
		if _, ok := src["B"]; ok || len(next) != 2 {
			t.Fatalf("mutation leak: src=%v next=%v", src, next)
		}
	})
}

func TestKeySetAndCanonicalUnion(t *testing.T) {
	if got := KeySet(map[string]string{"B": "", "A": "", "C": ""}); !reflect.DeepEqual(got, []string{"A", "B", "C"}) {
		t.Fatalf("KeySet unsorted: %v", got)
	}
	if got := KeySet(map[string]string{}); len(got) != 0 {
		t.Fatalf("KeySet empty: %v", got)
	}
	envs := map[string]map[string]string{
		"dev":        {"DB_URL": "x", "SHARED": "d"},
		"staging":    {"SHARED": "s", "LATE_ONLY_ON_STAGING": "y"},
		"production": {},
	}
	want := []string{"DB_URL", "LATE_ONLY_ON_STAGING", "SHARED"}
	if got := CanonicalKeySet(envs); !reflect.DeepEqual(got, want) {
		t.Fatalf("CanonicalKeySet = %v, want %v", got, want)
	}
	// determinism: map iteration must not leak into the result order
	for range 20 {
		if got := CanonicalKeySet(envs); !reflect.DeepEqual(got, want) {
			t.Fatalf("non-deterministic union: %v", got)
		}
	}
	if got := CanonicalKeySet(map[string]map[string]string{}); len(got) != 0 {
		t.Fatalf("empty app union = %v, want empty", got)
	}
}
