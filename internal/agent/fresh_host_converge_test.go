package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// freshHost simulates the machine agent-a was on 2026-10-06: a Debian VM with
// its OS packages provisioned and nothing of the fleet installed. Huyang and
// the steward run through their real adapters against a real filesystem; the
// host's programs and systemd are answered here.
type freshHost struct {
	t            *testing.T
	home         string
	huyangActive bool
	t3Installed  bool
}

func (h *freshHost) run(command procx.Command) procx.Result {
	argv := command.Argv
	last := argv[len(argv)-1]
	if filepath.IsAbs(argv[0]) {
		body, err := os.ReadFile(argv[0])
		if err != nil {
			return procx.Result{Code: -1, Err: errors.New("fork/exec " + argv[0] + ": no such file or directory")}
		}
		switch {
		case last == "version" && string(body) == stewardTestBinary:
			return procx.Result{Stdout: stewardTestReport}
		case last == "check" && h.t3Installed:
			return procx.Result{Stdout: stewardTestCheck}
		case last == "check":
			return procx.Result{Code: 1, Stderr: "dial tcp 127.0.0.1:7391: connection refused"}
		}
		return procx.Result{Code: 1, Stderr: "no journal"}
	}
	checked := func(unit string) bool {
		switch unit {
		case "huyang.service":
			return h.huyangActive
		case "t3code.service":
			return h.t3Installed
		}
		return false
	}
	switch {
	case adapterTestMatches(argv, "rev-parse", "HEAD"):
		if _, err := os.Stat(filepath.Join(argv[2], ".git")); err == nil {
			return procx.Result{Stdout: huyangPinned}
		}
		return procx.Result{Code: 128, Stderr: "fatal: not a git repository"}
	case adapterTestMatches(argv, "git", "clone"):
		candidate := last
		adapterTestWrite(h.t, filepath.Join(candidate, ".git/HEAD"), "ref: refs/heads/main\n")
	case adapterTestMatches(argv, "make", "build"):
		adapterTestWrite(h.t, filepath.Join(command.Dir, "bin/huyang"), "#!/bin/sh\n")
	case adapterTestMatches(argv, "enable", "--now", "huyang.service"):
		h.huyangActive = true
		adapterTestWrite(h.t, filepath.Join(h.home, "run/huyang/control.sock"), "")
	case adapterTestMatches(argv, "systemctl", "is-active"):
		if checked(last) {
			return procx.Result{Stdout: "active"}
		}
		return procx.Result{Code: 3, Stdout: "inactive"}
	case adapterTestMatches(argv, "systemctl", "cat"):
		return procx.Result{Code: 1, Stderr: "No files found for " + last + "."}
	case adapterTestMatches(argv, "systemctl", "show") && adapterTestMatches(argv, "LoadState"):
		state := "not-found"
		if checked(argv[3]) {
			state = "loaded"
		}
		if adapterTestMatches(argv, "--value") {
			return procx.Result{Stdout: state}
		}
		return procx.Result{Stdout: "LoadState=" + state + "\nActiveState=inactive"}
	case adapterTestMatches(argv, "npm", "ls"):
		if h.t3Installed {
			return procx.Result{Stdout: `{"dependencies": {"t3": {"version": "0.0.38"}}}`}
		}
		return procx.Result{Code: 1, Stdout: `{}`}
	case adapterTestMatches(argv, "npm", "prefix", "-g"):
		// An empty global prefix: npm has never installed anything.
		return procx.Result{Stdout: filepath.Join(h.home, ".npm-global") + "\n"}
	case adapterTestMatches(argv, "secret-tool", "lookup"):
		return procx.Result{Code: 1}
	}
	return procx.Result{}
}

// freshStandIn converges a component this test does not drive through its real
// adapter: the first apply installs it, every later one finds it converged.
// requires names a file an earlier component must already have put in place.
func freshStandIn(h *freshHost, name string, applied *[]string, requires string, install func()) Handler {
	marker := filepath.Join(h.home, "fresh-host-installed", name)
	return Handler{
		Observe: func(*Context, map[string]any) (map[string]any, error) {
			return map[string]any{"installed": pathExists(marker)}, nil
		},
		Apply: func(*Context, map[string]any, map[string]any, *[]string) (string, error) {
			*applied = append(*applied, name)
			if requires != "" && !pathExists(requires) {
				return "", Failf("%s converged before %s existed", name, requires)
			}
			if pathExists(marker) {
				return "converged", nil
			}
			adapterTestWrite(h.t, marker, "installed\n")
			if install != nil {
				install()
			}
			return "updated", nil
		},
	}
}

// A fresh host converges through every component of a release, in release
// order, with no manual step between runs except creating the one credential
// UpKeeper must never copy; until then only the component that needs it is
// blocked.
func TestFreshHostConvergesEveryComponentInOrder(t *testing.T) {
	home := t.TempDir()
	host := &freshHost{t: t, home: home}
	bin := freshTestTools(t, home, freshTestCommands...)
	huyangTestPatch(t, huyangTestProbe(true, 19))
	archive := stewardTestArchive(t, []byte(stewardTestBinary), nil, false)
	stewardTestServe(t, archive, stewardTestDigest(archive))

	steward := filepath.Join(home, ".local/bin/t3-steward")
	applied := []string{}
	components := map[string]any{}
	for _, name := range release.ComponentOrder {
		components[name] = map[string]any{}
		switch name {
		case "huyang", "t3-steward":
			continue
		case "t3", "worker-configuration":
			var install func()
			if name == "t3" {
				install = func() { host.t3Installed = true }
			}
			replaceHandler(t, name, freshStandIn(host, name, &applied, steward, install))
		default:
			replaceHandler(t, name, freshStandIn(host, name, &applied, "", nil))
		}
	}
	components[release.ToolingComponent] = validToolingSpec(t)
	components["huyang"] = huyangTestSpec()
	components["t3-steward"] = stewardTestSpec(false)
	components["t3"] = map[string]any{
		"kind": "npm-package", "package": "t3", "service": "t3code.service", "version": "0.0.38",
		"compatibility": map[string]any{"min": "0.0.38", "max": "0.0.38"},
	}
	converge := func() map[string]any {
		t.Helper()
		return Execute(freshTestContext(t, home, bin, host.run, release.ComponentOrder, components))
	}
	states := func(result map[string]any) []string {
		list := []string{}
		for _, name := range release.ComponentOrder {
			list = append(list, name+"="+resultEntry(t, result, name)["state"].(string))
		}
		return list
	}

	first := converge()
	want := []string{}
	for _, name := range release.ComponentOrder {
		state := "updated"
		if name == release.ToolingComponent {
			state = "blocked"
		}
		want = append(want, name+"="+state)
	}
	if got := states(first); !reflect.DeepEqual(got, want) || first["result"] != "blocked" {
		t.Fatalf("first converge = %v (%v), want %v", got, first["result"], want)
	}
	if message, _ := resultEntry(t, first, release.ToolingComponent)["error"].(string); !strings.Contains(message, "credential missing ov-memory API key") {
		t.Fatalf("tooling blocker = %q", message)
	}
	wantOrder := []string{}
	for _, name := range release.ComponentOrder {
		if name != release.ToolingComponent && name != "huyang" && name != "t3-steward" {
			wantOrder = append(wantOrder, name)
		}
	}
	if !reflect.DeepEqual(applied, wantOrder) {
		t.Fatalf("components applied in %v, want %v", applied, wantOrder)
	}
	for _, path := range []string{".local/share/huyang/bin/huyang", ".config/systemd/user/huyang.service", ".local/bin/t3-steward"} {
		if _, err := os.Stat(filepath.Join(home, path)); err != nil {
			t.Fatalf("the first converge did not install %s: %v", path, err)
		}
	}

	// The operator creates the credential on the host; nothing else changes.
	adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/key"), "created by hand\n")
	second := converge()
	want = []string{}
	for _, name := range release.ComponentOrder {
		state := "converged"
		if name == release.ToolingComponent {
			state = "updated"
		}
		want = append(want, name+"="+state)
	}
	if got := states(second); !reflect.DeepEqual(got, want) || second["result"] != "updated" {
		t.Fatalf("second converge = %v (%v), want %v", got, second["result"], want)
	}

	third := converge()
	for _, state := range states(third) {
		if !strings.HasSuffix(state, "=converged") {
			t.Fatalf("third converge = %v", states(third))
		}
	}
	if third["result"] != "converged" {
		t.Fatalf("third converge result = %v", third["result"])
	}
}
