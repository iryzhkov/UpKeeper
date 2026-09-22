package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/cli"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
)

// commitEverything makes the release checkout clean, which is the state a
// capture insists on before it looks at anything else.
func commitEverything(t *testing.T, root string) {
	t.Helper()
	for _, argv := range [][]string{
		{"init", "--quiet", "-b", "main"},
		{"add", "-A"},
		{
			"-c", "user.email=test@example.invalid", "-c", "user.name=test",
			// Background maintenance outlives the commit that spawns it and
			// writes lock files under .git while the test walks the tree, so
			// the fixture switches it off as well.
			"-c", "gc.auto=0", "-c", "maintenance.auto=false",
			"commit", "--quiet", "-m", "fixture",
		},
	} {
		command := exec.Command("git", append([]string{"-C", root}, argv...)...)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, output)
		}
	}
}

// This is the whole command, from argv to exit code, on a host where the check
// cannot run: an empty home directory with no omarchy-setup checkout and no
// generator. It must refuse, it must say which degradation it is rather than
// reporting a divergence it never observed, it must exit 30 (preflight: a check
// that had to pass before anything was published did not pass), and it must
// leave the release checkout byte-for-byte as it found it.
//
// The home directory is a temporary one, so nothing here can read or write the
// real ~/.claude, ~/.config/agents or ~/.codex.
func TestPushRefusesAndWritesNothingWhenTheHostCannotBeChecked(t *testing.T) {
	root := fleetConfigRoot(t, false)
	commitEverything(t, root)
	t.Setenv(cli.ConfigRootEnvironment, root)
	t.Setenv("HOME", t.TempDir())
	before := worktreeOnly(snapshot(t, root))

	var out, errs bytes.Buffer
	code := cli.Main("upkeeper", []string{"push", "--no-commit"},
		cli.Streams{In: strings.NewReader(""), Out: &out, Err: &errs})

	if code != exitcode.Preflight {
		t.Fatalf("exit %d, wanted %d\nstdout=%s\nstderr=%s",
			code, exitcode.Preflight, out.String(), errs.String())
	}
	message := errs.String()
	for _, wanted := range []string{
		"cannot check whether this host's agent instructions match their source",
		"no omarchy-setup checkout at",
		"photographs this host's generated resident instructions",
		"Nothing has been captured, written, committed or pushed.",
	} {
		if !strings.Contains(message, wanted) {
			t.Errorf("the refusal does not say %q:\n%s", wanted, message)
		}
	}
	if after := worktreeOnly(snapshot(t, root)); !reflect.DeepEqual(before, after) {
		t.Errorf("a refused push changed the release checkout:\n%s",
			strings.Join(changedPaths(before, after), "\n"))
	}
}

// worktreeOnly drops Git's own bookkeeping from a snapshot. A read-only `git
// status` may refresh .git/index as a side effect of asking, which says nothing
// about whether the command under test wrote anything.
func worktreeOnly(files map[string]string) map[string]string {
	kept := map[string]string{}
	for path, content := range files {
		if strings.Contains(path, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			continue
		}
		kept[path] = content
	}
	return kept
}

// changedPaths names what a command that should have written nothing wrote.
func changedPaths(before, after map[string]string) []string {
	changed := []string{}
	for path, content := range after {
		if previous, present := before[path]; !present || previous != content {
			changed = append(changed, filepath.Base(path)+" ("+path+")")
		}
	}
	for path := range before {
		if _, present := after[path]; !present {
			changed = append(changed, "removed "+path)
		}
	}
	return changed
}
