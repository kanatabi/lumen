package main

// config.go — the config-plane gate verbs (config-gate contract). check-epoch
// is the CI dispatcher's preflight: the DECLARED epoch of an app/env must
// agree with the vault's head, and a PIN — which is a lock, not a label —
// must not disagree with head. Same portal AppRole as driftcheck
// (LUMEN_BAO_ADDR / LUMEN_BAO_ROLE_ID / LUMEN_BAO_SECRET_ID); same exit
// discipline: 0 aligned, 1 gate failure (the remedy line), 2 plane/config
// error. The remedy line's SHAPE AND CAUSE TEXTS are wire contract — sibling
// agents' docs and UI match them verbatim; evaluateEpoch + the line render are
// pure so the strings themselves get unit-proven without a plane.

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/bao"
)

// The two causes, verbatim from the contract ("<cause>: <fix>" — cause 1
// carries its fix in parentheses, cause 2 after the colon).
const (
	epochCauseDeclared = "align the epoch bump with the vault (materialize the pin via unpin→restore→save, or re-dispatch with the number the matrix shows)"
	epochCausePin      = "pin is a lock and it disagrees with head: unpin to release the head, or restore the pinned version's content as new head"
)

func configCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "config-plane gates over the vault (read-only CI verbs)",
	}
	cmd.AddCommand(checkEpochCommand())
	return cmd
}

func checkEpochCommand() *cobra.Command {
	var app, env string
	var declared int
	cmd := &cobra.Command{
		Use:   "check-epoch",
		Short: "gate a dispatch: declared epoch == vault head, and a pin lock must agree",
		Long: `check-epoch compares a dispatcher's DECLARED config epoch against the
vault truth for <app>/<env>.

Exit 0 iff declared == head AND (no pin OR pin == declared).
A pin is a LOCK: while one is set the relay refuses saves to the path
(403), so a pin that disagrees with head means intent and content
diverged — the gate says which remedy releases the freeze.

Declared "0" is the NOT YET UNDER CONTRACT exemption. Head is always >= 1,
so a plain comparison could never pass an initial annotation: "0" instead
passes iff NO pin is set. The pin keeps full authority in block mode —
declaring 0 at a PINNED path fails: pin != head takes the lock remedy,
pin == head means the path IS under contract, re-dispatch with the number
the matrix shows.

Failure prints EXACTLY one remedy line to stderr:
  config-gate: <app>/<env> declared <N>; vault head <H>, pin <P|unset> — <cause>: <fix>

Exit codes: 0 aligned · 1 gate failure (remedy line) · 2 plane/config error
(unreachable vault, missing path, invalid flags). Vault access rides the
same portal AppRole env as driftcheck: LUMEN_BAO_ADDR, LUMEN_BAO_ROLE_ID,
LUMEN_BAO_SECRET_ID. The check costs ONE kv/metadata GET: it answers both
the head version (current_version) and the pin (custom_metadata
pinned_version) — the data read would see neither.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !adminsvc.ValidAppSlug(app) || !adminsvc.ValidEnvSlug(env) {
				fmt.Fprintln(os.Stderr, "config-gate: --app/--env must match the slug grammar")
				os.Exit(2)
			}
			if declared < 0 {
				fmt.Fprintln(os.Stderr, "config-gate: --declared must be >= 0")
				os.Exit(2)
			}
			baoClient, err := bao.New()
			if err != nil {
				fmt.Fprintln(os.Stderr, "config-gate: plane unreachable:", err)
				os.Exit(2)
			}
			meta, err := baoClient.Meta(context.Background(), app, env)
			if err != nil {
				fmt.Fprintln(os.Stderr, "config-gate: plane unreachable:", err)
				os.Exit(2)
			}
			if meta == nil {
				// a path the vault does not know is a dispatch misfit, not a
				// epoch verdict — the two documented causes would both lie
				fmt.Fprintf(os.Stderr, "config-gate: %s/%s: no such path in the vault\n", app, env)
				os.Exit(2)
			}
			line := epochLine(app, env, declared, meta.CurrentVersion, meta.PinnedVersion)
			ok, cause := evaluateEpoch(declared, meta.CurrentVersion, meta.PinnedVersion)
			if ok {
				fmt.Println(line + " — ok")
				return nil
			}
			fmt.Fprintln(os.Stderr, line+" — "+cause)
			os.Exit(1)
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "vault app namespace (required)")
	cmd.Flags().StringVar(&env, "env", "", "vault env under that app (required)")
	cmd.Flags().IntVar(&declared, "declared", -1, `the epoch number the dispatcher declares; "0" = not yet under contract (pass iff no pin)`)
	// Deliberately NO MarkFlagRequired: cobra answers a missing flag with
	// exit 1, which would collide with the contract's "1 = gate failure".
	// Unset flags fall into the RunE guards below and exit 2 (config error).
	return cmd
}

// evaluateEpoch is the gate's whole decision, pure. See the Long text for
// the "0" exemption rationale.
func evaluateEpoch(declared, head int, pin *int) (ok bool, cause string) {
	if declared == 0 {
		// not yet under contract: the ONLY authority left is the pin lock
		if pin == nil {
			return true, ""
		}
		if *pin != head {
			return false, epochCausePin
		}
		return false, epochCauseDeclared // a pin exists ⇒ there IS a contract: name it
	}
	if declared != head {
		return false, epochCauseDeclared
	}
	// declared == head: "pin == declared" and "pin == head" are the same test
	if pin != nil && *pin != head {
		return false, epochCausePin
	}
	return true, ""
}

// epochLine renders the contract's prefix — "config-gate: <app>/<env>
// declared <N>; vault head <H>, pin <P|unset>". The verdict/cause rides
// behind " — " appended by the caller.
func epochLine(app, env string, declared, head int, pin *int) string {
	pinText := "unset"
	if pin != nil {
		pinText = strconv.Itoa(*pin)
	}
	return fmt.Sprintf("config-gate: %s/%s declared %d; vault head %d, pin %s", app, env, declared, head, pinText)
}
