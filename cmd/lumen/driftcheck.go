package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/kanatabi/lumen/internal/bao"
)

// driftcheckCommand is §5.5's CI drift verb (moved from cmd/driftcheck):
// staging vs production KEY SETS per app, reported as names and counts
// only — values never print; a drift tool that leaks secrets to CI logs is
// not a drift tool. Exit 1 on drift, 2 on plane errors.
func driftcheckCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "driftcheck",
		Short: "compare staging vs production secret key sets (names only, never values)",
		RunE: func(c *cobra.Command, _ []string) error {
			baoClient, err := bao.New()
			if err != nil {
				fmt.Fprintln(os.Stderr, "driftcheck:", err)
				os.Exit(2)
			}
			ctx := context.Background()
			paths, err := baoClient.ListPaths(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "driftcheck: plane unreachable:", err)
				os.Exit(2)
			}
			byApp := map[string]map[string]bool{}
			for _, path := range paths {
				if path.Env != "staging" && path.Env != "production" {
					continue
				}
				if byApp[path.App] == nil {
					byApp[path.App] = map[string]bool{}
				}
				byApp[path.App][path.Env] = true
			}
			drift := false
			for app, envs := range byApp {
				if !envs["staging"] || !envs["production"] {
					continue // one-env presence is bootstrap reality, not drift
				}
				staging, err := baoClient.Read(ctx, app, "staging", 0)
				if err != nil || staging == nil {
					continue
				}
				production, err := baoClient.Read(ctx, app, "production", 0)
				if err != nil || production == nil {
					continue
				}
				missingInProd, missingInStaging := diffKeyNames(staging.Data, production.Data)
				if len(missingInProd) == 0 && len(missingInStaging) == 0 {
					fmt.Printf("%s: key sets aligned (%d)\n", app, len(staging.Data))
					continue
				}
				drift = true
				fmt.Printf("%s: DRIFT\n", app)
				for _, key := range missingInProd {
					fmt.Printf("  missing in production: %s\n", key)
				}
				for _, key := range missingInStaging {
					fmt.Printf("  missing in staging:    %s\n", key)
				}
			}
			// C2: pin awareness. head != pinned is INTENT (D1), not failure —
			// so the default posture is a WARN line, exit untouched. CI that
			// wants the release-freeze enforced passes --strict-pin.
			metaReader, err := asMetaReader(baoClient)
			if err != nil {
				fmt.Fprintln(os.Stderr, "driftcheck:", err)
				os.Exit(2)
			}
			pinDrift, err := collectPinDrift(ctx, metaReader, paths)
			if err != nil {
				fmt.Fprintln(os.Stderr, "driftcheck:", err)
				os.Exit(2)
			}
			for _, d := range pinDrift {
				fmt.Printf("%s/%s: pinned v%d, head v%d (held intent)\n", d.App, d.Env, d.Pinned, d.Head)
			}
			strict, _ := c.Flags().GetBool("strict-pin")
			if drift || (strict && len(pinDrift) > 0) {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().Bool("strict-pin", false, "fail (exit 1) when a staging/production path is pinned below its head")
	return cmd
}

// metaPaths is the read surface collectPinDrift needs — an interface so the
// classification is unit-testable without a live plane (vitest-style fakes are
// not the Go way, but a 1-method seam is).
type metaPaths interface {
	Meta(ctx context.Context, app, env string) (*bao.Meta, error)
}

func asMetaReader(c *bao.Client) (metaPaths, error) {
	if c == nil {
		return nil, fmt.Errorf("bao client unavailable")
	}
	return c, nil
}

type pinDrift struct {
	App, Env string
	Head     int
	Pinned   int
}

// collectPinDrift sweeps the staging/production paths and reports every one
// whose PINNED version sits below the head — counts and names only, never
// values (same rule as the key-set diff). Unpinned paths are alignment, not
// drift: absence of intent is not a signal.
func collectPinDrift(ctx context.Context, r metaPaths, paths []bao.Path) ([]pinDrift, error) {
	out := []pinDrift{}
	for _, path := range paths {
		if path.Env != "staging" && path.Env != "production" {
			continue
		}
		meta, err := r.Meta(ctx, path.App, path.Env)
		if err != nil {
			return nil, err
		}
		if meta == nil || meta.PinnedVersion == nil {
			continue
		}
		if *meta.PinnedVersion != meta.CurrentVersion {
			out = append(out, pinDrift{App: path.App, Env: path.Env, Head: meta.CurrentVersion, Pinned: *meta.PinnedVersion})
		}
	}
	return out, nil
}

func diffKeyNames(a, b map[string]string) (onlyA, onlyB []string) {
	for key := range a {
		if _, ok := b[key]; !ok {
			onlyA = append(onlyA, key)
		}
	}
	for key := range b {
		if _, ok := a[key]; !ok {
			onlyB = append(onlyB, key)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return onlyA, onlyB
}
