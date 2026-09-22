package publish_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/publish"
)

// The override exists because a gate with no way through it is a gate that gets
// removed the first time it is inconvenient. It is deliberately narrow: it is
// asked for by its own name, it applies only to a divergence that has been
// enumerated, and it prints every path it promotes before it promotes it.

func TestTheOverridePublishesADivergedHostAndPrintsEveryPath(t *testing.T) {
	subject := newFixture(t)
	subject.checkCode = 3
	subject.checkStdout = divergedClaudeReport
	var notice bytes.Buffer
	options := subject.options()
	options.PublishDivergedInstructions = true
	options.Notice = &notice

	report, err := publish.Release(subject.root, options)
	if err != nil {
		t.Fatalf("the named override did not publish: %v", err)
	}
	if report["committed"] == nil {
		t.Errorf("the override published nothing: %v", report)
	}
	printed := notice.String()
	for _, wanted := range []string{
		"--publish-diverged-instructions",
		"~/.claude/omarchy-setup/CLAUDE.md: the host copy differs from config/claude/CLAUDE.md",
		"~/.config/agents/AGENTS.md: the host copy differs from what this checkout generates",
		"every host in the fleet",
	} {
		if !strings.Contains(printed, wanted) {
			t.Errorf("the override did not account for %q:\n%s", wanted, printed)
		}
	}
	// The account is the whole point of the override: the operator reads it and
	// decides whether to let those bytes reach every host. Anything in it that is
	// not a promoted path makes it a worse record than none, and the generator's
	// converge advice is indented exactly like a path.
	if strings.Contains(printed, convergeCommandLine) {
		t.Errorf("the override listed the converge command as a path it promotes:\n%s", printed)
	}
	accounted := 0
	for _, line := range strings.Split(printed, "\n") {
		if strings.HasPrefix(line, "  ") {
			accounted++
		}
	}
	if accounted != 2 {
		t.Errorf("the override accounted for %d paths where the check named 2:\n%s",
			accounted, printed)
	}
}

// The override has to be asked for. A capture that does not name it refuses the
// same host, and no other option on the command reaches it.
func TestADivergedHostIsRefusedWhenTheOverrideIsNotNamed(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		adjust func(options *publish.Options)
		wanted string
	}{
		{name: "a plain capture", adjust: func(*publish.Options) {},
			wanted: "have diverged from"},
		{
			// A manifest-only run writes the candidate manifest, and a
			// candidate manifest holding this host's drift is exactly what the
			// next reviewer would commit.
			name:   "a manifest-only capture",
			adjust: func(options *publish.Options) { options.Commit, options.Push = false, false },
			wanted: "have diverged from",
		},
		{
			name:   "a capture that would not push",
			adjust: func(options *publish.Options) { options.Push = false },
			wanted: "have diverged from",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			subject := newFixture(t)
			subject.checkCode = 3
			subject.checkStdout = divergedClaudeReport
			options := subject.options()
			testCase.adjust(&options)

			_, err := publish.Release(subject.root, options)
			if err == nil {
				t.Fatal("a diverged host was captured without the override being named")
			}
			if !strings.Contains(err.Error(), testCase.wanted) {
				t.Errorf("unexpected refusal:\n%s", err.Error())
			}
			capturedNothing(t, subject)
		})
	}
}

// The override never covers a check that could not run. An override has to say
// what it is promoting, and a check with no answer cannot name it; treating the
// two cases alike would restore exactly the silent promotion the gate removes.
func TestTheOverrideDoesNotCoverACheckThatCouldNotRun(t *testing.T) {
	subject := newFixture(t)
	subject.removeGenerator(t)
	var notice bytes.Buffer
	options := subject.options()
	options.PublishDivergedInstructions = true
	options.Notice = &notice

	_, err := publish.Release(subject.root, options)
	if err == nil {
		t.Fatal("the override published a host that was never checked")
	}
	if !strings.Contains(err.Error(), "no override for this case") {
		t.Errorf("the refusal does not explain why the override does not apply:\n%s", err.Error())
	}
	if notice.Len() != 0 {
		t.Errorf("a refused override still announced a promotion: %s", notice.String())
	}
	capturedNothing(t, subject)
}

// An override with nowhere to print its account is not an override. A caller
// that sets the flag and provides no writer is refused rather than quietly
// promoting a divergence nobody was told about.
func TestAnOverrideWithNowhereToReportIsRefused(t *testing.T) {
	subject := newFixture(t)
	subject.checkCode = 3
	subject.checkStdout = divergedClaudeReport
	options := subject.options()
	options.PublishDivergedInstructions = true
	options.Notice = nil

	_, err := publish.Release(subject.root, options)
	if err == nil {
		t.Fatal("a divergence was promoted with no account of what it promoted")
	}
	if !strings.Contains(err.Error(), "nowhere to print") {
		t.Errorf("unexpected refusal:\n%s", err.Error())
	}
	capturedNothing(t, subject)
}
