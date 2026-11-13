// cmd/lumen — one binary, explicit process roles (owner ruling, 2026-11-09,
// refined by the ROLE split of 2026-11-13):
//
//	LUMEN_ROLE = "admin" — the CONTROL plane (lumen-admin): session mint +
//	  verify, users, the Bao relay, whoami. Its DB holds ONLY the users/
//	  session-lineage tables. No content routes, no queue, no public reads.
//	LUMEN_ROLE = "cms"   — the CONTENT plane (lumen-staging/production):
//	  §1 reads, admin content desks, the index worker, seed. Auth is
//	  INTROSPECTED against the admin plane (fail closed); no users table,
//	  no mint, no relay.
//	unset ("all")       — the legacy dev plane: every surface in one
//	  process with local JWT auth, so the dev loop and tests are unchanged.
//
// Verbs:
//
//	lumen all        server + queue worker in one process — THE DEV LOOP
//	                   (refuses a split LUMEN_ROLE: deploy server/worker)
//	lumen server     HTTP surfaces for the role; NEVER consumes the queue
//	lumen worker     consumes index jobs only (cms plane); serves no HTTP
//	lumen user       bootstrap/upsert an admin account (admin-plane subset)
//	lumen migrate    schema for the role's subset (--role, LUMEN_ROLE-aware)
//	lumen driftcheck compare staging/production secret key sets (CI verb)
//	lumen config     config-plane verbs: check-epoch (gate) · keys (the
//	                   machine-readable bindings surface per role)
//
// One repo, separable processes: the images deploy `server` (both planes)
// and `worker` (data pool, cms) independently; nothing else changes.
// Stdlib signals make the shutdown honest — river's Stop releases the
// leader lease, which the old kill9 left dangling (the stranded-jobs
// incident this closes structurally).
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
