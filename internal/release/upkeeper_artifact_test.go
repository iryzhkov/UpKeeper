package release_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
)

func programPinFixture() map[string]any {
	return map[string]any{
		"kind":          "upkeeper-binary",
		"repository":    "iryzhkov/UpKeeper",
		"tag":           "v0.1.31",
		"source_commit": strings.Repeat("a", 40),
		"toolchain":     "go1.24.13",
		"assets": map[string]any{
			"linux-amd64": map[string]any{
				"asset": "upkeeper-linux-amd64", "sha256": strings.Repeat("1", 64),
				"size": number("9437184"),
			},
			"linux-arm64": map[string]any{
				"asset": "upkeeper-linux-arm64", "sha256": strings.Repeat("2", 64),
				"size": number("9043968"),
			},
		},
	}
}

func withProgramPin(t *testing.T, change func(pin map[string]any)) map[string]any {
	t.Helper()
	manifest := pairedManifestFixture(t)
	pin := programPinFixture()
	if change != nil {
		change(pin)
	}
	components(manifest)[release.ProgramComponent] = pin
	return manifest
}

// Adding the pin may not invalidate a single release already published: they
// keep loading, converging and rolling back exactly as before.
func TestAReleaseWithAndWithoutTheProgramPinBothValidate(t *testing.T) {
	acceptAnyEnvironment(t)
	without, err := release.Validate(pairedManifestFixture(t))
	if err != nil {
		t.Fatalf("a release without the program pin was refused: %v", err)
	}
	if pin, err := without.ProgramPin(); pin != nil || err != nil {
		t.Errorf("a release without a pin reports %v, %v", pin, err)
	}
	with, err := release.Validate(withProgramPin(t, nil))
	if err != nil {
		t.Fatalf("a release with the program pin was refused: %v", err)
	}
	pin, err := with.ProgramPin()
	if err != nil || pin == nil {
		t.Fatalf("ProgramPin = %v, %v", pin, err)
	}
	if pin.VersionLine() != "upkeeper v0.1.31 (commit "+strings.Repeat("a", 40)+")" {
		t.Errorf("version line %q", pin.VersionLine())
	}
	if got := pin.URL("", "linux-arm64"); got !=
		"https://github.com/iryzhkov/UpKeeper/releases/download/v0.1.31/upkeeper-linux-arm64" {
		t.Errorf("URL %q", got)
	}
	if pin.Assets["linux-amd64"].Size != 9437184 {
		t.Errorf("size %d", pin.Assets["linux-amd64"].Size)
	}
	if with.Digest() == without.Digest() {
		t.Error("adding the program pin did not change the release identity")
	}
	if len(release.Describe(with)) != len(release.Describe(without))+1 {
		t.Error("the capture summary does not describe the program pin")
	}
}

// The agent converges components; the pin is applied by self-pull before any
// of them. It must never become something --components can select.
func TestTheProgramPinIsNotAConvergedComponent(t *testing.T) {
	for _, name := range release.ComponentOrder {
		if name == release.ProgramComponent {
			t.Fatal("the program pin is in the convergence order")
		}
	}
	if !release.OptionalComponents[release.ProgramComponent] {
		t.Error("the program pin must be optional, or older releases stop loading")
	}
}

// A legacy release has an exact component set, and a pin is not part of it.
func TestALegacyReleaseCannotCarryAProgramPin(t *testing.T) {
	acceptAnyEnvironment(t)
	manifest := pairedManifestFixture(t)
	all := components(manifest)
	kept := map[string]any{release.ProgramComponent: programPinFixture()}
	for _, name := range release.LegacyComponentOrder {
		kept[name] = all[name]
	}
	manifest["components"] = kept
	if _, err := release.Validate(manifest); err == nil {
		t.Fatal("a legacy release with a program pin was accepted")
	}
}

func TestProgramPinRefusals(t *testing.T) {
	acceptAnyEnvironment(t)
	asset := func(pin map[string]any, target string) map[string]any {
		return pin["assets"].(map[string]any)[target].(map[string]any)
	}
	cases := []struct {
		name     string
		mutate   func(map[string]any)
		expected string
	}{
		{"an extra field", func(p map[string]any) { p["url"] = "https://example.invalid" }, "must name exactly"},
		{"a missing field", func(p map[string]any) { delete(p, "toolchain") }, "must name exactly"},
		{"another kind", func(p map[string]any) { p["kind"] = "github-release" }, "upkeeper.kind"},
		{"another repository", func(p map[string]any) { p["repository"] = "someone/UpKeeper" }, "upkeeper.repository"},
		{"a tag without v", func(p map[string]any) { p["tag"] = "0.1.31" }, "upkeeper.tag"},
		{"a tag that is a ref", func(p map[string]any) { p["tag"] = "v0.1.31/../main" }, "upkeeper.tag"},
		{"a short commit", func(p map[string]any) { p["source_commit"] = "abc1234" }, "upkeeper.source_commit"},
		{"an uppercase commit", func(p map[string]any) { p["source_commit"] = strings.Repeat("A", 40) }, "upkeeper.source_commit"},
		{"a dirty commit", func(p map[string]any) { p["source_commit"] = strings.Repeat("a", 40) + "-dirty" }, "upkeeper.source_commit"},
		{"a toolchain that is not Go", func(p map[string]any) { p["toolchain"] = "1.24" }, "upkeeper.toolchain"},
		{"a missing architecture", func(p map[string]any) { delete(p["assets"].(map[string]any), "linux-arm64") }, "upkeeper.assets"},
		{"an extra architecture", func(p map[string]any) {
			p["assets"].(map[string]any)["darwin-arm64"] = asset(p, "linux-arm64")
		}, "upkeeper.assets"},
		{"another asset name", func(p map[string]any) { asset(p, "linux-amd64")["asset"] = "upkeeper" }, "upkeeper.assets.linux-amd64.asset"},
		{"a swapped asset", func(p map[string]any) { asset(p, "linux-amd64")["asset"] = "upkeeper-linux-arm64" }, "upkeeper.assets.linux-amd64.asset"},
		{"a short digest", func(p map[string]any) { asset(p, "linux-arm64")["sha256"] = "00" }, "upkeeper.assets.linux-arm64.sha256"},
		{"an extra asset field", func(p map[string]any) { asset(p, "linux-arm64")["url"] = "x" }, "must name asset, sha256 and size"},
		{"a zero size", func(p map[string]any) { asset(p, "linux-amd64")["size"] = number("0") }, "size"},
		{"a negative size", func(p map[string]any) { asset(p, "linux-amd64")["size"] = number("-5") }, "size"},
		{"a fractional size", func(p map[string]any) { asset(p, "linux-amd64")["size"] = number("5.5") }, "size"},
		{"an exponent size", func(p map[string]any) { asset(p, "linux-amd64")["size"] = number("5e3") }, "size"},
		{"a string size", func(p map[string]any) { asset(p, "linux-amd64")["size"] = "5" }, "size"},
		{"a size beyond the limit", func(p map[string]any) { asset(p, "linux-amd64")["size"] = number("268435457") }, "size"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := release.Validate(withProgramPin(t, test.mutate))
			if err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("error %v, want %q", err, test.expected)
			}
		})
	}
}

// symlinkTo marks a gitRepository change that makes the path a symbolic link to
// the rest of the content.
const symlinkTo = "\x00symlink:"

// gitRepository builds a throwaway repository with one commit per change and
// returns its root and the commits, oldest first.
func gitRepository(t *testing.T, changes ...map[string]string) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	git := func(arguments ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{
			"-C", root, "-c", "gc.auto=0", "-c", "maintenance.auto=false",
			"-c", "commit.gpgsign=false", "-c", "user.name=test", "-c", "user.email=test@invalid",
		}, arguments...)...)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet", "--initial-branch=main")
	commits := []string{}
	for _, change := range changes {
		for path, content := range change {
			if target, ok := strings.CutPrefix(content, symlinkTo); ok {
				writeFile(t, filepath.Join(root, path), nil)
				if err := os.Remove(filepath.Join(root, path)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(root, path)); err != nil {
					t.Fatal(err)
				}
				continue
			}
			writeFile(t, filepath.Join(root, path), []byte(content))
		}
		git("add", "--all")
		git("commit", "--quiet", "--allow-empty", "-m", "change")
		commits = append(commits, git("rev-parse", "HEAD"))
	}
	return root, commits
}

// The pin is checked against the program the commit holds, not against the
// commit itself: main advances on every release capture without the program
// changing, and each of those commits keeps the same pin.
func TestTheProgramPinMustMatchTheProgramOfTheCommit(t *testing.T) {
	root, commits := gitRepository(t,
		map[string]string{"go.mod": "module x\n", "go.sum": "", "internal/a.go": "package a\n", "cmd/main.go": "package main\n"},
		map[string]string{"desired/release.json": "{}\n", "docs/a.md": "words\n"},
		map[string]string{"go.sum": "example.invalid/x v1.0.0 h1:x=\n"},
		map[string]string{"internal/a.go": "package a // changed\n"},
		map[string]string{"cmd/main.go": "package main // changed\n"},
		map[string]string{"go.mod": "module y\n"},
	)
	if err := release.CheckProgramPin(root, commits[0], commits[1], nil); err != nil {
		t.Errorf("a release-only commit refused its program's pin: %v", err)
	}
	if err := release.CheckProgramPin(root, commits[0], commits[0], nil); err != nil {
		t.Errorf("the pin's own commit was refused: %v", err)
	}
	for index, path := range []string{"go.sum", "internal", "cmd", "go.mod"} {
		err := release.CheckProgramPin(root, commits[1], commits[index+2], nil)
		if err == nil || !strings.Contains(err.Error(), "differs") {
			t.Errorf("a change to %s was accepted: %v", path, err)
		}
	}
	if err := release.CheckProgramPin(root, strings.Repeat("e", 40), "HEAD", nil); err == nil ||
		!strings.Contains(err.Error(), "not in this repository's history") {
		t.Errorf("a commit the repository does not have was accepted: %v", err)
	}
	if err := release.CheckProgramPin(root, "HEAD", "HEAD", nil); err == nil {
		t.Error("a symbolic source commit was accepted")
	}
}

// Tests are not build inputs: a commit that only changes a test or a test
// fixture builds the same binary, so it must keep the program's identity.
// Otherwise a test-only fix would make every host refuse to converge until
// it ran a binary built from the new commit.
func TestTestOnlyChangesKeepTheProgramIdentity(t *testing.T) {
	root, commits := gitRepository(t,
		map[string]string{
			"go.mod": "module x\n", "go.sum": "",
			"internal/a/a.go": "package a\n", "internal/a/a_test.go": "package a\n",
			"internal/a/testdata/fixture.json": "{}\n", "cmd/main.go": "package main\n",
			"cmd/main_test.go": "package main\n",
		},
		map[string]string{"internal/a/a_test.go": "package a // changed\n"},
		map[string]string{"internal/a/b_test.go": "package a\n", "cmd/main_test.go": "package main // changed\n"},
		map[string]string{"internal/a/testdata/fixture.json": "{\"changed\": true}\n"},
		map[string]string{"internal/a/testdata/nested/new.txt": "new\n"},
		map[string]string{"internal/a/a.go": "package a // changed\n"},
		map[string]string{"go.sum": "example.invalid/x v1.0.0 h1:x=\n"},
	)
	for index, what := range []string{
		"a changed test", "a new test and a changed command test",
		"a changed test fixture", "a new nested test fixture",
	} {
		if err := release.CheckProgramPin(root, commits[0], commits[index+1], nil); err != nil {
			t.Errorf("%s changed the program: %v", what, err)
		}
	}
	if err := release.CheckProgramPin(root, commits[4], commits[5], nil); err == nil ||
		!strings.Contains(err.Error(), "differs") {
		t.Errorf("a non-test change under internal/ kept the program: %v", err)
	}
	if err := release.CheckProgramPin(root, commits[5], commits[6], nil); err == nil ||
		!strings.Contains(err.Error(), "differs") {
		t.Errorf("a go.sum change kept the program: %v", err)
	}
}

// A directory's name does not make its files test-only. Go skips testdata in
// wildcard package discovery, but a production package may still import a
// package there explicitly, and that package is compiled into the binary.
func TestAnImportedHelperUnderTestdataIsPartOfTheProgram(t *testing.T) {
	root, commits := gitRepository(t,
		map[string]string{
			"go.mod": "module x\n\ngo 1.21\n", "go.sum": "",
			"cmd/main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\t\"x/internal/testdata/helper\"\n)\n\n" +
				"func main() { fmt.Println(helper.Value) }\n",
			"internal/testdata/helper/helper.go": "package helper\n\nconst Value = 1\n",
		},
		map[string]string{"internal/testdata/helper/helper.go": "package helper\n\nconst Value = 2\n"},
	)
	if output, ok := goRun(t, root); ok && output != "2" {
		t.Fatalf("the fixture does not compile the helper into the program: %q", output)
	}
	if err := release.CheckProgramPin(root, commits[0], commits[1], nil); err == nil ||
		!strings.Contains(err.Error(), "differs") {
		t.Errorf("a changed helper the program imports kept the program: %v", err)
	}
}

// An embedded file is compiled into the binary whatever its name or
// directory, so a change to it changes the program.
func TestAnEmbeddedTestFixtureIsPartOfTheProgram(t *testing.T) {
	source := "package a\n\nimport _ \"embed\"\n\n%s\nvar Data string\n"
	for _, test := range []struct {
		name, directive, path string
	}{
		{"an embedded fixture", "//go:embed testdata/config.json", "internal/a/testdata/config.json"},
		{"an embedded fixture directory", "//go:embed testdata", "internal/a/testdata/nested/config.json"},
		{"a quoted embed pattern", "//go:embed \"testdata/*.json\"", "internal/a/testdata/config.json"},
		{"an embedded test-named file", "//go:embed all:golden_test.go", "internal/a/golden_test.go"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, commits := gitRepository(t,
				map[string]string{
					"go.mod": "module x\n", "go.sum": "", "cmd/main.go": "package main\n",
					"internal/a/a.go": strings.Replace(source, "%s", test.directive, 1),
					test.path:         "one\n",
				},
				map[string]string{test.path: "two\n"},
			)
			if err := release.CheckProgramPin(root, commits[0], commits[1], nil); err == nil ||
				!strings.Contains(err.Error(), "differs") {
				t.Errorf("a changed embedded file kept the program: %v", err)
			}
		})
	}
}

// Any file beside a compiled Go file can be compiled with it (assembly, cgo
// sources, syso objects), so a testdata directory that holds a package is not
// a fixture directory.
func TestAFileBesideAHelperUnderTestdataIsPartOfTheProgram(t *testing.T) {
	root, commits := gitRepository(t,
		map[string]string{
			"go.mod": "module x\n", "go.sum": "", "cmd/main.go": "package main\n",
			"internal/testdata/helper/helper.go":     "package helper\n",
			"internal/testdata/helper/value_amd64.s": "// one\n",
		},
		map[string]string{"internal/testdata/helper/value_amd64.s": "// two\n"},
	)
	if err := release.CheckProgramPin(root, commits[0], commits[1], nil); err == nil ||
		!strings.Contains(err.Error(), "differs") {
		t.Errorf("a changed file in a testdata package kept the program: %v", err)
	}
}

// The comparison must read every directive the go command reads: one in a
// file with CRLF line endings, and one in a file git's attributes call binary.
// And a fixture a cgo or assembly source can include is compiled wherever it
// lives.
func TestEveryWayAFixtureIsCompiledIsPartOfTheProgram(t *testing.T) {
	embed := "package a\n\nimport _ \"embed\"\n\n//go:embed testdata/x.txt\nvar Data string\n"
	for _, test := range []struct {
		name  string
		files map[string]string
		path  string
	}{
		{"a CRLF embed directive", map[string]string{
			"internal/a/a.go": strings.ReplaceAll(embed, "\n", "\r\n"),
		}, "internal/a/testdata/x.txt"},
		{"an embed directive in a file git treats as binary", map[string]string{
			".gitattributes": "*.go -diff\n", "internal/a/a.go": embed,
		}, "internal/a/testdata/x.txt"},
		{"a header a cgo preamble includes", map[string]string{
			"internal/a/a.go": "package a\n\n// #include \"testdata/x.h\"\nimport \"C\"\n",
		}, "internal/a/testdata/x.h"},
		{"a file an assembly source includes", map[string]string{
			"internal/a/a.go":       "package a\n",
			"internal/a/a_amd64.s":  "#include \"testdata/x.h\"\n",
			"internal/a/a_stub.go":  "package a\n\nfunc f()\n",
			"internal/a/a_other.go": "package a\n",
		}, "internal/a/testdata/x.h"},
		{"a test-named file a cgo preamble includes", map[string]string{
			"internal/a/a.go": "package a\n\n// #include \"x_test.go\"\nimport \"C\"\n",
		}, "internal/a/x_test.go"},
		{"a header a raw-string cgo import includes", map[string]string{
			"internal/a/a.go": "package a\n\n// #include \"testdata/x.inc\"\nimport `C`\n",
		}, "internal/a/testdata/x.inc"},
		{"a header outside the program paths a cgo preamble includes", map[string]string{
			"internal/a/a.go": "package a\n\n// #include \"../../include/x.inc\"\nimport \"C\"\n",
		}, "include/x.inc"},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := map[string]string{"go.mod": "module x\n", "go.sum": "", "cmd/main.go": "package main\n"}
			for name, content := range test.files {
				first[name] = content
			}
			first[test.path] = "one\n"
			root, commits := gitRepository(t, first, map[string]string{test.path: "two\n"})
			if err := release.CheckProgramPin(root, commits[0], commits[1], nil); err == nil ||
				!strings.Contains(err.Error(), "differs") {
				t.Errorf("a changed compiled fixture kept the program: %v", err)
			}
		})
	}
}

// A file named like a test is not compiled as Go, but an assembly or C source
// can still include it. Here the legacy build constraint keeps go vet and the
// go command from reading it as a test, and the assembler takes its #define.
func TestAnAssemblyIncludeOfATestNamedFileIsPartOfTheProgram(t *testing.T) {
	root, commits := gitRepository(t, map[string]string{
		"go.mod":            "module x\n\ngo 1.21\n",
		"go.sum":            "",
		"cmd/main.go":       "package main\n\nimport \"fmt\"\n\nfunc value() int\n\nfunc main() { fmt.Print(value()) }\n",
		"cmd/value_amd64.s": "#include \"textflag.h\"\n#include \"value_test.go\"\nTEXT ·value(SB), NOSPLIT, $0-8\nMOVQ $VALUE, ret+0(FP)\nRET\n",
		"cmd/value_test.go": "// +build ignore\n\n#define VALUE 1\n",
	}, map[string]string{"cmd/value_test.go": "// +build ignore\n\n#define VALUE 2\n"})
	if runtime.GOARCH == "amd64" {
		if output, ok := goRun(t, root); ok && output != "2" {
			t.Fatalf("the fixture does not assemble the included file: %q", output)
		}
	}
	if err := release.CheckProgramPin(root, commits[0], commits[1], nil); err == nil ||
		!strings.Contains(err.Error(), "differs") {
		t.Errorf("a changed file the assembly includes kept the program: %v", err)
	}
	if err := selfconv.VerifyProgram(root, commits[0], commits[1], nil); err == nil {
		t.Error("a changed file the assembly includes kept the binary's program")
	}
}

// The program is whatever the go command compiles, not what lives under
// ProgramPaths: a production package can import a package anywhere in the
// module, that package can embed its own files, and a symbolic link can make
// any file a Go source.
func TestCompiledInputsOutsideTheProgramPathsArePartOfTheProgram(t *testing.T) {
	command := func(importPath, name string) string {
		return "package main\n\nimport (\n\t\"fmt\"\n\n\t\"x/" + importPath + "\"\n)\n\n" +
			"func main() { fmt.Print(" + name + ".Value) }\n"
	}
	for _, test := range []struct {
		name  string
		files map[string]string
		path  string
	}{
		{"an imported package", map[string]string{
			"cmd/main.go":      command("helper", "helper"),
			"helper/helper.go": "package helper\n\nconst Value = 1\n",
		}, "helper/helper.go"},
		{"a file an imported package embeds", map[string]string{
			"cmd/main.go":             command("helper", "helper"),
			"helper/helper.go":        "package helper\n\nimport _ \"embed\"\n\n//go:embed assets/value.txt\nvar Value string\n",
			"helper/assets/value.txt": "1",
		}, "helper/assets/value.txt"},
		{"a file a symbolic link makes a Go source", map[string]string{
			"cmd/main.go":        command("cmd/value", "value"),
			"cmd/value/value.go": symlinkTo + "../../data/value.txt",
			"data/value.txt":     "package value\n\nconst Value = 1\n",
		}, "data/value.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := map[string]string{"go.mod": "module x\n\ngo 1.21\n", "go.sum": ""}
			for name, content := range test.files {
				first[name] = content
			}
			changed := strings.Replace(test.files[test.path], "1", "2", 1)
			root, commits := gitRepository(t, first, map[string]string{test.path: changed},
				map[string]string{"docs/a.md": "words\n", "desired/release.json": "{}\n"})
			if output, ok := goRun(t, root); ok && output != "2" {
				t.Fatalf("the fixture does not compile the changed file: %q", output)
			}
			if err := release.CheckProgramPin(root, commits[0], commits[1], nil); err == nil ||
				!strings.Contains(err.Error(), "differs") {
				t.Errorf("a changed compiled input kept the program: %v", err)
			}
			if err := selfconv.VerifyProgram(root, commits[0], commits[1], nil); err == nil {
				t.Error("a changed compiled input kept the binary's program")
			}
			// A symbolic link anywhere makes every change count, so only the
			// others show that the release intent beside them is not compiled.
			if test.path != "data/value.txt" {
				if err := release.CheckProgramPin(root, commits[1], commits[2], nil); err != nil {
					t.Errorf("a release-only change refused its program's pin: %v", err)
				}
			}
		})
	}
}

// Outside ProgramPaths, as under them, tests and files nothing compiles keep
// the program's identity, and so does a mention of cgo that is not an import.
func TestTestOnlyChangesAnywhereKeepTheProgramIdentity(t *testing.T) {
	root, commits := gitRepository(t,
		map[string]string{
			"go.mod": "module x\n", "go.sum": "", "cmd/main.go": "package main\n",
			"internal/a/a.go":                  "package a\n\nconst Letter = \"C\" + `C`\n",
			"internal/a/testdata/fixture.json": "{}\n",
			"helper/helper.go":                 "package helper\n",
			"helper/helper_test.go":            "package helper\n",
			"scripts/tool.py":                  "print(1)\n",
		},
		map[string]string{"internal/a/testdata/fixture.json": "{\"changed\": true}\n"},
		map[string]string{"helper/helper_test.go": "package helper // changed\n"},
		map[string]string{"scripts/tool.py": "print(2)\n", "docs/new.md": "new\n"},
	)
	for index, what := range []string{
		"a fixture beside a mention of cgo", "a test outside the program paths",
		"a script and a document",
	} {
		if err := release.CheckProgramPin(root, commits[index], commits[index+1], nil); err != nil {
			t.Errorf("%s changed the program: %v", what, err)
		}
	}
}

// goRun runs the fixture's command when a Go toolchain is available, proving
// that the fixture really compiles what the test says it does.
func goRun(t *testing.T, root string) (string, bool) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", false
	}
	command := exec.Command(goTool, "run", "./cmd")
	command.Dir = root
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off", "GO111MODULE=on")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./cmd: %v: %s", err, output)
	}
	return strings.TrimSpace(string(output)), true
}
