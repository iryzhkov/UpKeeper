// Package prereq declares the operating-system packages each component needs
// on a host before UpKeeper can converge it, and how to install them.
//
// UpKeeper does not install OS packages on its own. The agent reports a
// missing prerequisite as a blocker with the command that installs it, and
// doctor and pull --dry-run show the same blocker before anything is changed.
// Only a host whose inventory entry opts in, and on which sudo works without a
// password, has the packages installed by the agent.
package prereq

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// Package manager families the catalogue knows how to install for.
const (
	Debian = "debian"
	Arch   = "arch"
	Darwin = "darwin"
)

// Prerequisite is one thing a component needs from the operating system.
type Prerequisite struct {
	// Name is what an operator installs, such as "python3-venv".
	Name string
	// Command must resolve on the agent's PATH.
	Command string
	// Check, when set, must also succeed once Command is found. It proves a
	// capability the command alone does not, such as Python's venv module.
	Check []string
	// Packages names the package that provides it in each family. A family
	// without an entry has no package for it, and Manual says what to do.
	Packages map[string]string
	// Manual is the remedy when no package manager of the host provides it.
	Manual string
	// Staging is true for a prerequisite that only building or staging a new
	// version needs, so that a host already converged is not asked for it.
	Staging bool
}

// Catalogue is every component's prerequisites, in convergence order. The
// agent's own component names are used as keys.
var Catalogue = map[string][]Prerequisite{
	"omarchy-setup": {git},
	"agent-tooling": {
		{Name: "mise", Command: "mise", Packages: map[string]string{Arch: "mise", Darwin: "mise"},
			Manual: "install mise (https://mise.jdx.dev/installing-mise.html), for example: curl https://mise.run | sh"},
		command("bash", "bash", "bash", "bash"),
		command("curl", "curl", "curl", "curl"),
		command("jq", "jq", "jq", "jq"),
		command("python3", "python3", "python", "python"),
		command("ssh", "openssh-client", "openssh", ""),
		command("readlink", "coreutils", "coreutils", ""),
		command("secret-tool", "libsecret-tools", "libsecret", "libsecret"),
		// Debian splits venv and ensurepip out of the interpreter; Arch and
		// Homebrew ship them with python. The OV memory server lives in a venv.
		{Name: "python3-venv", Command: "python3",
			Check:    []string{"python3", "-c", "import ensurepip, venv; ensurepip.version()"},
			Packages: map[string]string{Debian: "python3-venv", Arch: "python", Darwin: "python"},
			Staging:  true},
	},
	"huyang": {
		git,
		{Name: "go", Command: "go", Packages: map[string]string{Debian: "golang-go", Arch: "go", Darwin: "go"},
			Staging: true},
		{Name: "make", Command: "make", Packages: map[string]string{Debian: "make", Arch: "make"},
			Manual: "install the Xcode command line tools: xcode-select --install", Staging: true},
	},
}

var git = command("git", "git", "git", "git")

func command(name, debian, arch, darwin string) Prerequisite {
	packages := map[string]string{Debian: debian, Arch: arch}
	if darwin != "" {
		packages[Darwin] = darwin
	}
	return Prerequisite{Name: name, Command: name, Packages: packages}
}

// Components lists the components that declare prerequisites, sorted.
func Components() []string {
	names := make([]string, 0, len(Catalogue))
	for name := range Catalogue {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Family maps an os-release ID, or the inventory's os_id, onto a package
// manager family, or "" for one the catalogue cannot install for.
func Family(osID string) string {
	switch strings.ToLower(osID) {
	case "debian", "ubuntu", "raspbian", "linuxmint", "pop":
		return Debian
	case "arch", "archarm", "omarchy", "endeavouros", "manjaro", "cachyos":
		return Arch
	case "darwin", "macos", "osx":
		return Darwin
	}
	return ""
}

// Package is the package that provides p in family, or "".
func (p Prerequisite) Package(family string) string {
	return p.Packages[family]
}

// InstallCommand is the argument vector that installs packages in family, as
// an operator would run it, or nil when the family has no package manager the
// catalogue knows. The packages are deduplicated and sorted.
func InstallCommand(family string, packages []string) []string {
	list := PackageList(packages)
	if len(list) == 0 {
		return nil
	}
	switch family {
	case Debian:
		return append([]string{"sudo", "apt-get", "install", "-y"}, list...)
	case Arch:
		return append([]string{"sudo", "pacman", "-S", "--needed", "--noconfirm"}, list...)
	case Darwin:
		return append([]string{"brew", "install"}, list...)
	}
	return nil
}

// PackageList drops empty and repeated package names and sorts the rest.
func PackageList(packages []string) []string {
	unique := map[string]bool{}
	list := []string{}
	for _, name := range packages {
		if name != "" && !unique[name] {
			unique[name] = true
			list = append(list, name)
		}
	}
	sort.Strings(list)
	return list
}

// Remedy is the one line that tells an operator how to provide p on a host of
// family.
func (p Prerequisite) Remedy(family string) string {
	if install := InstallCommand(family, []string{p.Package(family)}); install != nil {
		return strings.Join(install, " ")
	}
	if p.Manual != "" {
		return p.Manual
	}
	return "install " + p.Name + " with the host's package manager"
}

// Present reports whether p holds for a command environment. The command is
// looked up on env's PATH, never the caller's, and a check runs under env.
func (p Prerequisite) Present(env []string, runner procx.Runner) bool {
	if p.Command != "" && procx.LookupIn(p.Command, env) == "" {
		return false
	}
	if len(p.Check) == 0 {
		return true
	}
	if runner == nil {
		runner = procx.Run
	}
	return runner(procx.Command{Argv: p.Check, Env: env, Timeout: 30 * time.Second}).OK()
}

// AgentPath is the PATH the agent runs every command with: the fleet's tool
// directories in front of whatever the process inherited. It is explicit so
// that what a non-interactive SSH session's shell startup files did or did
// not add never matters.
func AgentPath(home, inherited string) string {
	if inherited == "" {
		inherited = "/usr/local/bin:/usr/bin:/bin"
	}
	return strings.Join([]string{
		filepath.Join(home, ".local/share/mise/shims"),
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, "go/bin"),
		"/usr/local/go/bin",
		inherited,
	}, ":")
}
