// cmd/lumen — one binary, explicit process roles (owner ruling, 2026-11-09):
//
//	lumen all        server + queue worker in one process — THE DEV LOOP
//	lumen server     HTTP surfaces only; NEVER consumes the queue
//	lumen worker     consumes index jobs only; serves no HTTP
//	lumen user       bootstrap/upsert an admin account (argon2id)
//	lumen driftcheck compare staging/production secret key sets (CI verb)
//	lumen config     config-plane gates (check-epoch: dispatch vs vault head/pin)
//
// One repo, separable processes: when the cluster split comes (vortex phase)
// the images deploy `server` (edge) and `worker` (data pool) independently;
// nothing else changes. Stdlib signals make the shutdown honest — river's
// Stop releases the leader lease, which the old kill9 left dangling (the
// stranded-jobs incident this closes structurally).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// buildVersion is stamped via -ldflags -X main.buildVersion in the image build.
var buildVersion = "dev"

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCommand is a function (not main's body) so commands_test.go can
// assert the FULL verb set — the regression this guards: an unregistered
// subcommand compiles clean (unused funcs are legal Go) and fails only at
// the user's terminal. Never again silently.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:     "lumen",
		Short:   "the blog's REST backend — API server, index worker, and bootstrap verbs",
		Version: fmt.Sprintf("lumen %s", buildVersion),
	}
	root.AddCommand(
		migrateCommand(),
		serverCommand(),
		workerCommand(),
		allCommand(),
		userCommand(),
		driftcheckCommand(),
		configCommand(),
	)
	return root
}
