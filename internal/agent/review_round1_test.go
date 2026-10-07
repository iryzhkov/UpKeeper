package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// The keyring CLI is one of several places the OV credential can live, and the
// tooling bundle only needs it while staging. A host that has already staged
// the release and keeps its key somewhere else must not be blocked for it, on
// any package manager family.
func TestReviewConvergedHostWithoutOptionalKeyringCLI(t *testing.T) {
	sources := map[string]func(t *testing.T, home string, c *Context){
		"key file": func(t *testing.T, home string, _ *Context) {
			adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/key"), "host-local-sentinel\n")
		},
		"environment": func(_ *testing.T, _ string, c *Context) {
			c.Env = append(c.Env, "OV_API_KEY=host-local-sentinel")
		},
		"config file": func(t *testing.T, home string, _ *Context) {
			adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/config.toml"), "api_key = \"host-local-sentinel\"\n")
		},
	}
	for _, family := range []string{"debian", "arch", "darwin"} {
		for source, provide := range sources {
			t.Run(family+"/"+source, func(t *testing.T) {
				home := t.TempDir()
				names := []string{}
				for _, name := range freshTestCommands {
					if name != "secret-tool" {
						names = append(names, name)
					}
				}
				bin := freshTestTools(t, home, names...)
				spec := validToolingSpec(t)
				adapterTestWrite(t, filepath.Join(home, ".local/share/upkeeper-tools/releases",
					toolingIdentity(spec), ".complete"), "staged\n")
				ran := false
				replaceHandler(t, release.ToolingComponent, Handler{
					Observe: func(*Context, map[string]any) (map[string]any, error) {
						return map[string]any{"drift": []string{}}, nil
					},
					Apply: func(*Context, map[string]any, map[string]any, *[]string) (string, error) {
						ran = true
						return "converged", nil
					},
				})
				installed := []string{}
				c := freshTestContext(t, home, bin, func(command procx.Command) procx.Result {
					if len(command.Argv) > 0 && (command.Argv[0] == "sudo" || command.Argv[0] == "brew") {
						installed = append(installed, strings.Join(command.Argv, " "))
					}
					return procx.Result{}
				}, []string{release.ToolingComponent}, map[string]any{release.ToolingComponent: spec})
				c.Host.OSID = family
				c.Host.InstallOSPackages = true
				provide(t, home, c)
				entry := resultEntry(t, Execute(c), release.ToolingComponent)
				if !ran || entry["state"] != "converged" {
					t.Fatalf("already staged host with a local key was blocked: %v", entry)
				}
				if len(installed) != 0 {
					t.Fatalf("an opted-in host installed packages it does not need: %v", installed)
				}
			})
		}
	}
}

// A host that has to stage, and whose OV key could only come from the
// keyring, is told about the keyring CLI up front. One whose key is in the
// key file does not need the keyring CLI at all.
func TestReviewStagingHostIsStillToldAboutTheKeyringCLI(t *testing.T) {
	for _, keyFile := range []bool{false, true} {
		home := t.TempDir()
		names := []string{}
		for _, name := range freshTestCommands {
			if name != "secret-tool" {
				names = append(names, name)
			}
		}
		bin := freshTestTools(t, home, names...)
		if keyFile {
			adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/key"), "redacted\n")
		}
		ran := stubTooling(t)
		c := freshTestContext(t, home, bin, func(procx.Command) procx.Result { return procx.Result{} },
			[]string{release.ToolingComponent}, map[string]any{release.ToolingComponent: validToolingSpec(t)})
		entry := resultEntry(t, Execute(c), release.ToolingComponent)
		message, _ := entry["error"].(string)
		if keyFile {
			if !*ran || entry["state"] != "updated" || strings.Contains(message, "secret-tool") {
				t.Fatalf("a host with a key file that stages without secret-tool = %v", entry)
			}
			continue
		}
		if *ran || entry["state"] != "blocked" || !strings.Contains(message, "secret-tool") ||
			!strings.Contains(message, "libsecret-tools") {
			t.Fatalf("a host that stages without secret-tool = %v", entry)
		}
	}
}

// Only a successfully parsed npm answer that lists no t3 package proves the
// package is absent. Any other answer leaves the host's T3 unknown, and the
// steward pair transition keeps its proof requirement.
func TestReviewPairRefusesUnknownNPMPackageState(t *testing.T) {
	answers := map[string]procx.Result{
		"permission error":      {Code: 1, Stderr: "EACCES: cannot inspect the existing global package tree"},
		"npm missing":           {Code: -1, Err: errors.New("exec: \"npm\": executable file not found in $PATH")},
		"timeout":               {Code: -1, TimedOut: true},
		"malformed json":        {Code: 1, Stdout: `{"dependencies": `},
		"json error report":     {Code: 1, Stdout: `{"error": {"code": "EACCES", "summary": "permission denied"}}`},
		"not an object":         {Code: 1, Stdout: `[]`},
		"unexpected exit code":  {Code: 243, Stdout: `{}`},
		"dependencies not map":  {Code: 1, Stdout: `{"dependencies": "t3"}`},
		"t3 listed unversioned": {Code: 1, Stdout: `{"dependencies": {"t3": {"missing": true}}}`},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			c, r := stewardPairPrepare(t, stewardPairHost{})
			inner := r.handle
			r.handle = func(command procx.Command) procx.Result {
				if adapterTestMatches(command.Argv, "npm", "ls") {
					return answer
				}
				return inner(command)
			}
			state, actions, err := stewardPairApply(t, c)
			if err == nil {
				t.Fatalf("unknown installed T3 accepted as absent: state=%s actions=%v", state, actions)
			}
		})
	}
}

// npm's own answers for a package that is not installed still prove absence.
func TestReviewPairAcceptsNPMsEmptyAnswers(t *testing.T) {
	answers := map[string]procx.Result{
		"empty object":      {Code: 1, Stdout: `{}`},
		"named prefix":      {Code: 1, Stdout: `{"name": "lib"}`},
		"other packages":    {Code: 0, Stdout: `{"name": "lib", "dependencies": {"npm": {"version": "10.9.2"}}}`},
		"warning on stderr": {Code: 1, Stdout: `{}`, Stderr: "npm warn config production Use --omit=dev instead."},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			c, r := stewardPairPrepare(t, stewardPairHost{})
			inner := r.handle
			r.handle = func(command procx.Command) procx.Result {
				if adapterTestMatches(command.Argv, "npm", "ls") {
					return answer
				}
				return inner(command)
			}
			state, actions, err := stewardPairApply(t, c)
			if err != nil || state != "updated" {
				t.Fatalf("fresh host refused: state=%s actions=%v err=%v", state, actions, err)
			}
		})
	}
}

// Pruning counts and deletes only failed stages whose marker proves that
// UpKeeper moved them there, for the identity in their name.
func TestReviewPruningPreservesUnknownMarkerContents(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	markers := map[string]string{
		"operator":    `{"created_by":"operator","identity":"not-an-upkeeper-release"}`,
		"malformed":   `{"created_by":"upkeeper",`,
		"mismatched":  `{"created_by":"upkeeper","identity":"fedcba9876543210","started_at":"20261001T000000Z"}`,
		"empty":       ``,
		"wrong time":  `{"created_by":"upkeeper","identity":"` + identity + `","started_at":"20250101T000000Z"}`,
		"no identity": `{"created_by":"upkeeper","started_at":"20261001T000000Z"}`,
		"oversized":   `{"created_by":"upkeeper","identity":"` + identity + `","pad":"` + strings.Repeat("x", 5000) + `"}`,
	}
	for kind, marker := range markers {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			for _, stamp := range []string{"20261001T000000Z", "20261002T000000Z", "20261003T000000Z", "20261004T000000Z"} {
				dir := filepath.Join(root, stamp+"-"+shortIdentity(identity))
				content := strings.ReplaceAll(marker, "20261001T000000Z", stamp)
				adapterTestWrite(t, filepath.Join(dir, toolingStageMarker), content)
				adapterTestWrite(t, filepath.Join(dir, "diagnostic"), "keep me")
			}
			if err := pruneFailedStages(root); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 4 {
				t.Fatalf("unknown-origin directories deleted: %v (%v)", entries, err)
			}
		})
	}
}

// Validated archives are still pruned to the limit, oldest first, and a
// foreign directory among them neither counts nor goes.
func TestReviewPruningStillBoundsValidatedArchives(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	root := t.TempDir()
	stamps := []string{"20261001T000000Z", "20261002T000000Z", "20261003T000000Z", "20261004T000000Z", "20261005T000000Z"}
	for _, stamp := range stamps {
		adapterTestWrite(t, filepath.Join(root, stamp+"-"+shortIdentity(identity), toolingStageMarker),
			`{"created_by":"upkeeper","identity":"`+identity+`","run_id":"r","started_at":"`+stamp+`"}`)
	}
	// The same second twice gets a counter suffix.
	adapterTestWrite(t, filepath.Join(root, "20261005T000000Z-"+shortIdentity(identity)+"-002", toolingStageMarker),
		`{"created_by":"upkeeper","identity":"`+identity+`","run_id":"r","started_at":"20261005T000000Z"}`)
	foreign := filepath.Join(root, "20260901T000000Z-"+shortIdentity(identity))
	adapterTestWrite(t, filepath.Join(foreign, toolingStageMarker), `{"created_by":"operator"}`)
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
		"20260901T000000Z-" + shortIdentity(identity),
		"20261004T000000Z-" + shortIdentity(identity),
		"20261005T000000Z-" + shortIdentity(identity),
		"20261005T000000Z-" + shortIdentity(identity) + "-002",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("kept %v, want %v", names, want)
	}
}
