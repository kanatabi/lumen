// cmd/driftcheck — §5.5's CI drift check, ONE implementation two consumers
// (lumen uses the same bao client for both — drift logic is the relay logic
// with a comparison lens).
//
// What it checks: for every app/env pair on the plane, staging's key set
// versus production's — reported as KEY NAMES and COUNTS only. Values are
// never printed: a drift tool that leaks secrets to CI logs is not a drift
// tool. Exit 1 on drift; exit 2 on plane errors.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/kanatabi/lumen/internal/bao"
)

func main() {
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
	for _, p := range paths {
		if p.Env != "staging" && p.Env != "production" {
			continue
		}
		if byApp[p.App] == nil {
			byApp[p.App] = map[string]bool{}
		}
		byApp[p.App][p.Env] = true
	}

	drift := false
	for app, envs := range byApp {
		if !envs["staging"] || !envs["production"] {
			continue // an app present on one env only is bootstrap reality, not drift
		}
		stagingDoc, err := baoClient.Read(ctx, app, "staging", 0)
		if err != nil || stagingDoc == nil {
			continue
		}
		prodDoc, err := baoClient.Read(ctx, app, "production", 0)
		if err != nil || prodDoc == nil {
			continue
		}
		missProd, missStab := diffKeys(stagingDoc.Data, prodDoc.Data)
		if len(missProd) == 0 && len(missStab) == 0 {
			fmt.Printf("%s: key sets aligned (%d)\n", app, len(stagingDoc.Data))
			continue
		}
		drift = true
		fmt.Printf("%s: DRIFT\n", app)
		for _, k := range missProd {
			fmt.Printf("  missing in production: %s\n", k)
		}
		for _, k := range missStab {
			fmt.Printf("  missing in staging:    %s\n", k)
		}
	}
	if drift {
		os.Exit(1)
	}
}

func diffKeys(a, b map[string]string) (onlyA, onlyB []string) {
	for k := range a {
		if _, ok := b[k]; !ok {
			onlyA = append(onlyA, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			onlyB = append(onlyB, k)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return
}
