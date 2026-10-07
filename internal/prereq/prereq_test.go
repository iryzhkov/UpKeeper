package prereq

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func TestEveryFleetOperatingSystemHasAPackageFamily(t *testing.T) {
	for osID, family := range map[string]string{
		"debian": Debian, "ubuntu": Debian, "arch": Arch, "archarm": Arch,
		"omarchy": Arch, "darwin": Darwin, "plan9": "",
	} {
		if got := Family(osID); got != family {
			t.Errorf("Family(%q) = %q, want %q", osID, got, family)
		}
	}
}

func TestTheVirtualenvModuleIsNamedTheWayEachFamilyInstallsIt(t *testing.T) {
	var venv Prerequisite
	for _, item := range Catalogue["agent-tooling"] {
		if item.Name == "python3-venv" {
			venv = item
		}
	}
	if !venv.Staging || len(venv.Check) == 0 {
		t.Fatalf("python3-venv = %+v", venv)
	}
	for family, want := range map[string]string{
		Debian: "sudo apt-get install -y python3-venv",
		Arch:   "sudo pacman -S --needed --noconfirm python",
		Darwin: "brew install python",
		"":     "install python3-venv with the host's package manager",
	} {
		if got := venv.Remedy(family); got != want {
			t.Errorf("Remedy(%q) = %q, want %q", family, got, want)
		}
	}
}

func TestInstallCommandsAreDeduplicatedAndSorted(t *testing.T) {
	got := InstallCommand(Debian, []string{"make", "", "git", "make"})
	if want := []string{"sudo", "apt-get", "install", "-y", "git", "make"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("InstallCommand = %v", got)
	}
	if InstallCommand(Debian, []string{""}) != nil || InstallCommand("", []string{"git"}) != nil {
		t.Fatal("an empty or unknown installation produced a command")
	}
}

func TestPresenceIsJudgedOnTheGivenPathAndCheck(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin}
	calls := []string{}
	ok := true
	runner := func(command procx.Command) procx.Result {
		calls = append(calls, strings.Join(command.Argv, " "))
		if !reflect.DeepEqual(command.Env, env) {
			t.Errorf("the check ran with %v", command.Env)
		}
		if ok {
			return procx.Result{}
		}
		return procx.Result{Code: 1}
	}
	venv := Prerequisite{Name: "python3-venv", Command: "python3", Check: []string{"python3", "-c", "pass"}}
	if !venv.Present(env, runner) {
		t.Fatal("a passing check was reported missing")
	}
	ok = false
	if venv.Present(env, runner) {
		t.Fatal("a failing check was reported present")
	}
	if (Prerequisite{Name: "jq", Command: "jq"}).Present(env, runner) {
		t.Fatal("a program outside the PATH was reported present")
	}
	if len(calls) != 2 {
		t.Fatalf("checks run = %v; a missing command must not be checked", calls)
	}
}

func TestTheAgentPathPutsTheHostsToolsFirst(t *testing.T) {
	got := AgentPath("/home/igor", "/usr/bin:/bin")
	want := "/home/igor/.local/share/mise/shims:/home/igor/.local/bin:/home/igor/go/bin:/usr/local/go/bin:/usr/bin:/bin"
	if got != want {
		t.Fatalf("AgentPath = %q", got)
	}
	if !strings.HasSuffix(AgentPath("/h", ""), ":/usr/local/bin:/usr/bin:/bin") {
		t.Fatal("an empty inherited PATH has no system fallback")
	}
}
