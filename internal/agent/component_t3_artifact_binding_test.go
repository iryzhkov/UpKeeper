package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func t3BindingRecord(path, args string) string {
	return "{ path=" + path + " ; argv[]=" + path + " " + args + " ; ignore_errors=no ; }"
}

func TestT3BindingSupportedInvocations(t *testing.T) {
	c, _, runner, prefix := t3ArtifactHost(t, false, false, 0, 0)
	launcher := filepath.Join(prefix, "bin/t3")
	root := filepath.Join(prefix, "lib/node_modules/t3")
	native := filepath.Join(root, "node_modules/@t3code/t3-"+release.T3Target()+"/t3")
	adapterTestWrite(t, native, "native")
	mise := c.Path("owned/mise")
	adapterTestWrite(t, mise, "mise")
	shim := c.Path(".local/share/mise/shims/t3")
	if e := os.MkdirAll(filepath.Dir(shim), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(mise, shim); e != nil {
		t.Fatal(e)
	}
	cases := []struct {
		name, path, args, which string
		ok                      bool
	}{
		{"direct", launcher, "start", launcher, true},
		{"entry", filepath.Join(root, "bin/t3.js"), "serve", launcher, true},
		{"native", native, "serve --host 127.0.0.1 --port 7391 --no-browser", launcher, true},
		{"shim", shim, "serve --host 127.0.0.1 --port 7391 --no-browser", launcher, true},
		{"mise-search", mise, "exec -- t3 start", launcher, true},
		{"mise-direct", mise, "exec -- " + launcher + " start", "/foreign/t3", true},
		{"mise-native", mise, "exec -- " + native + " serve", "/foreign/t3", true},
		{"foreign-actual", mise, "exec -- /foreign/t3 start", launcher, false},
		{"mise-self", mise, "exec -- " + mise + " start", launcher, false},
		{"foreign-selection", mise, "exec -- t3 start", "/foreign/t3", false},
		{"foreign-shim-selection", shim, "start", "/foreign/t3", false},
		{"relative-selection", shim, "start", "bin/t3", false},
		{"multiline-selection", shim, "start", launcher + "\n/foreign/t3", false},
		{"mise-tool-selector", mise, "exec t3@latest -- t3 start", launcher, false},
		{"mise-cwd", mise, "exec --cd /foreign -- t3 start", launcher, false},
		{"mise-env", mise, "exec --env foreign -- t3 start", launcher, false},
		{"mise-run", mise, "run t3 start", launcher, false},
		{"mise-shim-subcommand", shim, "exec -- t3 start", launcher, false},
		{"unknown-command", launcher, "update", launcher, false},
		{"unknown-option", launcher, "serve --eval anything", launcher, false},
		{"duplicate-option", launcher, "serve --port 7391 --port 7392", launcher, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			whichCalls := 0
			runner.handle = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "ExecStart") {
					return procx.Result{Stdout: t3BindingRecord(tc.path, tc.args)}
				}
				if len(cmd.Argv) == 3 && cmd.Argv[0] == mise && cmd.Argv[1] == "which" {
					whichCalls++
					return procx.Result{Stdout: tc.which}
				}
				t.Fatalf("unexpected command %v", cmd.Argv)
				return procx.Result{}
			}
			_, e := t3UnitBinding(c, "t3code.service", prefix)
			if (e == nil) != tc.ok {
				t.Fatalf("supported=%v error=%v", tc.ok, e)
			}
			if strings.HasPrefix(tc.args, "exec -- /") && whichCalls != 0 {
				t.Fatal("queried unrelated mise selection for explicit program")
			}
		})
	}
}

func TestT3BindingDisplayParsing(t *testing.T) {
	good := t3BindingRecord("/owned/t3", "serve --host 127.0.0.1 --port 7391 --no-browser")
	for _, bad := range []string{
		"", "{ path=/owned/t3 ; }",
		strings.Replace(good, "ignore_errors=no", "ignore_errors=yes", 1),
		strings.Replace(good, "ignore_errors=no", "flags=ignore-failure", 1),
		strings.Replace(good, "argv[]=/owned/t3", "argv[]=t3", 1),
		strings.Replace(good, "argv[]=/owned/t3", "argv[]=/foreign/t3", 1),
		strings.Replace(good, "argv[]=/owned/t3", "argv[]=\"/owned/t3", 1),
		strings.Replace(good, "/owned/t3", "/owned/\\qt3", 1),
		strings.Replace(good, "/owned/t3", "/owned/\\x00t3", 1),
		strings.Replace(good, " ; }", " ; path=/foreign/t3 ; }", 1),
		strings.Replace(good, " ; }", " ; unknown=1 ; }", 1),
		good + " " + good, good + " trailing",
		strings.Replace(good, " ; }", " ; ; }", 1),
	} {
		if _, e := t3ParseUnitCommand(bad); e == nil {
			t.Fatalf("accepted malformed record %q", bad)
		}
	}
	for _, encoded := range []string{`"/owned/a b/t3"`, `/owned/a\x20b/t3`, `/owned/a\ b/t3`, `'/owned/a b/t3'`} {
		command, e := t3ParseUnitCommand("{ path=" + encoded + " ; argv[]=" + encoded + " serve --host \"127.0.0.1\" ; ignore_errors=no ; }")
		if e != nil || command.Path != "/owned/a b/t3" || command.Argv[2] != "--host" {
			t.Fatalf("escape %s: %+v %v", encoded, command, e)
		}
	}
}

func TestT3BindingForeignMiseBeforeEffects(t *testing.T) {
	for _, forceBadPID := range []bool{false, true} {
		t.Run(fmt.Sprint(forceBadPID), func(t *testing.T) {
			c, spec, runner, prefix := t3ArtifactHost(t, false, false, 0, 0)
			root := filepath.Join(prefix, "lib/node_modules/t3")
			old, _ := release.T3TreeDigest(root)
			mise := c.Path("owned/mise")
			adapterTestWrite(t, mise, "mise")
			original := runner.handle
			restarts := 0
			activated := false
			runner.handle = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "ExecStart") {
					return procx.Result{Stdout: t3BindingRecord(mise, "exec -- /foreign/t3 start")}
				}
				if len(cmd.Argv) == 3 && cmd.Argv[0] == mise && cmd.Argv[1] == "which" {
					return procx.Result{Stdout: filepath.Join(prefix, "bin/t3")}
				}
				if adapterTestMatches(cmd.Argv, "MainPID") && forceBadPID && restarts > 0 {
					return procx.Result{Stdout: "999"}
				}
				if adapterTestMatches(cmd.Argv, "restart") {
					restarts++
					now, _ := release.T3TreeDigest(root)
					activated = now != old
				}
				return original(cmd)
			}
			before, _ := t3Observe(c, spec)
			actions := []string{}
			_, e := t3Apply(c, spec, before, &actions)
			now, _ := release.T3TreeDigest(root)
			if e == nil || restarts != 0 || activated || now != old {
				t.Fatalf("foreign invocation effects: %v restarts=%d activated=%v preserved=%v", e, restarts, activated, now == old)
			}
		})
	}
}

func TestT3BindingRuntimeMetadataRecovery(t *testing.T) {
	c, spec, runner, prefix := t3ArtifactHost(t, false, true, 0, 0)
	root := filepath.Join(prefix, "lib/node_modules/t3")
	old, _ := release.T3TreeDigest(root)
	link := filepath.Join(prefix, "bin/t3")
	oldLink, _ := os.Readlink(link)
	oldMode, _ := os.Stat(filepath.Join(root, "bin/t3.js"))
	original := runner.handle
	restarts := 0
	idleChecks := 0
	runner.handle = func(cmd procx.Command) procx.Result {
		if cmd.Argv[len(cmd.Argv)-1] == "check" && restarts == 1 {
			idleChecks++
		}
		if adapterTestMatches(cmd.Argv, "ExecStart") {
			path := filepath.Join(prefix, "bin/t3")
			return procx.Result{Stdout: fmt.Sprintf("{ path=%s ; argv[]=%s start ; ignore_errors=no ; start_time=[Mon 2026-10-05 00:00:0%d UTC] ; stop_time=[n/a] ; pid=%d ; code=(null) ; status=0/0 }", path, path, restarts, 123+restarts)}
		}
		if adapterTestMatches(cmd.Argv, "restart") {
			restarts++
		}
		return original(cmd)
	}
	before, _ := t3Observe(c, spec)
	actions := []string{}
	_, e := t3Apply(c, spec, before, &actions)
	var rollback *RolledBack
	now, _ := release.T3TreeDigest(root)
	newLink, _ := os.Readlink(link)
	newMode, _ := os.Stat(filepath.Join(root, "bin/t3.js"))
	if restarts != 2 || idleChecks < 2 || !errors.As(e, &rollback) || now != old || newLink != oldLink || newMode.Mode() != oldMode.Mode() {
		t.Fatalf("owned recovery denied: %v restarts=%d restored=%v", e, restarts, now == old)
	}
}

func TestT3BindingRuntimeRecoveryIdleRefusal(t *testing.T) {
	c, spec, runner, prefix := t3ArtifactHost(t, false, true, 0, 0)
	root := filepath.Join(prefix, "lib/node_modules/t3")
	old, _ := release.T3TreeDigest(root)
	original := runner.handle
	restarts := 0
	idleChecksAfterFailure := 0
	runner.handle = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "ExecStart") {
			path := filepath.Join(prefix, "bin/t3")
			return procx.Result{Stdout: fmt.Sprintf("{ path=%s ; argv[]=%s start ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=%d ; code=exited ; status=1/FAILURE }", path, path, 123+restarts)}
		}
		if adapterTestMatches(cmd.Argv, "restart") {
			restarts++
		}
		if cmd.Argv[len(cmd.Argv)-1] == "check" && restarts == 1 {
			idleChecksAfterFailure++
			// Allow the independent pre-restoration proof, refuse the new proof
			// after restoration. Runtime normalization cannot waive this boundary.
			digest, _ := release.T3TreeDigest(root)
			if digest == old {
				return procx.Result{Code: 1, Stderr: "idle unavailable"}
			}
		}
		return original(cmd)
	}
	before, _ := t3Observe(c, spec)
	actions := []string{}
	_, e := t3Apply(c, spec, before, &actions)
	var rollback *RolledBack
	now, _ := release.T3TreeDigest(root)
	if e == nil || errors.As(e, &rollback) || restarts != 1 || idleChecksAfterFailure < 2 || now != old {
		t.Fatalf("idle guard bypassed: %v restarts=%d checks=%d restored=%v", e, restarts, idleChecksAfterFailure, now == old)
	}
}

func TestT3BindingConfigurationChangeBeforeEffects(t *testing.T) {
	for _, when := range []string{"staged", "activated", "restoration"} {
		t.Run(when, func(t *testing.T) {
			c, spec, runner, prefix := t3ArtifactHost(t, false, false, 0, 0)
			root := filepath.Join(prefix, "lib/node_modules/t3")
			old, _ := release.T3TreeDigest(root)
			original := runner.handle
			changed := false
			restarts := 0
			runner.handle = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "ExecStart") {
					port := "7391"
					if changed {
						port = "7392"
					}
					return procx.Result{Stdout: t3BindingRecord(filepath.Join(prefix, "bin/t3"), "serve --host 127.0.0.1 --port "+port+" --no-browser")}
				}
				r := original(cmd)
				if adapterTestMatches(cmd.Argv, t3QualificationSupervisorArg) {
					_, e := os.Stat(filepath.Join(root, "node_modules"))
					if (when == "staged" && e != nil) || (when == "activated" && e == nil) {
						changed = true
					}
				}
				if adapterTestMatches(cmd.Argv, "restart") {
					restarts++
					if when == "restoration" {
						changed = true
						return procx.Result{Code: 1, Stderr: "injected owned restart failure"}
					}
				}
				return r
			}
			before, _ := t3Observe(c, spec)
			actions := []string{}
			_, e := t3Apply(c, spec, before, &actions)
			want := 0
			if when == "restoration" {
				want = 1
			}
			now, _ := release.T3TreeDigest(root)
			if e == nil || restarts != want || now != old {
				t.Fatalf("changed configuration effects: %v restarts=%d restored=%v", e, restarts, now == old)
			}
		})
	}
}
