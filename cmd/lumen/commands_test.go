package main

import "testing"

// TestRootCommandsExist — the guard for the regression the owner's terminal
// caught: user/driftcheck lost from AddCommand while their files compiled
// fine. The CLI's verb set is part of its interface; it gets a test.
func TestRootCommandsExist(t *testing.T) {
	want := map[string]bool{
		"server": false, "worker": false, "all": false,
		"user": false, "driftcheck": false,
	}
	for _, cmd := range newRootCommand().Commands() {
		if _, ok := want[cmd.Name()]; ok {
			want[cmd.Name()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("subcommand %q is not registered on the root", name)
		}
	}
}
