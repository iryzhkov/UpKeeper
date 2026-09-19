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

// A capture photographs the pushing host's home directory, and the resident
// agent instructions it finds there are published to the whole fleet as mode
// "file" assets that overwrite every other host. No repository installs them,
// so nothing after the capture can notice that this host had drifted. These
// tests hold the one place that can notice to refusing.

// divergedSkillReport is what agents-instructions-gen --check prints when a
// skill has been edited in place on this host.
const divergedSkillReport = "agents-instructions-gen: checking this host against " +
	"/home/igor/.local/share/omarchy-setup\n" +
	"  ~/.claude/skills/t3-backlog/SKILL.md: the host copy differs from " +
	"config/claude/skills/t3-backlog/SKILL.md\n" +
	"agents-instructions-gen: this host has diverged.\n"

// divergedClaudeReport is what it prints when the shared instruction file this
// host installs is behind the checkout that generates it.
const divergedClaudeReport = "agents-instructions-gen: checking this host against " +
	"/home/igor/.local/share/omarchy-setup\n" +
	"  ~/.claude/omarchy-setup/CLAUDE.md: the host copy differs from " +
	"config/claude/CLAUDE.md\n" +
	"  ~/.config/agents/AGENTS.md: the host copy differs from what this checkout generates\n" +
	"agents-instructions-gen: this host has diverged.\n"

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
	if report["committed"] == nil {
		t.Errorf("a converged host did not publish: %v", report)
	}
}

// A skill edited in place on this host is the case the gate exists for: nothing
// installs a repository's skills onto a host, so this is the only moment at
// which such an edit is still distinguishable from an authored one.
func TestAHandEditedSkillRefusesTheCapture(t *testing.T) {
	subject := newFixture(t)
	subject.checkCode = 3
	subject.checkStdout = divergedSkillReport

	_, err := publish.Release(subject.root, subject.options())
	if err == nil {
		t.Fatal("a host with a hand-edited skill was captured")
	}
	message := err.Error()
	for _, wanted := range []string{
		"~/.claude/skills/t3-backlog/SKILL.md",
		"the host copy differs from config/claude/skills/t3-backlog/SKILL.md",
		"photographs this host's home directory",
		"--publish-diverged-instructions",
		"Nothing has been captured, written, committed or pushed.",
	} {
		if !strings.Contains(message, wanted) {
			t.Errorf("the refusal does not say %q:\n%s", wanted, message)
		}
	}
	capturedNothing(t, subject)
}

// The shared instruction file has the same shape of failure and a different
// owner: a stale copy here is published over every other host's copy.
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
