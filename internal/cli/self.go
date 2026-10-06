package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/iryzhkov/UpKeeper/internal/buildinfo"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
	"github.com/iryzhkov/UpKeeper/internal/selfupdate"
)

// selfUpdate is the seam between a self-pull and the machinery that replaces
// its binary, so that a test can supply the release, the installation and the
// exec without a network, a checkout or a second process.
type selfUpdate struct {
	// loadPin reads the program pin of the release at a checkout commit.
	loadPin func(root, commit string) (*release.ProgramPin, error)
	// verify is selfconv.VerifyProgram.
	verify func(root, binaryCommit, checkoutCommit string) error
	// checkout is selfconv.CheckoutRevision.
	checkout func(root string) (string, error)
	// binaryCommit is the source commit of the running binary.
	binaryCommit func() string
	install      func(pin *release.ProgramPin) (selfupdate.Result, error)
	rollback     func(reason string) (selfupdate.Result, error)
	generation   func() string
	exec         selfupdate.ExecFunc
	// converge is selfconv.ConvergeSelfAfterUpdate.
	converge func(root string, details map[string]any) (map[string]any, int, error)
	// scheduledHold names this host's scheduled hold marker when it exists.
	scheduledHold func(host string) (string, bool)
}

// newSelfUpdate builds the production seam. It is a variable so that a test
// of the command can replace it.
var newSelfUpdate = func() (*selfUpdate, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, failure.Validation("cannot resolve the home directory: %v", err)
	}
	installer := &selfupdate.Installer{Layout: selfupdate.Layout{Home: home}}
	return &selfUpdate{
		loadPin: func(root, commit string) (*release.ProgramPin, error) {
			value, _, _, err := release.LoadHistorical(root, commit, "", procx.Run)
			if err != nil {
				return nil, err
			}
			return value.ProgramPin()
		},
		verify: func(root, binaryCommit, checkoutCommit string) error {
			return selfconv.VerifyProgram(root, binaryCommit, checkoutCommit, procx.Run)
		},
		checkout: func(root string) (string, error) {
			return selfconv.CheckoutRevision(root, procx.Run)
		},
		binaryCommit: buildinfo.SourceCommit,
		install:      installer.Apply,
		rollback:     installer.Rollback,
		generation:   func() string { return selfupdate.ActiveGeneration(home) },
		exec:         selfupdate.Exec,
		converge: func(root string, details map[string]any) (map[string]any, int, error) {
			return selfconv.ConvergeSelfAfterUpdate(root, procx.Run, details)
		},
		scheduledHold: func(host string) (string, bool) {
			directory, err := selfconv.HoldDirectory()
			if err != nil {
				// An unknowable hold is treated as present.
				return "(hold directory unavailable)", true
			}
			marker := selfconv.HoldPath(directory, host)
			if _, err := os.Lstat(marker); err != nil {
				return "", false
			}
			return marker, true
		},
	}, nil
}

// updateProgram replaces this binary with the one the gated release pins and
// re-executes it.
//
// It returns replaced=true only when the exec returned without an error,
// which a real exec never does; the error is the refusal to report otherwise.
// Nothing here converges a component: a refusal leaves the host on the
// program it was running.
func (s *selfUpdate) updateProgram(root, refreshed string, pin *release.ProgramPin) (bool, error) {
	// The gate already proved this; it is proved again here because this is
	// the moment a host acts on it.
	if err := s.verify(root, pin.SourceCommit, refreshed); err != nil {
		return false, failure.Value(
			"the release pins upkeeper %s built from %s, which is not the gated program: %s",
			pin.Tag, pin.SourceCommit, err.Error())
	}
	result, err := s.install(pin)
	if err != nil {
		return false, failure.Wrap(err, failure.KindValue,
			"self-update to upkeeper %s refused: %s; this host keeps its current binary",
			pin.Tag, err.Error())
	}
	err = selfupdate.Reexec(s.exec, result.Path, refreshed)
	if err == nil {
		return true, nil
	}
	if !result.Switched {
		return false, failure.Value("cannot start upkeeper %s at %s: %v", pin.Tag, result.Path, err)
	}
	restored, rollbackErr := s.rollback("re-exec failed: " + err.Error())
	if rollbackErr != nil {
		return false, failure.Value(
			"cannot start upkeeper %s at %s: %v; rolling back also failed: %v",
			pin.Tag, result.Path, err, rollbackErr)
	}
	return false, failure.Value("cannot start upkeeper %s at %s: %v; rolled back to generation %s",
		pin.Tag, result.Path, err, restored.Generation)
}

// afterUpdate is the self-pull of a freshly installed binary. It refreshes
// nothing: the checkout must still be at exactly the commit it was installed
// for, and its program must now be this binary's. If it is not, the update did
// not do what it was for and the previous generation is restored before any
// component is touched.
func (s *selfUpdate) afterUpdate(root, commit string) (map[string]any, int, error) {
	revision, err := s.checkout(root)
	if err != nil {
		return nil, 0, err
	}
	if revision != commit {
		return nil, 0, failure.Value(
			"the checkout moved from %s to %s while this binary was installed; "+
				"the next self-pull converges it", commit, revision)
	}
	if err := s.verify(root, s.binaryCommit(), commit); err != nil {
		restored, rollbackErr := s.rollback("post-update program check failed")
		if rollbackErr != nil {
			return nil, 0, failure.Value(
				"the installed binary is not the gated program: %s; rolling back also failed: %v",
				err.Error(), rollbackErr)
		}
		return nil, 0, failure.Value(
			"the installed binary is not the gated program: %s; rolled back to generation %s",
			err.Error(), restored.Generation)
	}
	return s.converge(root, map[string]any{
		"after_update":  commit,
		"binary_commit": s.binaryCommit(),
		"version":       buildinfo.Version,
		"generation":    s.generation(),
	})
}

// programUpdate decides what a self-pull does with VerifyProgram's answer.
//
// A changed program is updated to the pinned binary, and without a pin it
// stays the refusal it always was; any other refusal is returned unchanged.
// An unchanged program built from another commit than the pin is updated
// too, because the agent's program fence compares commits and a host left on
// it would be refused by every controller pull; a failure of that update is
// reported on notice and convergence goes on, since the program is the gated
// one either way. A scheduled hold on this host stops every self-update: the
// host is waiting for an operator, not for a newer binary.
func programUpdate(root, host, refreshed string, verified error, notice io.Writer) (bool, error) {
	if verified != nil && !errors.Is(verified, selfconv.ErrProgramDiffers) {
		return false, verified
	}
	updater, err := newSelfUpdate()
	if err != nil {
		return false, err
	}
	pin, err := updater.loadPin(root, refreshed)
	if err != nil {
		return false, err
	}
	if verified == nil && (pin == nil || updater.binaryCommit() == pin.SourceCommit) {
		return false, nil
	}
	if pin == nil {
		return false, failure.Wrap(verified, failure.KindValue,
			"%s; the release at %s pins no upkeeper binary to update to", verified.Error(), refreshed)
	}
	if marker, held := updater.scheduledHold(host); held {
		if verified != nil {
			return false, failure.Wrap(verified, failure.KindValue,
				"%s; self-update skipped while the scheduled hold %s is present", verified.Error(), marker)
		}
		return false, nil
	}
	replaced, err := updater.updateProgram(root, refreshed, pin)
	if err != nil && verified == nil {
		fmt.Fprintf(notice, "upkeeper self-pull: update to the pinned binary skipped, "+
			"converging with the same program: %s\n", firstLineOf(err.Error()))
		return false, nil
	}
	return replaced, err
}

// runSelf is `upkeeper self install|rollback|status`.
func runSelf(parsed *arguments, streams Streams) (int, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return exitcode.Preflight, failure.Validation("cannot resolve the home directory: %v", err)
	}
	installer := &selfupdate.Installer{Layout: selfupdate.Layout{Home: home}}
	document := map[string]any{
		"schema_version": exitcode.SchemaVersion,
		"command":        "self",
		"action":         parsed.selfAction,
	}
	var result selfupdate.Result
	switch parsed.selfAction {
	case "status":
		status := installer.Status()
		if parsed.asJSON {
			document["installation"] = status
			return exitcode.Success, printJSON(streams.Out, document)
		}
		printSelfStatus(streams.Out, status)
		return exitcode.Success, nil
	case "rollback":
		result, err = installer.Rollback("operator")
	case "install":
		result, err = installSelf(installer, parsed.selfExpectSHA256)
	}
	if err != nil {
		if failure.Is(err, failure.KindLock) {
			fmt.Fprintf(streams.Err, "upkeeper: %s\n", err.Error())
			return exitcode.LockContention, nil
		}
		return exitcode.Preflight, err
	}
	document["result"] = result.Map()
	if parsed.asJSON {
		return exitcode.Success, printJSON(streams.Out, document)
	}
	verb := "installed"
	if parsed.selfAction == "rollback" {
		verb = "rolled back to"
	}
	if !result.Switched {
		verb = "already on"
	}
	fmt.Fprintf(streams.Out, "%s generation %s\n", verb, result.Generation)
	if result.Adopted != "" {
		fmt.Fprintf(streams.Out, "adopted the previous binary as generation %s\n", result.Adopted)
	}
	if result.Previous != "" {
		fmt.Fprintf(streams.Out, "previous generation %s\n", result.Previous)
	}
	return exitcode.Success, nil
}

// installSelf installs the binary running this command. It must name a clean
// source commit, as every binary a self-pull will run must, and its digest
// must be the one the operator checked.
func installSelf(installer *selfupdate.Installer, expectSHA256 string) (selfupdate.Result, error) {
	commit := buildinfo.SourceCommit()
	if !fullCommit(commit) {
		return selfupdate.Result{}, failure.Value(
			"this binary does not name a clean source commit (%s); it cannot be installed",
			displayValue(commit))
	}
	executable, err := os.Executable()
	if err != nil {
		return selfupdate.Result{}, failure.Value("cannot locate this binary: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return installer.InstallFile(executable, expectSHA256, buildinfo.Summary())
}

func displayValue(value string) string {
	if value == "" {
		return "unset"
	}
	return value
}
