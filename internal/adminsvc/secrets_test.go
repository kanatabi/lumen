package adminsvc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kanatabi/lumen/internal/bao"
)

// A3 unit proof (PLAN-SECRETS-CONTROL §3): every control-plane decision —
// onboarding refusals, union seeding, the key-set mirror and its
// self-healing, stale-sibling stories, pin validation — against the
// in-memory fake behind the baoAPI seam. No server, no wire.

type fakeVersion struct {
	data      map[string]string
	destroyed bool
}

type fakePath struct {
	versions []fakeVersion
	custom   map[string]any
}

type fakeBao struct {
	paths     map[string]*fakePath
	failCAS   map[string]bool // app/env whose next CAS write mismatches
	patchPerm bool            // simulate the missing `patch` capability
	writes    int             // every data write counted — the NO-version-bump promises assert on this
}

func newFake() *fakeBao {
	return &fakeBao{paths: map[string]*fakePath{}, failCAS: map[string]bool{}}
}

func key(app, env string) string { return app + "/" + env }

func (f *fakeBao) seed(app, env string, data map[string]string) {
	f.paths[key(app, env)] = &fakePath{versions: []fakeVersion{{data: data}}}
}

func (f *fakeBao) ListPaths(_ context.Context) ([]bao.Path, error) {
	var out []bao.Path
	for k := range f.paths {
		app, env, _ := strings.Cut(k, "/")
		out = append(out, bao.Path{App: app, Env: env})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Env < out[j].Env
	})
	return out, nil
}

func (f *fakeBao) Read(_ context.Context, app, env string, version int) (*bao.Payload, error) {
	p := f.paths[key(app, env)]
	if p == nil || len(p.versions) == 0 {
		return nil, nil
	}
	idx := len(p.versions) - 1
	if version > 0 {
		idx = version - 1
		if idx < 0 || idx >= len(p.versions) {
			return nil, nil
		}
	}
	v := p.versions[idx]
	data := make(map[string]string, len(v.data))
	for k, val := range v.data {
		data[k] = val
	}
	return &bao.Payload{Data: data, Version: idx + 1}, nil
}

func (f *fakeBao) Meta(_ context.Context, app, env string) (*bao.Meta, error) {
	p := f.paths[key(app, env)]
	if p == nil {
		return nil, nil
	}
	m := &bao.Meta{CurrentVersion: len(p.versions)}
	for i, v := range p.versions {
		m.Versions = append(m.Versions, bao.VersionInfo{Version: i + 1, Destroyed: v.destroyed})
	}
	if n, ok := p.custom["pinned_version"].(json.Number); ok {
		if i, _ := n.Int64(); i > 0 {
			pi := int(i)
			m.PinnedVersion = &pi
		}
	}
	if m.Versions == nil {
		m.Versions = []bao.VersionInfo{}
	}
	return m, nil
}

func (f *fakeBao) WriteCAS(_ context.Context, app, env string, data map[string]string, cas int) (int, error) {
	f.writes++
	k := key(app, env)
	p := f.paths[k]
	if p == nil {
		p = &fakePath{}
		f.paths[k] = p
	}
	if f.failCAS[k] {
		f.failCAS[k] = false
		return 0, &bao.Error{Kind: bao.ErrCheckAndSet, Err: errors.New("cas mismatch")}
	}
	if cas != len(p.versions) {
		return 0, &bao.Error{Kind: bao.ErrCheckAndSet, Err: errors.New("cas mismatch")}
	}
	cp := make(map[string]string, len(data))
	for kk, v := range data {
		cp[kk] = v
	}
	p.versions = append(p.versions, fakeVersion{data: cp})
	return len(p.versions), nil
}

func (f *fakeBao) CreateIfAbsent(ctx context.Context, app, env string, data map[string]string) error {
	k := key(app, env)
	if p := f.paths[k]; p != nil && len(p.versions) > 0 {
		return &bao.Error{Kind: bao.ErrAlreadyExists, Err: errors.New("path exists")}
	}
	_, err := f.WriteCAS(ctx, app, env, data, 0)
	return err
}

func (f *fakeBao) PatchMetadataCustom(_ context.Context, app, env string, custom map[string]any) error {
	k := key(app, env)
	p := f.paths[k]
	if p == nil || f.patchPerm {
		if f.patchPerm {
			return &bao.Error{Kind: bao.ErrPermission, Err: errors.New("permission denied for kv/metadata")}
		}
		return errors.New("path not found")
	}
	if p.custom == nil {
		p.custom = map[string]any{}
	}
	for kk, v := range custom {
		if v == nil {
			delete(p.custom, kk) // RFC 7396
			continue
		}
		p.custom[kk] = json.Number(strconv.Itoa(v.(int)))
	}
	return nil
}

func svc(f *fakeBao) *Service { s := &Service{}; s.bao = f; return s }

// ---- tests ----

func TestOnboardAppSeedsTheTrio(t *testing.T) {
	f := newFake()
	if err := svc(f).OnboardApp(context.Background(), "credit", nil); err != nil {
		t.Fatal(err)
	}
	for _, env := range DefaultEnvs {
		p := f.paths[key("credit", env)]
		if p == nil || len(p.versions) != 1 || len(p.versions[0].data) != 0 {
			t.Fatalf("credit/%s not seeded as an empty v1 bundle: %+v", env, p)
		}
	}
}

func TestOnboardRefusals(t *testing.T) {
	f := newFake()
	f.seed("lumen", "dev", map[string]string{"A": "1"})
	s := svc(f)
	if err := s.OnboardApp(context.Background(), "lumen", nil); !errors.Is(err, ErrAppExists) {
		t.Errorf("partial app must refuse: %v", err)
	}
	for _, bad := range []string{"Lumen", "_pins", "a", "x/y", ""} {
		if err := s.OnboardApp(context.Background(), bad, nil); !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("slug %q: want ErrInvalidSlug, got %v", bad, err)
		}
	}
	if err := s.OnboardApp(context.Background(), "ok-app", []string{"Dev"}); !errors.Is(err, ErrInvalidSlug) {
		t.Errorf("uppercase env must fail: %v", err)
	}
	// explicit env list + dedupe honored
	if err := s.OnboardApp(context.Background(), "ops", []string{"dev", "dev", "lab"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.paths[key("ops", "lab")]; !ok {
		t.Error("explicit env missing")
	}
	writes := f.writes
	if err := s.OnboardApp(context.Background(), "ops", nil); !errors.Is(err, ErrAppExists) {
		t.Errorf("rerun must refuse, got %v", err)
	}
	if f.writes != writes {
		t.Error("refused onboarding must write nothing")
	}
}

func TestAddEnvSeedsCanonicalUnion(t *testing.T) {
	f := newFake()
	f.seed("app", "dev", map[string]string{"A": "1", "B": "2"})
	f.seed("app", "staging", map[string]string{"B": "3", "C": "4"})
	s := svc(f)
	if err := s.AddEnv(context.Background(), "app", "lab"); err != nil {
		t.Fatal(err)
	}
	p := f.paths[key("app", "lab")]
	want := map[string]string{"A": "", "B": "", "C": ""}
	if len(p.versions) != 1 || !reflect.DeepEqual(p.versions[0].data, want) {
		t.Fatalf("lab seed = %+v, want canonical union %v", p.versions, want)
	}
	if err := s.AddEnv(context.Background(), "app", "lab"); !errors.Is(err, ErrEnvExists) {
		t.Errorf("duplicate env: %v", err)
	}
	if err := s.AddEnv(context.Background(), "ghost", "dev"); !errors.Is(err, ErrAppUnknown) {
		t.Errorf("unknown app: %v", err)
	}
}

func TestWriteSecretMirror(t *testing.T) {
	f := newFake()
	f.seed("app", "dev", map[string]string{"K1": "d", "K2": "x"})
	f.seed("app", "staging", map[string]string{"K1": "s", "K2": "y"})
	f.seed("app", "production", map[string]string{"K1": "p", "K2": "z"})
	s := svc(f)

	// value-only save: siblings see NOTHING (no writes, no bumps)
	headStagingBefore := len(f.paths[key("app", "staging")].versions)
	if _, err := s.WriteSecret(context.Background(), "app", "dev", map[string]string{"K1": "d2", "K2": "x"}, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.paths[key("app", "staging")].versions) != headStagingBefore {
		t.Fatal("value-only edit must not touch siblings")
	}

	// structural add: every sibling gains the EMPTY key, values intact
	if _, err := s.WriteSecret(context.Background(), "app", "dev", map[string]string{"K1": "d2", "K2": "x", "NEW": "n"}, 2); err != nil {
		t.Fatal(err)
	}
	for env, oldVal := range map[string]string{"staging": "y", "production": "z"} {
		head := f.paths[key("app", env)].versions[len(f.paths[key("app", env)].versions)-1].data
		if head["NEW"] != "" {
			t.Fatalf("%s missing mirrored key: %v", env, head)
		}
		if head["K2"] != oldVal {
			t.Fatalf("%s value clobbered: %v", env, head)
		}
	}

	// structural remove: gone from every sibling too
	if _, err := s.WriteSecret(context.Background(), "app", "dev", map[string]string{"K1": "d2", "NEW": "n"}, 3); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"staging", "production"} {
		head := f.paths[key("app", env)].versions[len(f.paths[key("app", env)].versions)-1].data
		if _, ok := head["K2"]; ok {
			t.Fatalf("%s kept removed key: %v", env, head)
		}
	}
}

func TestMirrorSelfHealsInterruptedShape(t *testing.T) {
	// the §7 partial failure, told as data: an old interrupted mirror left
	// staging holding a key the app shape no longer has. ANY later save to
	// any env reconciles it — the oldHead-delta design could not (its delta
	// recomputes empty).
	f := newFake()
	f.seed("app", "dev", map[string]string{"A": "1"})
	f.seed("app", "staging", map[string]string{"A": "2", "GHOST": "leftover"})
	s := svc(f)
	if _, err := s.WriteSecret(context.Background(), "app", "dev", map[string]string{"A": "9"}, 1); err != nil {
		t.Fatal(err)
	}
	head := f.paths[key("app", "staging")].versions[1].data
	if _, ok := head["GHOST"]; ok {
		t.Fatalf("stale sibling not repaired: %v", head)
	}
	if head["A"] != "2" {
		t.Fatalf("sibling value clobbered during repair: %v", head)
	}
}

func TestMirrorStaleSiblingStories(t *testing.T) {
	f := newFake()
	f.seed("app", "dev", map[string]string{"A": "1"})
	f.seed("app", "staging", map[string]string{"A": "2"})
	f.failCAS[key("app", "staging")] = true // sibling raced ahead of our read
	s := svc(f)
	version, err := s.WriteSecret(context.Background(), "app", "dev", map[string]string{"A": "1", "B": "3"}, 1)
	if !errors.Is(err, ErrMirrorStale) {
		t.Fatalf("want wrapped ErrMirrorStale, got %v", err)
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Fatalf("stale story must name the env, got %q", err.Error())
	}
	if version != 2 {
		t.Fatalf("the head write DID land (honest partial): want version 2, got %d", version)
	}
	// and the next save heals: no fail scripted anymore
	if _, err := s.WriteSecret(context.Background(), "app", "dev", map[string]string{"A": "1", "B": "3"}, version); err != nil {
		t.Fatalf("retry must reconcile cleanly: %v", err)
	}
	staging := f.paths[key("app", "staging")].versions
	if _, ok := staging[len(staging)-1].data["B"]; !ok {
		t.Fatal("heal did not reach the sibling")
	}
}

func TestWriteSecretHeadStale(t *testing.T) {
	f := newFake()
	f.seed("app", "dev", map[string]string{"A": "1"})
	if _, err := svc(f).WriteSecret(context.Background(), "app", "dev", map[string]string{"A": "2"}, 99); !errors.Is(err, ErrStaleCAS) {
		t.Fatalf("head CAS must stay OUR sentinel: %v", err)
	}
}

func TestPinValidation(t *testing.T) {
	f := newFake()
	f.seed("app", "dev", map[string]string{"A": "1"})
	f.paths[key("app", "dev")].versions = append(f.paths[key("app", "dev")].versions,
		fakeVersion{data: map[string]string{"A": "2"}, destroyed: true}) // v2 destroyed
	s := svc(f)
	ctx := context.Background()

	if err := s.Pin(ctx, "ghost", "dev", 1); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("missing path: %v", err)
	}
	for _, bad := range []int{0, -1, 9} {
		if err := s.Pin(ctx, "app", "dev", bad); !errors.Is(err, ErrVersionUnknown) {
			t.Errorf("version %d: want ErrVersionUnknown, got %v", bad, err)
		}
	}
	if err := s.Pin(ctx, "app", "dev", 2); !errors.Is(err, ErrVersionUnknown) {
		t.Errorf("destroyed version must be unpinnable: %v", err)
	}
	if err := s.Pin(ctx, "app", "dev", 1); err != nil {
		t.Fatal(err)
	}
	pin := 1
	if m, _ := s.SecretMeta(ctx, "app", "dev"); m.PinnedVersion == nil || *m.PinnedVersion != pin {
		t.Fatalf("pin did not roundtrip through meta: %+v", m.PinnedVersion)
	}
	// D1: the pin never routes reads — head is still v2's content...
	if pl, _ := s.ReadSecret(ctx, "app", "dev", 0); pl.Version != 2 {
		t.Fatalf("read must still serve head: %+v", pl)
	}
	if err := s.Unpin(ctx, "app", "dev"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.SecretMeta(ctx, "app", "dev"); m.PinnedVersion != nil {
		t.Fatal("unpin must clear the intent (RFC 7396 nil-delete)")
	}
	if err := s.Unpin(ctx, "ghost", "dev"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("unpin on missing path: %v", err)
	}
}

func TestPinWithoutPatchCapabilityIsLoud(t *testing.T) {
	// the A5 runbook case: policy missing `patch` must surface as the
	// permission story, never a silent success
	f := newFake()
	f.seed("app", "dev", map[string]string{"A": "1"})
	f.patchPerm = true
	if err := svc(f).Pin(context.Background(), "app", "dev", 1); err == nil {
		t.Fatal("want permission failure")
	} else {
		var be *bao.Error
		if !errors.As(err, &be) || be.Kind != bao.ErrPermission {
			t.Fatalf("want classified ErrPermission, got %v", err)
		}
	}
}
