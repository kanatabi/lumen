package main

// check-epoch unit battery — the config-gate truth table and the VERBATIM
// remedy lines (sibling agents' docs and UI match these strings, so the
// constants and the line render get pinned at byte level, no plane needed).

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEvaluateEpoch(t *testing.T) {
	cases := []struct {
		name           string
		declared, head int
		pin            *int
		wantOK         bool
		wantCause      string
	}{
		// the strict contract: 0 iff declared==head AND (no pin OR pin==declared)
		{"declared==head, no pin", 3, 3, nil, true, ""},
		{"declared==head, pin==head", 3, 3, ptr(3), true, ""},
		{"declared==head, pin≠head → cause 2", 3, 3, ptr(2), false, epochCausePin},
		{"declared≠head, no pin → cause 1", 2, 3, nil, false, epochCauseDeclared},
		{"declared≠head, pin==head → cause 1 (any pin)", 2, 3, ptr(3), false, epochCauseDeclared},
		{"declared≠head, pin≠head → cause 1 owns the line (the dispatcher's own lie first)", 4, 3, ptr(2), false, epochCauseDeclared},
		// the "0" exemption (DECIDED, in the Long text): head is always >= 1
		// so 0 could never match a number; "0" means NOT YET UNDER CONTRACT
		// and passes IFF no pin — block-mode keeps the pin's authority.
		{"declared 0, no pin → pass regardless of head", 0, 7, nil, true, ""},
		{"declared 0, pin≠head → cause 2 (the lock)", 0, 7, ptr(5), false, epochCausePin},
		{"declared 0, pin==head → a contract EXISTS → cause 1 (name the number)", 0, 7, ptr(7), false, epochCauseDeclared},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, cause := evaluateEpoch(c.declared, c.head, c.pin)
			if ok != c.wantOK || cause != c.wantCause {
				t.Fatalf("evaluateEpoch(%d,%d,%v) = (%v,%q), want (%v,%q)",
					c.declared, c.head, c.pin, ok, cause, c.wantOK, c.wantCause)
			}
		})
	}
}

func TestEpochLineShapeIsContract(t *testing.T) {
	// "config-gate: <app>/<env> declared <N>; vault head <H>, pin <P|unset>"
	if got := epochLine("lumen", "production", 3, 4, nil); got !=
		"config-gate: lumen/production declared 3; vault head 4, pin unset" {
		t.Fatalf("prefix drifted: %q", got)
	}
	if got := epochLine("earendil", "staging", 5, 5, ptr(2)); got !=
		"config-gate: earendil/staging declared 5; vault head 5, pin 2" {
		t.Fatalf("pin render drifted: %q", got)
	}
}

func TestEpochRemediesVerbatim(t *testing.T) {
	// the COMPLETE failure line as CI will see it on stderr — byte-for-byte
	// what the sibling docs promise
	cases := []struct {
		name           string
		app, env       string
		declared, head int
		pin            *int
		want           string
	}{
		{"cause 1", "lumen", "production", 3, 4, nil,
			"config-gate: lumen/production declared 3; vault head 4, pin unset — align the epoch bump with the vault (materialize the pin via unpin→restore→save, or re-dispatch with the number the matrix shows)"},
		{"cause 2", "earendil", "staging", 5, 5, ptr(2),
			"config-gate: earendil/staging declared 5; vault head 5, pin 2 — pin is a lock and it disagrees with head: unpin to release the head, or restore the pinned version's content as new head"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, cause := evaluateEpoch(c.declared, c.head, c.pin)
			if ok {
				t.Fatal("these rows must fail")
			}
			line := epochLine(c.app, c.env, c.declared, c.head, c.pin) + " — " + cause
			if line != c.want {
				t.Fatalf("remedy line drifted:\n got %q\nwant %q", line, c.want)
			}
		})
	}
}

func TestCheckEpochWiredIntoConfigGroup(t *testing.T) {
	// the commands_test.go lesson: an unattached verb compiles fine and
	// fails only at the terminal
	var epoch *cobra.Command
	for _, sub := range configCommand().Commands() {
		if sub.Name() == "check-epoch" {
			epoch = sub
		}
	}
	if epoch == nil {
		t.Fatal("check-epoch is not attached to the config group")
	}
	for _, name := range []string{"app", "env", "declared"} {
		if epoch.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s missing", name)
		}
	}
	// the "0" semantic must live in the help text the operator actually reads
	if !strings.Contains(epoch.Long, `Declared "0"`) || !strings.Contains(epoch.Long, "NOT YET UNDER CONTRACT") {
		t.Error("the declared-0 exemption must be documented in the Long help")
	}
}
