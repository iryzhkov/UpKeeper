package agent

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/prereq"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/pelletier/go-toml/v2"
)

// A component can be blocked rather than failed: it needs something only the
// host's operator can provide, an OS package or a per-host credential that
// UpKeeper must never copy. A blocked component changes nothing, tells the
// operator exactly what to do, and does not stop the components after it;
// only the ones that really depend on it are skipped.

// credential is one per-host secret a component needs. Present must never
// read the secret into anything that is reported.
type credential struct {
	Name    string
	Present func(c *Context) bool
	Create  string
}

// componentCredentials declares the credentials each component needs.
var componentCredentials = map[string][]credential{
	release.ToolingComponent: {{
		Name:    "ov-memory API key",
		Present: ovMemoryCredentialPresent,
		Create: "create it on the host: umask 077; mkdir -p ~/.config/ov-mcp; " +
			"cat > ~/.config/ov-mcp/key (paste the OpenViking API key, then Ctrl-D); " +
			"or store it in the keyring: secret-tool store --label='OpenViking API key' service openviking key api",
	}},
}

// componentDependencies names, for each component, the earlier components it
// cannot be converged without. A component that is blocked, or skipped for
// one of those, skips its dependents. Failures still stop the whole host.
var componentDependencies = map[string][]string{
	// The T3 adapter proves its pair through the installed steward binary.
	"t3": {"t3-steward"},
	// Both configure the steward that t3-steward installs.
	"worker-configuration": {"t3-steward"},
	fleet.Component:        {"t3-steward"},
}

// componentPrerequisites returns what a component needs from the OS. A test
// can replace it.
var componentPrerequisites = func(name string) []prereq.Prerequisite {
	return prereq.Catalogue[name]
}

// blockedDependency names the earlier component that keeps name from running,
// or "".
func blockedDependency(name string, unavailable map[string]bool) string {
	for _, dependency := range componentDependencies[name] {
		if unavailable[dependency] {
			return dependency
		}
	}
	return ""
}

// requirementsBlock checks a component's OS prerequisites and credentials
// before it runs. It returns "" when the component may run, and otherwise the
// blocker, which names every missing item and how to provide it. On a host that
// opted in, missing OS packages are installed first when sudo needs no password.
func requirementsBlock(c *Context, name string, spec map[string]any, actions *[]string) string {
	return requirementsBlockForStaging(c, name, spec, actions, false)
}

// requirementsBlockForStaging can require build tools unconditionally once an
// adapter has selected a build. A transient service observation is no longer
// evidence that staging-only prerequisites are unnecessary.
func requirementsBlockForStaging(c *Context, name string, spec map[string]any, actions *[]string, forceStaging bool) string {
	family := prereq.Family(c.Host.OSID)
	missing := missingPrerequisites(c, name, spec, forceStaging)
	problems := []string{}
	if len(missing) > 0 && c.Host.InstallOSPackages && !c.DryRun {
		var refused string
		missing, refused = installPrerequisites(c, name, spec, family, missing, actions, forceStaging)
		if refused != "" {
			problems = append(problems, refused)
		}
	}
	if len(missing) > 0 {
		names := []string{}
		remedies := []string{}
		packages := []string{}
		for _, item := range missing {
			names = append(names, item.Name)
			if item.Minimum == "" && prereq.InstallCommand(family, []string{item.Package(family)}) != nil {
				packages = append(packages, item.Package(family))
			} else {
				remedies = append(remedies, item.Name+": "+item.Remedy(family))
			}
		}
		if install := prereq.InstallCommand(family, packages); install != nil {
			remedies = append([]string{"install with: " + strings.Join(install, " ")}, remedies...)
		}
		problems = append([]string{"missing OS prerequisite " + strings.Join(names, ", ") +
			"; " + strings.Join(remedies, "; ")}, problems...)
	}
	for _, needed := range componentCredentials[name] {
		if !needed.Present(c) {
			problems = append(problems, "credential missing "+needed.Name+"; "+needed.Create)
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return "blocked: " + strings.Join(problems, "; ")
}

// missingPrerequisites lists the OS prerequisites a component needs now.
func missingPrerequisites(c *Context, name string, spec map[string]any, forceStaging bool) []prereq.Prerequisite {
	missing := []prereq.Prerequisite{}
	// Whether the component stages is only asked once something it needs only
	// for staging is in question, so a provisioned host pays nothing for it.
	staging, known := forceStaging, forceStaging
	needed := func(item prereq.Prerequisite) bool {
		if !item.Staging {
			return true
		}
		if !known {
			staging, known = componentStages(c, name, spec), true
		}
		return staging
	}
	for _, item := range componentPrerequisites(name) {
		if item.Command != "" && procx.LookupIn(item.Command, c.Env) == "" {
			if needed(item) {
				missing = append(missing, item)
			}
			continue
		}
		if (len(item.Check) > 0 || len(item.Version) > 0) && needed(item) && !item.Present(c.Env, c.Runner) {
			missing = append(missing, item)
		}
	}
	return missing
}

// componentStages reports whether a component would have to build or stage a
// new version, which is when its staging-only prerequisites are needed. An
// answer that cannot be had is yes.
func componentStages(c *Context, name string, spec map[string]any) bool {
	switch name {
	case release.ToolingComponent:
		base, err := agentenv.TargetPath(c.Home, ".local/share/upkeeper-tools")
		if err != nil {
			return true
		}
		_, err = os.Stat(filepath.Join(base, "releases", toolingIdentity(spec), ".complete"))
		return err != nil
	case "huyang":
		return huyangWouldBuild(c, spec)
	}
	return true
}

// installPrerequisites installs missing OS packages on a host that opted in,
// with sudo that needs no password. It returns what is still missing and, when
// it could not install at all, why.
func installPrerequisites(
	c *Context, name string, spec map[string]any, family string,
	missing []prereq.Prerequisite, actions *[]string, forceStaging bool,
) ([]prereq.Prerequisite, string) {
	packages := []string{}
	for _, item := range missing {
		packages = append(packages, item.Package(family))
	}
	install := prereq.InstallCommand(family, packages)
	if install == nil {
		// Nothing missing has a package here; the remedies say what to do.
		return missing, ""
	}
	if install[0] != "sudo" {
		return missing, "host opted in to OS package installation, but UpKeeper installs packages only with apt-get or pacman"
	}
	if sudo := c.Runner(procx.Command{Argv: []string{"sudo", "-n", "true"}, Env: c.Env, Timeout: 15 * time.Second}); !sudo.OK() {
		// A program that could not be started at all is not there.
		if sudo.Err != nil && !sudo.TimedOut {
			return missing, "host opted in to OS package installation, but sudo is not installed; " +
				"install the packages as root, or install sudo and allow it without a password"
		}
		return missing, "host opted in to OS package installation, but sudo needs a password; " +
			"install the packages by hand or configure passwordless sudo"
	}
	argv := append([]string{"sudo", "-n"}, install[1:]...)
	if family == prereq.Debian {
		argv = append([]string{"sudo", "-n", "env", "DEBIAN_FRONTEND=noninteractive"}, install[1:]...)
	}
	if result := c.Run(argv, 15*time.Minute, ""); !result.OK {
		tail := result.Output
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return missing, "OS package installation failed: " + tail
	}
	*actions = append(*actions, "installed OS packages: "+strings.Join(prereq.PackageList(packages), " "))
	return missingPrerequisites(c, name, spec, forceStaging), ""
}

// ovMemoryCredentialPresent reports whether the ov-memory server will find an
// OpenViking API key. It looks where the server looks, in the server's own
// order: OV_API_KEY, api_key in its config file, the keyring, then the 0600
// key file. The environment is the one the server is registered with.
func ovMemoryCredentialPresent(c *Context) bool {
	env := c.Env
	if rendered, err := agentenv.MCPEnvironment(c.Spec("agent-environment"), c.Home, "ov-memory", c.Env); err == nil {
		env = rendered
	}
	lookup := func(key string) string {
		value := ""
		for _, entry := range env {
			if found, ok := strings.CutPrefix(entry, key+"="); ok {
				value = found
			}
		}
		return value
	}
	if strings.TrimSpace(lookup("OV_API_KEY")) != "" {
		return true
	}
	config := lookup("OV_CONFIG")
	if config == "" {
		base := lookup("XDG_CONFIG_HOME")
		if base == "" {
			base = c.Path(".config")
		}
		config = filepath.Join(base, "ov-mcp", "config.toml")
	}
	if configHasKey(c, config) {
		return true
	}
	keyFile := lookup("OV_KEY_FILE")
	if keyFile == "" {
		keyFile = "~/.config/ov-mcp/key"
	}
	if rest, found := strings.CutPrefix(keyFile, "~/"); found {
		keyFile = c.Path(rest)
	}
	if data, ok := readSmallRegularFile(keyFile, 64<<10); ok && len(bytes.TrimSpace(data)) > 0 {
		return true
	}
	if procx.LookupIn("secret-tool", env) == "" {
		return false
	}
	result := c.Runner(procx.Command{
		Argv: []string{"secret-tool", "lookup", "service", "openviking", "key", "api"},
		Env:  env, Timeout: 10 * time.Second,
	})
	return result.OK() && strings.TrimSpace(result.Stdout) != ""
}

// configHasKey reports whether a valid TOML config assigns a non-empty string
// api_key at its top level, just as the server's TOML loader requires.
func configHasKey(c *Context, path string) bool {
	if rest, found := strings.CutPrefix(path, "~/"); found {
		path = c.Path(rest)
	}
	data, ok := readSmallRegularFile(path, 1<<20)
	if !ok {
		return false
	}
	var document map[string]any
	if err := toml.Unmarshal(data, &document); err != nil {
		return false
	}
	key, _ := document["api_key"].(string)
	return strings.TrimSpace(key) != ""
}

// readSmallRegularFile reads a regular file of at most limit bytes. A FIFO, a
// device or a directory is never opened for reading, so a check cannot hang or
// read without bound; ok is false for anything else.
func readSmallRegularFile(path string, limit int64) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, false
	}
	handle, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer handle.Close()
	if opened, err := handle.Stat(); err != nil || !opened.Mode().IsRegular() {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(handle, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, false
	}
	return data, true
}
