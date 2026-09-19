package cli

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/release"
)

// The capture gate's override has to be unreachable by accident. It is off
// unless it is typed, it has no short form, no general --force stands in for
// it, and the help names it so that an operator who meets the refusal can find
// out what they would be agreeing to before they agree to it.
func TestTheInstructionOverrideIsOnlyReachableByItsOwnName(t *testing.T) {
	if parsed := parseOrFail(t, "push"); parsed.publishDivergedInstructions {
		t.Error("a plain push already carries permission to publish a diverged host")
	}
	parsed := parseOrFail(t, "push", "origin", "--publish-diverged-instructions")
	if !parsed.publishDivergedInstructions {
		t.Error("the named override was not parsed")
	}
	for _, spelling := range []string{"--force", "-f", "--yes", "--publish-diverged"} {
		if _, err := parse([]string{"push", spelling}, release.ComponentOrder); err == nil {
			t.Errorf("push accepted %q, so the override has a second way in", spelling)
		}
	}
	help := helpText("upkeeper", "push")
	if !strings.Contains(help, "--publish-diverged-instructions") {
		t.Errorf("push help does not name the override:\n%s", help)
	}
}
