package publish_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/publish"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// A capture projects repository-owned skills from the pinned source checkout
// and photographs the remaining resident agent environment. The resident gate
// must ignore only source-projected skill outputs: every other generated
// instruction divergence still refuses publication. These tests hold that
// ownership boundary.

// divergedHeader and divergedEpilogue are what the generator prints around the
// block of diverged paths. The epilogue is here because of what its third line
// looks like: it is indented by the same two spaces as a path line and it is a
// command, not a path. A fixture that stops at the path block never shows the
// filter the one line it has to exclude, which is how the converge command came
// to be listed among the paths an override promotes.
const divergedHeader = "agents-instructions-gen: checking this host against " +
	"/home/igor/.local/share/omarchy-setup\n"

const divergedEpilogue = "agents-instructions-gen: this host has diverged from " +
	"/home/igor/.local/share/omarchy-setup.\n" +
	"The repository is authoritative. Converge the host with\n" +
	"  /home/igor/.local/share/omarchy-setup/run.sh   " +
	"(or: agents-instructions-gen --root /home/igor/.local/share/omarchy-setup)\n" +
	"and never by editing an installed copy: the next convergence overwrites it, and an\n" +
	"UpKeeper capture taken from this host would publish it to the whole fleet.\n"

// convergeCommandLine is that indented line. No refusal and no override account
// may present it as a path this host promotes to the fleet.
const convergeCommandLine = "/home/igor/.local/share/omarchy-setup/run.sh   (or:"

// divergedSkillReport is what agents-instructions-gen --check prints when a
// skill has been edited in place on this host.
const divergedSkillReport = divergedHeader +
	"  ~/.claude/skills/t3-backlog/SKILL.md: the host copy differs from " +
	"config/claude/skills/t3-backlog/SKILL.md\n" +
	divergedEpilogue

// divergedClaudeReport is what it prints when the shared instruction file this
// host installs is behind the checkout that generates it.
const divergedClaudeReport = divergedHeader +
	"  ~/.claude/omarchy-setup/CLAUDE.md: the host copy differs from " +
	"config/claude/CLAUDE.md\n" +
	"  ~/.config/agents/AGENTS.md: the host copy differs from what this checkout generates\n" +
	divergedEpilogue

const divergedSkillAndInstructionReport = divergedHeader +
	"  ~/.claude/skills/t3-backlog/SKILL.md: the host copy differs from " +
	"config/claude/skills/t3-backlog/SKILL.md\n" +
	"  ~/.config/agents/AGENTS.md: the host copy differs from what this checkout generates\n" +
	divergedEpilogue

// capturedNothing holds a refused capture to having left no trace: no candidate
// manifest to review, no staged change, no commit to amend, no branch pushed.
func capturedNothing(t *testing.T, subject *fixture) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(subject.root, release.File)); !os.IsNotExist(err) {
		t.Errorf("a refused capture left a candidate manifest behind (%v)", err)
	}
	for _, forbidden := range []string{" add ", " commit ", " push "} {
		if subject.did(forbidden) {
			t.Errorf("a refused capture ran git%s", forbidden)
		}
	}
}

// checkCommand is the check the gate must have run, with the checkout named
// explicitly so that the answer cannot depend on the operator's shell.
func checkCommand(subject *fixture) string {
	return filepath.Join(subject.home, ".local/bin/agents-instructions-gen") +
		" --root " + filepath.Join(subject.home, ".local/share/omarchy-setup") + " --check"
}

// A converged host publishes, and it publishes because it was asked, not
// because nobody looked. The assertion that matters here is the second one: a
// gate that lets everything through is indistinguishable from no gate at all
// unless the check is shown to have run.
func TestAConvergedHostIsCheckedAndThenCaptured(t *testing.T) {
	subject := newFixture(t)

	report, err := publish.Release(subject.root, subject.options())
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !subject.did(checkCommand(subject)) {
		t.Fatalf("the capture never asked whether this host matches its source; it ran %v",
			subject.calls)
	}
	wantSkills := filepath.Join(subject.home,
		".local/share/omarchy-setup/config/claude/skills")
	if subject.capturedRepositorySkills != wantSkills {
		t.Fatalf("the capture projected skills from %q rather than pinned source %q",
			subject.capturedRepositorySkills, wantSkills)
	}
	if report["committed"] == nil {
		t.Errorf("a converged host did not publish: %v", report)
	}
}

// The check has to be asked about the host this capture is photographing, and
// about nothing else. HOME is what the generator resolves every path it
// compares against, so a check that inherited the process's own HOME would
// report on one machine while the capture published another, and the capture
// would believe it. OMARCHY_SETUP_ROOT is dropped for the same reason: the
// checkout is named on the command line, and an inherited value pointing at a
// different tree would make the answer depend on the shell push was run from.
func TestTheCheckIsAskedAboutTheCapturedHomeAndNothingElse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OMARCHY_SETUP_ROOT", "/somewhere/else/entirely")
	subject := newFixture(t)

	if _, err := publish.Release(subject.root, subject.options()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if subject.checkEnv == nil {
		t.Fatalf("the check never ran, so its environment proves nothing: %v", subject.calls)
	}
	homes := []string{}
	for _, entry := range subject.checkEnv {
		switch {
		case strings.HasPrefix(entry, "HOME="):
			homes = append(homes, entry)
		case strings.HasPrefix(entry, "OMARCHY_SETUP_ROOT="):
			t.Errorf("the check inherited %s, so its answer depends on the caller's shell", entry)
		}
	}
	if len(homes) != 1 || homes[0] != "HOME="+subject.home {
		t.Errorf("the check ran with %v rather than with HOME=%s, the home being captured",
			homes, subject.home)
	}
}

// Installed repository-skill copies are outputs, not publication inputs. An
// older generator may still report one, but UpKeeper replaces that name from
// the pinned source checkout and must not require a manual host overlay.
func TestAHandEditedSkillDoesNotBlockSourceProjection(t *testing.T) {
	subject := newFixture(t)
	subject.checkCode = 3
	subject.checkStdout = divergedSkillReport

	report, err := publish.Release(subject.root, subject.options())
	if err != nil {
		t.Fatalf("a source-projected skill was treated as a capture input: %v", err)
	}
	if report["committed"] == nil {
		t.Errorf("the release was not published: %v", report)
	}
}

// The shared instruction file has the same shape of failure and a different
// owner: a stale copy here is published over every other host's copy.
func TestSkillFilteringDoesNotHideInstructionDrift(t *testing.T) {
	subject := newFixture(t)
	subject.checkCode = 3
	subject.checkStdout = divergedSkillAndInstructionReport

	_, err := publish.Release(subject.root, subject.options())
	if err == nil {
		t.Fatal("a real instruction divergence was hidden beside a source-projected skill")
	}
	message := err.Error()
	if !strings.Contains(message,
		"~/.config/agents/AGENTS.md: the host copy differs from what this checkout generates") {
		t.Errorf("the refusal does not retain the real instruction divergence:\n%s", message)
	}
	if strings.Contains(message, "~/.claude/skills/t3-backlog/SKILL.md") {
		t.Errorf("the refusal still treats a source-projected skill as resident input:\n%s", message)
	}
	capturedNothing(t, subject)
}

func TestAStaleSharedInstructionFileRefusesTheCapture(t *testing.T) {
	subject := newFixture(t)
	subject.checkCode = 3
	subject.checkStdout = divergedClaudeReport

	_, err := publish.Release(subject.root, subject.options())
	if err == nil {
		t.Fatal("a host with a stale shared instruction file was captured")
	}
	message := err.Error()
	for _, wanted := range []string{
		"~/.claude/omarchy-setup/CLAUDE.md: the host copy differs from config/claude/CLAUDE.md",
		"~/.config/agents/AGENTS.md: the host copy differs from what this checkout generates",
	} {
		if !strings.Contains(message, wanted) {
			t.Errorf("the refusal does not name %q:\n%s", wanted, message)
		}
	}
	capturedNothing(t, subject)
}

// "I could not check" is not "there is nothing to check". Every way the check
// can fail to produce an answer refuses the capture, names which failure it
// was, and says so in words that cannot be mistaken for a divergence report.
func TestACaptureThatCannotBeCheckedIsRefused(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		breakIt func(t *testing.T, subject *fixture)
		wanted  string
	}{
		{
			name:    "the generator is not installed",
			breakIt: func(t *testing.T, subject *fixture) { subject.removeGenerator(t) },
			wanted:  "no executable",
		},
		{
			name: "there is no omarchy-setup checkout to compare against",
			breakIt: func(t *testing.T, subject *fixture) {
				if err := os.RemoveAll(
					filepath.Join(subject.home, ".local/share/omarchy-setup")); err != nil {
					t.Fatalf("RemoveAll: %v", err)
				}
			},
			wanted: "no omarchy-setup checkout at",
		},
		{
			// The generator that predates the check ignores every argument
			// and rewrites ~/.config/agents and ~/.codex/AGENTS.md, then
			// exits 0. Running it as a check would modify the host it was
			// supposed to report on and call that success.
			name: "the installed generator predates the check",
			breakIt: func(t *testing.T, subject *fixture) {
				subject.installGenerator(t, legacyGenerator)
			},
			wanted: "does not understand --check",
		},
		{
			name: "the check itself failed",
			breakIt: func(t *testing.T, subject *fixture) {
				subject.checkCode = 1
				subject.checkStdout = ""
				subject.checkErr = nil
			},
			wanted: "exited 1 rather than 0 (converged) or 3 (diverged)",
		},
		{
			name: "the check reported a divergence without naming a path",
			breakIt: func(t *testing.T, subject *fixture) {
				subject.checkCode = 3
				subject.checkStdout = "agents-instructions-gen: something is wrong\n"
			},
			wanted: "reported a divergence without naming a path",
		},
		{
			// The shape a real generator would produce if it lost its path
			// block: the header, then straight to the converge advice. The
			// epilogue's indented line is a command, so nothing here names a
			// path and the capture must refuse as unexaminable rather than
			// report a divergence in a file called run.sh.
			name: "the check reported nothing but its converge epilogue",
			breakIt: func(t *testing.T, subject *fixture) {
				subject.checkCode = 3
				subject.checkStdout = divergedHeader + divergedEpilogue
			},
			wanted: "reported a divergence without naming a path",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			subject := newFixture(t)
			testCase.breakIt(t, subject)

			_, err := publish.Release(subject.root, subject.options())
			if err == nil {
				t.Fatal("an unexaminable host was captured")
			}
			message := err.Error()
			if !strings.Contains(message,
				"cannot check whether this host's agent instructions match their source") {
				t.Errorf("the refusal is not the degraded one:\n%s", message)
			}
			if !strings.Contains(message, testCase.wanted) {
				t.Errorf("the refusal does not say which degradation it is (%q):\n%s",
					testCase.wanted, message)
			}
			if strings.Contains(message, "have diverged from") {
				t.Errorf("a check that could not run was reported as a divergence:\n%s", message)
			}
			// An expected refusal is what makes the CLI exit 30,
			// exitcode.Preflight: a check that had to pass before anything was
			// deployed did not pass. An unexpected error would exit 1 and read
			// as a crash instead of a decision.
			if !failure.IsExpected(err) {
				t.Errorf("the refusal would exit 1 rather than 30: %#v", err)
			}
			capturedNothing(t, subject)
		})
	}
}

// The gate must not be the thing that modifies the host. The generator that
// predates the check ignores its arguments and rewrites ~/.config/agents and
// ~/.codex/AGENTS.md, so asking that copy for a report would silently install
// over the files it was asked about and then exit 0. It is recognised and
// refused without being executed at all.
func TestAGeneratorThatPredatesTheCheckIsNeverRun(t *testing.T) {
	subject := newFixture(t)
	subject.installGenerator(t, legacyGenerator)

	_, err := publish.Release(subject.root, subject.options())
	if err == nil {
		t.Fatal("a host whose generator cannot be asked a question was captured")
	}
	if subject.did("agents-instructions-gen") {
		t.Errorf("the gate ran the copy that would have rewritten the host: %v", subject.calls)
	}
	capturedNothing(t, subject)
}

// A selection that does not name agent-environment publishes none of the
// resident instructions, so it is not gated. This is the escape hatch that
// keeps a host with a broken check from being unable to publish anything at
// all, and it is safe precisely because such a capture restores the resident
// assets the release already carried.
func TestASelectionThatPublishesNoInstructionsIsNotGated(t *testing.T) {
	subject := newFixture(t)
	if _, err := publish.Release(subject.root, subject.options()); err != nil {
		t.Fatalf("first capture: %v", err)
	}
	subject.removeGenerator(t)
	subject.calls = nil

	options := subject.options()
	options.Components = []string{"t3-steward"}
	if _, err := publish.Release(subject.root, options); err != nil {
		t.Fatalf("a capture that publishes no agent instructions was refused: %v", err)
	}
	if subject.did("agents-instructions-gen") {
		t.Errorf("a capture that publishes no agent instructions still ran the check")
	}
}
