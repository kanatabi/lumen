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
	return &cobra.Command{
		Use:   "driftcheck",
		Short: "compare staging vs production secret key sets (names only, never values)",
		RunE: func(*cobra.Command, []string) error {
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
			if drift {
				os.Exit(1)
			}
			return nil
		},
	}
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
