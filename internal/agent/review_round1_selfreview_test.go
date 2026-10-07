package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// strictCommandV answers `bash -c "command -v a b ... >/dev/null"` the way a
// shell that wants every name would, looking each one up on the command's own
// PATH. Other commands succeed.
func strictCommandV(command procx.Command) procx.Result {
	argv := command.Argv
	if len(argv) == 3 && argv[0] == "bash" && argv[1] == "-c" && strings.HasPrefix(argv[2], "command -v ") {
		for _, name := range strings.Fields(strings.TrimSuffix(strings.TrimPrefix(argv[2], "command -v "), ">/dev/null")) {
			if procx.LookupIn(name, command.Env) == "" {
				return procx.Result{Code: 1}
			}
		}
	}
	return procx.Result{}
}

// The preflight asks for secret-tool only when the release has to be staged,
// so staging must not ask for it either once the release is staged.
func TestSelfReviewAStagedReleaseDoesNotNeedTheStagingCLIs(t *testing.T) {
	home := t.TempDir()
	names := []string{}
	for _, name := range freshTestCommands {
		if name != "secret-tool" {
			names = append(names, name)
		}
	}
	bin := freshTestTools(t, home, names...)
	spec := validToolingSpec(t)
	tool, err := release.DecodeTooling(spec)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".local/share/upkeeper-tools/releases", toolingIdentity(spec))
	adapterTestWrite(t, filepath.Join(dir, ".complete"), "staged\n")
	adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/key"), "redacted\n")
	c := &Context{Home: home, RunID: "run", Env: []string{"PATH=" + bin, "HOME=" + home}, Runner: strictCommandV}
	if block := requirementsBlock(c, release.ToolingComponent, spec, &[]string{}); strings.Contains(block, "secret-tool") {
		t.Fatalf("preflight = %s", block)
	}
	if err := stageTooling(c, dir, tool); err != nil && strings.Contains(err.Error(), "CLI prerequisites") {
		t.Fatalf("a staged release was refused for a staging-only CLI: %v", err)
	}
	// A release that still has to be staged needs secret-tool only when the
	// OV key could come from nowhere but the keyring; the preflight checks it,
	// by name, before staging runs.
	if err := os.Remove(filepath.Join(dir, ".complete")); err != nil {
		t.Fatal(err)
	}
	if block := requirementsBlock(c, release.ToolingComponent, spec, &[]string{}); strings.Contains(block, "secret-tool") {
		t.Fatalf("staging with a key file = %s", block)
	}
	if err := os.Remove(filepath.Join(home, ".config/ov-mcp/key")); err != nil {
		t.Fatal(err)
	}
	if block := requirementsBlock(c, release.ToolingComponent, spec, &[]string{}); !strings.Contains(block, "missing OS prerequisite secret-tool") {
		t.Fatalf("staging with only the keyring = %s", block)
	}
}

func selfReviewStage(t *testing.T, root, started, identity string) string {
	t.Helper()
	name := started + "-" + shortIdentity(identity)
	marker, err := json.Marshal(toolingStageOwner{CreatedBy: "upkeeper", Identity: identity, RunID: "r", StartedAt: started})
	if err != nil {
		t.Fatal(err)
	}
	adapterTestWrite(t, filepath.Join(root, name, toolingStageMarker), string(marker))
	adapterTestWrite(t, filepath.Join(root, name, "ov-memory/requirements.lock"), "x\n")
	return name
}

// A prune that is interrupted part way through a stage, by a crash or by a
// file it cannot remove yet, is finished by a later prune: the stage never
// loses the marker that proves it is UpKeeper's before everything else is gone.
func TestSelfReviewAnInterruptedPruneIsFinishedLater(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	const identity = "0123456789abcdef0123456789abcdef"
	oldest := selfReviewStage(t, root, "20261001T000000Z", identity)
	stuck := filepath.Join(root, oldest, "venv")
	adapterTestWrite(t, filepath.Join(stuck, "lib.py"), "x")
	if err := os.Chmod(stuck, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	for _, started := range []string{"20261002T000000Z", "20261003T000000Z", "20261004T000000Z"} {
		selfReviewStage(t, root, started, identity)
	}
	if err := pruneFailedStages(root); err == nil {
		t.Fatal("the first prune removed a directory it had no permission to empty")
	}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
	selfReviewStage(t, root, "20261005T000000Z", identity)
	if err := pruneFailedStages(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{
		"20261003T000000Z-" + shortIdentity(identity),
		"20261004T000000Z-" + shortIdentity(identity),
		"20261005T000000Z-" + shortIdentity(identity),
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("after an interrupted prune kept %v, want %v", names, want)
	}
}

// A directory put at a validated stage's name after it was validated is not
// the stage that was validated, and is never deleted.
func TestSelfReviewPruningNeverDeletesADirectorySwappedInAfterValidation(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for i := 0; i < 300; i++ {
		root := t.TempDir()
		stamps := []string{"20261001T000000Z", "20261002T000000Z", "20261003T000000Z", "20261004T000000Z"}
		for _, stamp := range stamps {
			selfReviewStage(t, root, stamp, identity)
		}
		oldest := filepath.Join(root, stamps[0]+"-"+shortIdentity(identity))
		var swapped atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			if os.Rename(oldest, filepath.Join(root, "moved-away")) != nil {
				return
			}
			if os.Mkdir(oldest, 0o700) == nil &&
				os.WriteFile(filepath.Join(oldest, "precious"), []byte("foreign"), 0o600) == nil {
				swapped.Store(true)
			}
		}()
		_ = pruneFailedStages(root)
		<-done
		if !swapped.Load() {
			continue
		}
		found := false
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.Name() == "precious" {
				found = true
			}
			return nil
		})
		if !found {
			t.Fatalf("iteration %d: a foreign directory swapped in after validation was deleted", i)
		}
	}
}

// npm reports ENOENT for a global prefix whose lib directory does not exist
// yet, such as a user prefix nothing was ever installed into. A prefix
// without lib holds no global package, so that is absence too, but only when
// the prefix can be read and its lib directory is really missing.
func TestSelfReviewAGlobalPrefixWithoutLibHoldsNoT3(t *testing.T) {
	enoent := procx.Result{Code: 254, Stdout: `{"error":{"code":"ENOENT","summary":"ENOENT: no such file or directory, lstat 'PREFIX/lib'"}}`,
		Stderr: "npm error code ENOENT"}
	cases := []struct {
		name   string
		lib    bool
		prefix func(dir string) procx.Result
		absent bool
	}{
		{name: "prefix without lib", prefix: func(dir string) procx.Result { return procx.Result{Stdout: dir + "\n"} }, absent: true},
		{name: "prefix with lib", lib: true, prefix: func(dir string) procx.Result { return procx.Result{Stdout: dir + "\n"} }},
		{name: "prefix unknown", prefix: func(string) procx.Result { return procx.Result{Code: 1, Stderr: "EACCES"} }},
		{name: "prefix relative", prefix: func(string) procx.Result { return procx.Result{Stdout: "relative/prefix\n"} }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			c, r := stewardPairPrepare(t, stewardPairHost{})
			prefix := filepath.Join(t.TempDir(), "prefix")
			if err := os.MkdirAll(prefix, 0o700); err != nil {
				t.Fatal(err)
			}
			if test.lib {
				if err := os.MkdirAll(filepath.Join(prefix, "lib"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			inner := r.handle
			r.handle = func(command procx.Command) procx.Result {
				switch {
				case adapterTestMatches(command.Argv, "npm", "ls"):
					return enoent
				case adapterTestMatches(command.Argv, "npm", "prefix"):
					return test.prefix(prefix)
				}
				return inner(command)
			}
			state, actions, err := stewardPairApply(t, c)
			if test.absent && (err != nil || state != "updated") {
				t.Fatalf("fresh host with an empty prefix refused: state=%s actions=%v err=%v", state, actions, err)
			}
			if !test.absent && err == nil {
				t.Fatalf("unknown T3 accepted as absent: state=%s actions=%v", state, actions)
			}
		})
	}
}
