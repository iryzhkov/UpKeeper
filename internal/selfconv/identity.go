// Package selfconv holds the scheduled and self-managed convergence paths: a
// host that brings itself onto the published release without a controller ever
// connecting to it.
//
// These are the entry points systemd timers call, so every failure has to end
// in a report and an exit code rather than a traceback, and every dispatch has
// to leave a durable marker behind so that a machine which died mid-run is
// observed by a human before it is retried.
package selfconv

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/transport"
)

// CheckoutRevision returns the commit a checkout is on, and refuses anything
// that is not exactly the release checkout, clean.
//
// A dirty checkout is refused rather than stashed: whatever is in the tree may
// be work in progress, and losing it would be worse than not converging.
func CheckoutRevision(root string, runner procx.Runner) (string, error) {
	if runner == nil {
		runner = procx.Run
	}
	git := func(arguments ...string) (string, error) {
		result := runner(procx.Command{
			Argv:    append([]string{"git", "-C", root}, arguments...),
			Timeout: 30 * time.Second,
		})
		if result.Err != nil {
			return "", failure.Validation("cannot inspect self checkout")
		}
		if result.Code != 0 {
			return "", failure.Validation("--self requires a readable Git checkout")
		}
		return strings.TrimSpace(result.Stdout), nil
	}
	toplevel, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	resolvedTop, err := filepath.EvalSymlinks(toplevel)
	if err != nil {
		resolvedTop = filepath.Clean(toplevel)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = filepath.Clean(root)
	}
	if resolvedTop != resolvedRoot {
		return "", failure.Validation("--self must use the dev-fleet checkout root")
	}
	status, err := git("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", failure.Validation(
			"--self refuses a dirty checkout; commit or preserve work first")
	}
	return git("rev-parse", "--verify", "HEAD")
}

// LocalHost names this machine in the inventory.
//
// The match is on the hashed machine identity and nothing else: there is no SSH
// fallback and no hostname heuristic, because a self-converging host that
// misidentifies itself would apply another machine's configuration.
func LocalHost(config *inventory.Config) (string, error) {
	identity := transport.LocalMachineIDSHA256()
	matches := []string{}
	if identity != "" {
		for _, name := range config.HostNames() {
			if config.Hosts[name].MachineIDSHA256 == identity {
				matches = append(matches, name)
			}
		}
	}
	if len(matches) != 1 {
		return "", failure.Validation(
			"--self requires exactly one inventory match for this machine-id")
	}
	return matches[0], nil
}
