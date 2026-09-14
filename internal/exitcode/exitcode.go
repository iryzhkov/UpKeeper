// Package exitcode holds the stable process exit codes of the UpKeeper CLI.
//
// These values are part of the public contract. They are consumed by systemd
// units, scheduled wrappers and operators, so they may never be renumbered.
package exitcode

// SchemaVersion is the version of every JSON document the CLI prints.
const SchemaVersion = 1

// The stable exit codes. Exit code 2 is additionally reserved for command-line
// usage errors, matching the historical argparse behaviour.
const (
	Success        = 0
	Drift          = 10
	Partial        = 20
	Preflight      = 30
	Deployment     = 40
	Rollback       = 50
	LockContention = 60

	// Usage is what a malformed command line returns. It is not part of the
	// documented set but it is observable, so it is preserved deliberately.
	Usage = 2
)

// Names maps each stable exit code to its reporting name.
var Names = map[int]string{
	Success:        "success",
	Drift:          "drift",
	Partial:        "partial",
	Preflight:      "preflight-failure",
	Deployment:     "deployment-failure",
	Rollback:       "rollback-failure",
	LockContention: "lock-contention",
}
