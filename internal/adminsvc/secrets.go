package adminsvc

// secrets.go — the SECRETS PLANE service (§9 + PLAN-SECRETS-CONTROL A3).
// The handlers decode; everything with a decision lives here: onboarding,
// the key-set mirror, pins. Bao is behind the narrow baoAPI seam (the
// concrete client satisfies it; unit tests fake it — no server needed to
// prove the mirror). Doctrine unchanged: values ride responses, logs carry
// op + app + env + counts, pins are intent (D1) that never routes a read.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/kanatabi/lumen/internal/bao"
)

// baoAPI is the ENTIRE face the service uses from the relay (minimal-surface
// doctrine applies inside the house too — a fake with only these six is an
// honest contract mirror).
type baoAPI interface {
	ListPaths(ctx context.Context) ([]bao.Path, error)
	Read(ctx context.Context, app, env string, version int) (*bao.Payload, error)
	Meta(ctx context.Context, app, env string) (*bao.Meta, error)
	WriteCAS(ctx context.Context, app, env string, data map[string]string, cas int) (int, error)
	CreateIfAbsent(ctx context.Context, app, env string, data map[string]string) error
	PatchMetadataCustom(ctx context.Context, app, env string, custom map[string]any) error
}

// Secrets sentinels (the bao kinds translate HERE; handlers name only ours).
var (
	ErrInvalidSlug    = errors.New("app/env fails slug shape validation")
	ErrAppExists      = errors.New("app namespace already exists — onboarding never overwrites")
	ErrAppUnknown     = errors.New("app has no envs — no key set to derive from")
	ErrEnvExists      = errors.New("env already exists for this app")
	ErrSecretNotFound = errors.New("secret path not found")
	ErrVersionUnknown = errors.New("pin version is not a live version of this secret")
	// ErrMirrorStale wraps WITH the env name (%w: env): the head write
	// landed, one sibling refused the mirror — retry-after-reload is the
	// story, and the next save re-reconciles (§7 partial-failure honesty).
	ErrMirrorStale = errors.New("key-set mirror stale")
)

// ErrPinned is the WRITE LOCK (config-gate contract): while a path carries a
// pin, the save path refuses CAS writes — a pin freezes content until an
// explicit unpin, which is what makes "pin is a lock" more than a slogan.
// The message is WIRE CONTRACT COPY: the relay answers 403 with it verbatim
// ({"error":"pinned at v<N> — unpin first"}), so mellon and CI match on it.
type ErrPinned struct{ Version int }

func (e ErrPinned) Error() string { return fmt.Sprintf("pinned at v%d — unpin first", e.Version) }

// ---- passthrough relay surface (existing handlers depend on these) ----

func (s *Service) SecretPaths(ctx context.Context) ([]bao.Path, error) {
	return s.bao.ListPaths(ctx)
}

func (s *Service) ReadSecret(ctx context.Context, app, env string, version int) (*bao.Payload, error) {
	return s.bao.Read(ctx, app, env, version)
}

func (s *Service) SecretMeta(ctx context.Context, app, env string) (*bao.Meta, error) {
	return s.bao.Meta(ctx, app, env)
}

// ---- control-plane decisions ----

// OnboardApp creates the namespace: D4's trio (or an explicit env list),
// each as an EMPTY bundle. It never overwrites — an app with even one
// existing env is refused — and the create itself is cas=0, so a racing
// duplicate still lands as ErrAppExists, not a silent clobber.
func (s *Service) OnboardApp(ctx context.Context, app string, envs []string) error {
	if !ValidAppSlug(app) {
		return ErrInvalidSlug
	}
	if len(envs) == 0 {
		envs = DefaultEnvs
	}
	uniq := make([]string, 0, len(envs))
	seen := map[string]bool{}
	for _, e := range envs {
		if !ValidEnvSlug(e) {
			return ErrInvalidSlug
		}
		if !seen[e] {
			seen[e] = true
			uniq = append(uniq, e)
		}
	}
	paths, err := s.bao.ListPaths(ctx)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if p.App == app {
			return ErrAppExists
		}
	}
	for _, e := range uniq {
		if err := s.bao.CreateIfAbsent(ctx, app, e, map[string]string{}); err != nil {
			var be *bao.Error
			if errors.As(err, &be) && be.Kind == bao.ErrAlreadyExists {
				return ErrAppExists
			}
			return err
		}
	}
	return nil
}

// AddEnv extends an EXISTING app (unknown apps are typos, not intent) with
// one env, seeded to the app's canonical key set (union of live siblings,
// empty values) — a late env must never drift a smaller shape than its
// siblings; the invariant is structural, not merely write-time.
func (s *Service) AddEnv(ctx context.Context, app, env string) error {
	if !ValidAppSlug(app) || !ValidEnvSlug(env) {
		return ErrInvalidSlug
	}
	paths, err := s.bao.ListPaths(ctx)
	if err != nil {
		return err
	}
	var siblings []bao.Path
	for _, p := range paths {
		if p.App == app {
			if p.Env == env {
				return ErrEnvExists
			}
			siblings = append(siblings, p)
		}
	}
	if len(siblings) == 0 {
		return ErrAppUnknown
	}
	sets := make(map[string]map[string]string, len(siblings))
	for _, p := range siblings {
		head, err := s.bao.Read(ctx, app, p.Env, 0)
		if err != nil {
			return err // a unreadable sibling shrinks the union — refuse loudly instead
		}
		if head != nil {
			sets[p.Env] = head.Data
		}
	}
	target := make(map[string]string)
	for _, k := range CanonicalKeySet(sets) {
		target[k] = ""
	}
	if err := s.bao.CreateIfAbsent(ctx, app, env, target); err != nil {
		var be *bao.Error
		if errors.As(err, &be) && be.Kind == bao.ErrAlreadyExists {
			return ErrEnvExists
		}
		return err
	}
	return nil
}

// WriteSecret writes the env's bundle (CAS as before) and then runs the
// KEY-SET MIRROR. The plan sketched a diff against the env's previous head;
// the implementation RECONCILES each sibling's live shape toward the saved
// shape instead — strictly better where it matters: a mirror interrupted
// (sibling CAS stale, plane blip) is repaired by the NEXT write, because
// the target is the current saved set, not a delta that would recompute as
// empty. Value-only edits produce zero shape diffs, so siblings see no
// writes and no version bumps at all.
//
// WRITE LOCK (config-gate contract): a path carrying a pin refuses saves
// BEFORE anything is written — the pin's freeze is enforced by the only
// writer in the house, not by convention. The lock guards the SAVE path
// the operator targets; the mirror deliberately still reconciles a PINNED
// sibling: the key-set invariant is structural, and silently skipping a
// sibling would shrink the estate's shape honesty for an intent that lives
// one env over. The resulting pin≠head surfaces at the next check-epoch as
// cause 2 — with its own remedy.
func (s *Service) WriteSecret(ctx context.Context, app, env string, data map[string]string, cas int) (int, error) {
	meta, err := s.bao.Meta(ctx, app, env)
	if err != nil {
		return 0, err
	}
	if meta != nil && meta.PinnedVersion != nil {
		return 0, ErrPinned{Version: *meta.PinnedVersion}
	}
	version, err := s.bao.WriteCAS(ctx, app, env, data, cas)
	if err != nil {
		var be *bao.Error
		if errors.As(err, &be) && be.Kind == bao.ErrCheckAndSet {
			return 0, ErrStaleCAS
		}
		return 0, err
	}
	if err := s.mirrorKeySet(ctx, app, env, data); err != nil {
		return version, err
	}
	return version, nil
}

func (s *Service) mirrorKeySet(ctx context.Context, app, env string, saved map[string]string) error {
	paths, err := s.bao.ListPaths(ctx)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if p.App != app || p.Env == env {
			continue
		}
		head, err := s.bao.Read(ctx, app, p.Env, 0)
		if err != nil {
			return err
		}
		current := map[string]string{}
		headVersion := 0
		if head != nil {
			current = head.Data
			headVersion = head.Version
		}
		adds, removes := KeyDelta(current, saved)
		next, changed := ApplyDelta(current, adds, removes)
		if !changed {
			continue // shape already agrees — no write, no version bump
		}
		if _, err := s.bao.WriteCAS(ctx, app, p.Env, next, headVersion); err != nil {
			var be *bao.Error
			if errors.As(err, &be) && be.Kind == bao.ErrCheckAndSet {
				return fmt.Errorf("%w: %s", ErrMirrorStale, p.Env)
			}
			return err
		}
	}
	return nil
}

// SecretSummary is one cell of the estate matrix (C1): per app/env the
// HEAD version, the pin intent, and key COUNTS — empty-key counts are the
// "onboarded but unfilled" signal; values never leave the relay, the matrix
// is all arithmetic over truth. Errors on one path abort the sweep honestly
// (a half-truth matrix is worse than a visible 5xx).
type SecretSummary struct {
	App           string `json:"app"`
	Env           string `json:"env"`
	Version       int    `json:"version"`
	PinnedVersion *int   `json:"pinnedVersion"`
	Keys          int    `json:"keys"`
	EmptyKeys     int    `json:"emptyKeys"`
}

func (s *Service) EstateSummary(ctx context.Context) ([]SecretSummary, error) {
	paths, err := s.bao.ListPaths(ctx)
	if err != nil {
		return nil, err
	}
	out := []SecretSummary{}
	for _, path := range paths {
		meta, err := s.bao.Meta(ctx, path.App, path.Env)
		if err != nil {
			return nil, err
		}
		row := SecretSummary{App: path.App, Env: path.Env}
		if meta != nil {
			row.Version = meta.CurrentVersion
			row.PinnedVersion = meta.PinnedVersion
		}
		payload, err := s.bao.Read(ctx, path.App, path.Env, 0)
		if err != nil {
			return nil, err
		}
		if payload != nil {
			row.Keys = len(payload.Data)
			for _, v := range payload.Data {
				if v == "" {
					row.EmptyKeys++
				}
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// Pin records intent (D1): the version must be a LIVE, non-destroyed
// version of THIS path. Consumers keep reading head — nothing routes here.
func (s *Service) Pin(ctx context.Context, app, env string, version int) error {
	meta, err := s.bao.Meta(ctx, app, env)
	if err != nil {
		return err
	}
	if meta == nil {
		return ErrSecretNotFound
	}
	live := false
	for _, v := range meta.Versions {
		if v.Version == version && !v.Destroyed {
			live = true
			break
		}
	}
	if version < 1 || !live {
		return ErrVersionUnknown
	}
	// KV-v2 custom_metadata is a map[string]string — the ENGINE enforces it
	// (a numeric value answers 500, discovered by the A5 integration leg):
	// the pin travels as its decimal STRING.
	return s.bao.PatchMetadataCustom(ctx, app, env, map[string]any{"pinned_version": strconv.Itoa(version)})
}

// Unpin removes the intent (RFC 7396: null deletes the key — no sentinel
// numbers in metadata).
func (s *Service) Unpin(ctx context.Context, app, env string) error {
	meta, err := s.bao.Meta(ctx, app, env)
	if err != nil {
		return err
	}
	if meta == nil {
		return ErrSecretNotFound
	}
	return s.bao.PatchMetadataCustom(ctx, app, env, map[string]any{"pinned_version": nil})
}
