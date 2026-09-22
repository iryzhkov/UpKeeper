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

// The resident agent instructions are the shared CLAUDE.md and the AGENTS.md
// family under ~/.config/agents. The environment capturer reads those generated
// files out of the publishing host. Repository-owned skills have a pinned source
// tree and are projected separately; installed skill copies are not inputs here.
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

// residentCapabilityMarkers are the two options the check is spelled with.
//
// They are looked for in the installed generator before it is run, and their
// absence is the degraded case rather than a reason to try anyway. This is not
// defensive pedantry: the generator that predates the check ignores every
// argument it is given and rewrites ~/.config/agents and ~/.codex/AGENTS.md
// unconditionally, then exits 0. Running that copy as a check would modify the
// host it was supposed to be reporting on and report success while doing it,
// which is the exact failure this gate exists to prevent, performed by the gate
// itself. A host whose generator is that old has not converged omarchy-setup,
// which is the thing the check would have told it.
var residentCapabilityMarkers = []string{"--check", "--root"}

// residentGeneratorSizeLimit bounds how much of an installed command is read
// while looking for those markers. The generator is a script of a few tens of
// kilobytes; anything far larger is not the command this expects.
const residentGeneratorSizeLimit = 1 << 20

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
const residentWhy = "A release capture photographs this host's generated resident " +
	"instructions. The shared CLAUDE.md and files under ~/.config/agents are copied " +
	"into the manifest exactly as this machine holds them, and convergence writes " +
	"those bytes to every other host. Repository-owned skills are different: UpKeeper " +
	"reads them from the pinned source checkout and ignores installed copies. A host " +
	"whose generated resident instructions drifted would still publish that drift."

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
	if err := residentGeneratorAnswersChecks(generator, info.Size()); err != nil {
		return err
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
	reported := residentReportedPaths(result.Stdout)
	if len(reported) == 0 {
		return residentUncheckable(fmt.Sprintf(
			"%s --root %s --check reported a divergence without naming a path: %s",
			generator, checkout, residentDetail(result)))
	}
	diverged := residentPublishedPaths(reported)
	if len(diverged) == 0 {
		// Older generators also compared installed skills. Those paths are no
		// longer capture inputs, so an installed skill difference is irrelevant.
		return nil
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

// residentGeneratorAnswersChecks refuses to run an installed generator that
// cannot be asked a question, because the copy that cannot be asked answers by
// rewriting the files instead.
//
// Be exact about how much this establishes. It is a substring test over the
// installed file's bytes, so it proves only that both option strings appear
// somewhere in it — in a usage line, in a comment, in a here-document. It does
// not prove the command implements them. What it reliably recognises is the one
// copy it was written for: the generator that predates the check parses no
// arguments at all and mentions neither string, so it is refused before it can
// rewrite the host it was asked to report on. A generator that merely mentions
// both flags, ignores them and exits 0 rather than dying on an unknown option
// would pass here and be believed, which is a residual risk this inspection
// does not close.
//
// It deliberately does not compare the installed generator against the copy in
// the checkout. That would be a second definition of "this host is converged",
// stated here rather than in the repository that owns the files, and it would
// refuse every capture on a host whose checkout carries an instruction-neutral
// change to the generator that has not been converged yet — a state in which
// nothing wrong would be published. The place to close the gap is the
// generator's own --check, which could compare its installed copy with
// $root/config/bin/agents-instructions-gen and report the difference as one
// more diverged path, keeping one definition of diverged in one repository.
func residentGeneratorAnswersChecks(generator string, size int64) error {
	if size > residentGeneratorSizeLimit {
		return residentUncheckable(fmt.Sprintf(
			"%s is %d bytes, which is not the generator script this expects", generator, size))
	}
	payload, err := os.ReadFile(generator)
	if err != nil {
		return residentUncheckable(fmt.Sprintf("%s cannot be read: %v", generator, err))
	}
	for _, marker := range residentCapabilityMarkers {
		if strings.Contains(string(payload), marker) {
			continue
		}
		return residentUncheckable(fmt.Sprintf(
			"the generator installed at %s does not understand %s, so it predates the "+
				"check; that copy ignores its arguments and rewrites this host's "+
				"instruction files instead of reporting on them, so it is not run. "+
				"Converge omarchy-setup to install the copy that can answer",
			generator, marker))
	}
	return nil
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
//
// What it must not keep is the rest of the report. A divergence is printed as a
// header, then the block of path lines, then an epilogue telling the operator
// how to converge, and the third line of that epilogue is indented exactly like
// a path line:
//
//	/home/igor/.local/share/omarchy-setup/run.sh   (or: agents-instructions-gen --root …)
//
// Keeping it put a converge command into the override's list of the paths it
// promotes, which is the one artifact the override exists to produce. It also
// meant that a report naming no path at all still yielded one line here, so the
// "divergence without naming a path" refusal could never fire against the real
// generator.
//
// Two conditions separate the block from the epilogue. A path line belongs to
// the first run of indented lines, which the epilogue's unindented opening
// ends, and its indent is followed by one unbroken token and ": ", which
// "run.sh   (or: …" is not. A line in a shape neither condition recognises is
// dropped rather than reported, which can only turn a divergence into the
// refusal that says no path was named; it can never turn one into a capture.
func residentPublishedPaths(reported []string) []string {
	published := make([]string, 0, len(reported))
	for _, entry := range reported {
		trimmed := strings.TrimSpace(entry)
		if strings.HasPrefix(trimmed, "~/.claude/skills/") ||
			strings.HasPrefix(trimmed, "~/.agents/skills/") ||
			strings.HasPrefix(trimmed, "~/.codex/skills/") {
			continue
		}
		published = append(published, entry)
	}
	return published
}

func residentReportedPaths(stdout string) []string {
	paths := []string{}
	block := false
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.TrimSpace(line) == "" {
			if block {
				break
			}
			continue
		}
		block = true
		entry := strings.TrimSpace(line)
		if !residentNamesPath(entry) {
			continue
		}
		paths = append(paths, "  "+entry)
	}
	return paths
}

// residentNamesPath reports whether one line of the generator's path block
// opens with a path: a single unbroken token, then ": ", then the direction of
// the difference. That is the shape the generator prints every comparison in,
// and the shape its converge epilogue does not have.
func residentNamesPath(entry string) bool {
	label, direction, found := strings.Cut(entry, ": ")
	return found && label != "" && direction != "" && !strings.ContainsAny(label, " \t")
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
