package release

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// The resident agent instructions are the shared CLAUDE.md, the AGENTS.md
// family under ~/.config/agents and the skills under ~/.claude/skills. Every
// agent session on every host loads them, and they are the one part of a
// release with no repository behind it at capture time: the environment
// capturer reads them out of the pushing host's home directory, stores them as
// mode "file" assets, and convergence writes those exact bytes over the same
// paths on every other host.
//
// Nothing downstream can notice a mistake here. A manifest captured from a host
// whose copies have drifted is well formed, its assets validate, and every
// convergence that applies it succeeds. The difference between what this host
// holds and what the fleet agreed to is visible only on the machine the capture
// is taken from, which makes this the only place it can be refused.

// ResidentGenerator is the path, relative to the home directory, of the command
// that decides whether this host's resident instructions are converged.
//
// The comparison is delegated to it rather than reimplemented here on purpose.
// It lives beside the files it compares, in the repository that generates them,
// so it already knows every artifact and it will know the next one without this
// package being changed. A second implementation would be a second definition
// of "diverged", and two definitions of one rule is the defect this gate exists
// to close.
const ResidentGenerator = ".local/bin/agents-instructions-gen"

// residentDivergedExit is the exit status the generator's --check mode uses for
// "this host has diverged". Zero is converged. Every other status means the
// check itself could not run, which is refused rather than believed.
const residentDivergedExit = 3

// residentCheckTimeout bounds the check. It reads a few dozen small files and
// compares them, so a minute is generous; a check that hangs is a check that
// could not run.
const residentCheckTimeout = 60 * time.Second

// ResidentOverrideFlag is the operator's explicit, named permission to publish
// this host's resident instructions even though they disagree with their
// source. It is spelled out in refusals so that the person reading one does not
// have to go looking for it.
const ResidentOverrideFlag = "--publish-diverged-instructions"

// residentWhy explains the mechanism to somebody who has never seen this code.
// It is repeated in both refusals because either one may be the first thing a
// new operator ever reads about how the resident instruction layer travels.
const residentWhy = "A release capture photographs this host's home directory. " +
	"The shared CLAUDE.md, the files under ~/.config/agents and every skill under " +
	"~/.claude/skills are copied into the manifest exactly as this machine holds " +
	"them, and the next convergence writes those bytes over the same paths on every " +
	"other host. No repository installs a skill onto a host, so a file edited in " +
	"place here becomes the fleet's instruction to every agent on every machine, and " +
	"an edit made anywhere else is destroyed without a word. A host that has drifted " +
	"from its source would publish its drift."

// ResidentPolicy is how a capture treats resident instructions that do not
// match their source.
type ResidentPolicy struct {
	// PublishDiverged is the operator's decision, asked for by name, to publish
	// what this host holds. It never applies to a check that could not run.
	PublishDiverged bool
	// Notice receives the path-by-path account of what an override promotes.
	// An override with nowhere to print that account is refused.
	Notice io.Writer
}

// guardResidentInstructions refuses a capture that would publish resident agent
// instructions this host cannot show to be the ones its source generates.
//
// It runs before anything is observed or written, so a refusal leaves no
// candidate manifest, no stored asset, no commit to amend and no branch behind.
func guardResidentInstructions(
	runner procx.Runner, home string, setup map[string]any, policy ResidentPolicy,
) error {
	path, _ := setup["path"].(string)
	if path == "" {
		return residentUncheckable(
			"the release names no omarchy-setup checkout, so there is no source to " +
				"compare this host against")
	}
	checkout := filepath.Join(home, path)
	if info, err := os.Stat(filepath.Join(checkout, ".git")); err != nil || info == nil {
		return residentUncheckable(fmt.Sprintf(
			"no omarchy-setup checkout at %s, which is where the instructions this "+
				"host installs are generated from", checkout))
	}
	generator := filepath.Join(home, ResidentGenerator)
	info, err := os.Stat(generator)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return residentUncheckable(fmt.Sprintf(
			"no executable %s, which is the command that decides whether this host "+
				"matches %s; the omarchy-setup 28-scripts module installs it",
			generator, checkout))
	}
	result := runner(procx.Command{
		Argv:    []string{generator, "--root", checkout, "--check"},
		Env:     residentEnvironment(home),
		Timeout: residentCheckTimeout,
	})
	switch {
	case result.Err != nil:
		return residentUncheckable(fmt.Sprintf(
			"%s --root %s --check could not be run: %v", generator, checkout, result.Err))
	case result.Code == 0:
		return nil
	case result.Code != residentDivergedExit:
		return residentUncheckable(fmt.Sprintf(
			"%s --root %s --check exited %d rather than 0 (converged) or %d (diverged): %s",
			generator, checkout, result.Code, residentDivergedExit,
			residentDetail(result)))
	}
	diverged := residentReportedPaths(result.Stdout)
	if len(diverged) == 0 {
		return residentUncheckable(fmt.Sprintf(
			"%s --root %s --check reported a divergence without naming a path: %s",
			generator, checkout, residentDetail(result)))
	}
	if policy.PublishDiverged {
		if policy.Notice == nil {
			return failure.Capture(
				"agent-environment: %s was given but this capture has nowhere to print "+
					"what it would promote, and an override nobody can read is not an "+
					"override.", ResidentOverrideFlag)
		}
		fmt.Fprintf(policy.Notice,
			"%s: publishing this host's agent instructions although they differ from %s.\n"+
				"Each path below is promoted to every host in the fleet on its next "+
				"convergence, overwriting whatever that host holds:\n%s\n",
			ResidentOverrideFlag, checkout, strings.Join(diverged, "\n"))
		return nil
	}
	return failure.Capture(
		"agent-environment: this host's agent instructions have diverged from %s, "+
			"and capturing it would publish that divergence to the whole fleet.\n%s\n\n%s\n\n"+
			"Converge this host and capture again:\n"+
			"  %s/run.sh\n  %s --root %s --check\n"+
			"If these bytes are the ones that should win, commit them to the "+
			"omarchy-setup checkout and converge from there; a capture is not a place "+
			"to author instructions. To publish them from this host anyway, ask for it "+
			"by name with %s, which prints every path it promotes.\n"+
			"Nothing has been captured, written, committed or pushed.",
		checkout, strings.Join(diverged, "\n"), residentWhy,
		checkout, generator, checkout, ResidentOverrideFlag)
}

// residentUncheckable refuses a capture whose resident instructions could not
// be checked at all.
//
// "I could not check" is not "there is nothing to check". Treating the two as
// the same is the silent-success shape this gate exists to remove, so the
// degraded case refuses as firmly as a detected divergence and names which
// degradation it is.
func residentUncheckable(reason string) error {
	return failure.Capture(
		"agent-environment: cannot check whether this host's agent instructions "+
			"match their source: %s.\n\n%s\n\n"+
			"A capture taken now would publish this host unexamined, so it is refused: "+
			"\"could not check\" is not \"nothing to check\". Repair the cause and "+
			"capture again, or capture only the components that do not carry the "+
			"resident instructions with --components. There is deliberately no "+
			"override for this case: an override has to print what it promotes, and a "+
			"check that could not run cannot name it.\n"+
			"Nothing has been captured, written, committed or pushed.",
		reason, residentWhy)
}

// residentReportedPaths keeps the generator's own per-path lines, which already
// carry the direction of each difference: whether the host copy differs from
// what the checkout generates, is absent, or exists on this host and is
// generated by nothing.
func residentReportedPaths(stdout string) []string {
	lines := []string{}
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, "  "+strings.TrimSpace(line))
	}
	return lines
}

// residentDetail summarises what a check that failed to run had to say, bounded
// so that a broken command cannot turn a refusal into a wall of output.
func residentDetail(result procx.Result) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		return "it printed nothing"
	}
	return tail(detail, 400)
}

// residentEnvironment runs the check against the home directory this capture is
// reading, which is not necessarily the process's own. OMARCHY_SETUP_ROOT is
// dropped because the checkout is passed explicitly and an inherited value
// naming a different tree would make the answer depend on the caller's shell.
func residentEnvironment(home string) []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "HOME=") || strings.HasPrefix(entry, "OMARCHY_SETUP_ROOT=") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "HOME="+home)
}
