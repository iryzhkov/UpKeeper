package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

func init() {
	Register("t3", Handler{Observe: t3Observe, Apply: t3Apply})
}

// The steward's check output is the only evidence the agent has about the
// running T3: which version it serves, and how many threads are running in it.
var (
	t3ServedPattern  = regexp.MustCompile(`T3 server (\d+\.\d+\.\d+) `)
	t3ThreadsPattern = regexp.MustCompile(`shell snapshot: \d+ threads, (\d+) running`)
	// t3ExecStartPattern reads the program out of systemd's ExecStart rendering,
	// which looks like "{ path=/home/x/.local/share/mise/shims/t3 ; argv[]=... }".
	t3ExecStartPattern = regexp.MustCompile(`path=([^\s;]+)`)
)

// t3ProcRoot is where a running unit's program is read from. A test points it
// at a directory of its own; production reads the kernel's.
var t3ProcRoot = "/proc"

// t3ServiceBinary finds the program that actually runs the T3 unit and reports
// whether it is still on disk. That answer is what separates "T3 is not
// answering" from "a Node upgrade deleted T3": the steward's check fails
// identically in both cases.
//
// The unit's ExecStart is not that program on this fleet: it is the mise shim,
// a symlink to /usr/bin/mise, and both survive a Node upgrade that removes the
// Node prefix T3 lives under. So the running process is asked first, through
// /proc/<MainPID>/exe, which the kernel marks "(deleted)" once the binary is
// gone. Without a running process the ExecStart path is resolved, and when it
// resolves to mise, `mise which t3` names the real program. The mise binary
// itself is never reported as existing T3; when nothing better is known the
// answer is nil, not a guess.
func t3ServiceBinary(c *Context, service string) (any, any) {
	if service == "" {
		return nil, nil
	}
	if path, exists, known := t3RunningProgram(c, service); known {
		return path, exists
	}
	result := c.Run(
		[]string{"systemctl", "--user", "show", service, "-p", "ExecStart", "--value"},
		10*time.Second, "")
	if !result.OK {
		return nil, nil
	}
	match := t3ExecStartPattern.FindStringSubmatch(result.Output)
	if match == nil {
		return nil, nil
	}
	path := strings.ReplaceAll(match[1], "%h", c.Home)
	if !filepath.IsAbs(path) {
		return path, nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path, false
	}
	if filepath.Base(resolved) != "mise" {
		_, err = os.Stat(resolved)
		return path, err == nil
	}
	// The launcher is mise; ask it which t3 it would run. A mise that cannot
	// answer leaves the question open rather than vouching for itself.
	which := c.Run([]string{resolved, "which", "t3"}, 15*time.Second, "")
	target := firstLine(which.Output)
	if !which.OK || !filepath.IsAbs(target) {
		return path, nil
	}
	if _, err := os.Stat(target); err != nil {
		return target, false
	}
	return target, true
}

// t3RunningProgram reads the program behind the unit's main process. The
// third result is false when there is no running process or its executable
// link cannot be read; then the caller falls back to the unit definition.
func t3RunningProgram(c *Context, service string) (string, bool, bool) {
	result := c.Run(
		[]string{"systemctl", "--user", "show", service, "-p", "MainPID", "--value"},
		10*time.Second, "")
	pid, err := strconv.Atoi(strings.TrimSpace(result.Output))
	if !result.OK || err != nil || pid <= 0 {
		return "", false, false
	}
	target, err := os.Readlink(filepath.Join(t3ProcRoot, strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", false, false
	}
	if deleted := strings.TrimSuffix(target, " (deleted)"); deleted != target {
		return deleted, false, true
	}
	if filepath.Base(target) == "mise" {
		// mise is still execing the shim; the program has not started.
		return "", false, false
	}
	_, err = os.Stat(target)
	return target, err == nil, true
}

// t3StewardBinary is the steward binary this release pins, which is also the
// program that answers for the running T3.
func t3StewardBinary(c *Context) string {
	return c.Path(adapterSpecString(c.Spec("t3-steward"), "path"))
}

func t3Observe(c *Context, spec map[string]any) (map[string]any, error) {
	observed := map[string]any{
		"installed":       nil,
		"served":          nil,
		"running_threads": nil,
	}
	packaged := c.Run([]string{"npm", "ls", "-g", "--depth=0", "--json", "t3"},
		30*time.Second, "")
	if packaged.OK {
		if document, err := canonjson.DecodeObject([]byte(packaged.Output)); err == nil {
			dependencies := adapterSpecMap(document, "dependencies")
			if version := adapterSpecString(adapterSpecMap(dependencies, "t3"), "version"); version != "" {
				observed["installed"] = version
			}
		}
	}
	check := c.Run([]string{t3StewardBinary(c), "check"}, 30*time.Second, "")
	if served := t3ServedPattern.FindStringSubmatch(check.Output); served != nil {
		observed["served"] = served[1]
	}
	if threads := t3ThreadsPattern.FindStringSubmatch(check.Output); threads != nil {
		if running, err := strconv.Atoi(threads[1]); err == nil {
			observed["running_threads"] = running
		}
	}
	service := adapterSpecString(spec, "service")
	observed["service"] = service
	observed["service_active"] = c.UnitActive(service)
	observed["check_ok"] = check.OK
	if !check.OK {
		// The first line is what a refusal quotes; the tail is already bounded.
		observed["check_error"] = firstLine(check.Output)
	}
	observed["installed_path"], observed["installed_path_exists"] = t3ServiceBinary(c, service)
	observed["running_path"] = nil
	if path, exists, known := t3RunningProgram(c, service); known && exists {
		observed["running_path"] = path
	}
	observed["npm_prefix"] = nil
	if prefix := c.Run([]string{"npm", "prefix", "-g"}, 30*time.Second, ""); prefix.OK {
		if path := firstLine(prefix.Output); filepath.IsAbs(path) {
			observed["npm_prefix"] = filepath.Clean(path)
		}
	}
	if launcher := t3LauncherProblem(c, service); launcher != "" {
		observed["launcher_problem"] = launcher
		warning := launcher + "; a restart of " + service + " will fail until T3 is installed for the selected Node"
		if earlier, _ := observed["warning"].(string); earlier != "" {
			warning = earlier + "; " + warning
		}
		observed["warning"] = warning
	}
	if exists, known := observed["installed_path_exists"].(bool); known && !exists {
		// A server that still answers from a deleted program keeps working until
		// it is restarted; the warning travels with the observation so a report
		// shows it even when nothing is refused.
		observed["warning"] = "T3 program missing on disk: " +
			adapterSpecString(observed, "installed_path") +
			"; a restart will fail until T3 is reinstalled"
	}
	return observed, nil
}

// t3LauncherProblem reports why the unit's launcher would not start T3, or ""
// when it would or when that cannot be told. The fleet's unit starts the mise
// shim, which runs t3 from whichever Node mise selects now; after a Node
// upgrade that is a prefix T3 was never installed into, while the running
// server keeps answering from the old one. Only a restart finds out.
func t3LauncherProblem(c *Context, service string) string {
	if service == "" {
		return ""
	}
	result := c.Run(
		[]string{"systemctl", "--user", "show", service, "-p", "ExecStart", "--value"},
		10*time.Second, "")
	match := t3ExecStartPattern.FindStringSubmatch(result.Output)
	if !result.OK || match == nil {
		return ""
	}
	path := strings.ReplaceAll(match[1], "%h", c.Home)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Base(resolved) != "mise" {
		return ""
	}
	which := c.Run([]string{resolved, "which", "t3"}, 15*time.Second, "")
	if which.OK {
		return ""
	}
	return "the unit's launcher " + path + " does not resolve t3 (" + firstLine(which.Output) + ")"
}

// t3InstallSparesRunning reports whether npm's global prefix is provably not
// the one the running server was started from. npm then writes into a Node
// install the running process never reads, so installing needs no idle proof;
// only a restart does. Any unknown answer is false.
func t3InstallSparesRunning(observed map[string]any) bool {
	prefix, _ := observed["npm_prefix"].(string)
	running, _ := observed["running_path"].(string)
	if prefix == "" || running == "" {
		return false
	}
	relative, err := filepath.Rel(prefix, running)
	return err == nil && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// firstLine returns the first non-empty line of a command's output.
func firstLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// t3Idle reports whether an observation proves that no thread is running.
//
// An observation the steward could not make is never idle: a missing answer is
// not the same as an answer of zero.
func t3Idle(observed map[string]any) bool {
	if checkOK, _ := observed["check_ok"].(bool); !checkOK {
		return false
	}
	running, counted := observed["running_threads"].(int)
	return counted && running == 0
}

func t3SelectedPrograms(c *Context) (string, string) {
	resolve := func(name string) string {
		selected := adapterLookPath(c, name)
		if selected == "" {
			return ""
		}
		resolved, err := filepath.EvalSymlinks(selected)
		if err != nil || filepath.Base(resolved) != "mise" {
			return selected
		}
		result := c.Run([]string{resolved, "which", name}, 15*time.Second, "")
		actual := firstLine(result.Output)
		if !result.OK || !filepath.IsAbs(actual) {
			return ""
		}
		return actual
	}
	return resolve("node"), resolve("t3")
}

func t3VerifyInstalledCLI(c *Context, version string) error {
	node, t3 := t3SelectedPrograms(c)
	if node == "" || t3 == "" {
		return Failf("installed T3 CLI failed its native startup check; preserving the running service")
	}
	// --version does not instantiate T3's PTY layer. Resolve node-pty relative
	// to the installed CLI entrypoint and load it under the exact Node selected
	// by the agent PATH, so an ABI/build failure is found before service restart.
	const loadNative = "const fs=require('node:fs'),m=require('node:module');" +
		"m.createRequire(fs.realpathSync(process.argv[1]))('node-pty')"
	native := c.Run([]string{node, "-e", loadNative, t3}, 30*time.Second, "")
	probe := c.Run([]string{t3, "--version"}, 30*time.Second, "")
	gotVersion := strings.TrimSpace(probe.Output)
	if !native.OK || !probe.OK ||
		(gotVersion != "t3 v"+version && gotVersion != "t3 "+version) {
		return Failf("installed T3 CLI failed its native startup check; preserving the running service")
	}
	return nil
}

func t3Apply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	version := adapterSpecString(spec, "version")
	service := adapterSpecString(spec, "service")
	// Seeding never overwrites an existing unit, its credentials or its bind
	// address, and it only starts after the installed package and the steward
	// have been verified.
	seeded := false
	installed, _ := before["installed"].(string)
	active, _ := before["service_active"].(bool)
	if installed == version && !active {
		if !c.DryRun {
			if _, _, err := stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
				return "", err
			}
			if err := t3VerifyInstalledCLI(c, version); err != nil {
				return "", err
			}
		}
		var err error
		seeded, err = SeedT3Service(c, actions)
		if err != nil {
			return "", err
		}
		if seeded && !c.DryRun {
			if before, err = t3Observe(c, spec); err != nil {
				return "", err
			}
			installed, _ = before["installed"].(string)
			active, _ = before["service_active"].(bool)
		}
	}
	served, _ := before["served"].(string)
	if installed == version && served == version && active {
		if !c.DryRun {
			if _, _, err := stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
				return "", err
			}
		}
		if seeded {
			return "updated", nil
		}
		return "converged", nil
	}
	if c.DryRun {
		if installed != version && !t3Idle(before) && t3InstallSparesRunning(before) {
			next := "; restart requires a fresh zero-running-thread observation"
			if served == version {
				next = "; no restart needed"
			}
			*actions = append(*actions, "would install t3@"+version+" into "+
				adapterSpecString(before, "npm_prefix")+" beside the running server"+next)
			return "updated", nil
		}
		*actions = append(*actions, "would install t3@"+version+
			"; restart requires a fresh zero-running-thread observation")
		return "updated", nil
	}
	if _, _, err := stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
		return "", err
	}
	// Guard the installation too: npm replaces files a running T3 process may
	// still need.
	fresh, err := t3Observe(c, spec)
	if err != nil {
		return "", err
	}
	busy := !t3Idle(fresh)
	freshVersion, _ := fresh["installed"].(string)
	if busy && (freshVersion == version || !t3InstallSparesRunning(fresh)) {
		*actions = append(*actions,
			"T3 change deferred: steward cannot prove zero running threads")
		return "deferred", nil
	}
	if freshVersion != version {
		if _, err := c.Must([]string{"npm", "install", "-g", "t3@" + version},
			400*time.Second, "", "install pinned T3"); err != nil {
			return "", err
		}
		if adapterLookPath(c, "mise") != "" {
			if _, err := c.Must([]string{"mise", "reshim"}, 0, "", "refresh T3 shim"); err != nil {
				return "", err
			}
		}
	}
	if err = t3VerifyInstalledCLI(c, version); err != nil {
		return "", err
	}
	if fresh, err = t3Observe(c, spec); err != nil {
		return "", err
	}
	if busy && served == version {
		// The running server already serves the pin; the install only made the
		// unit's launcher able to start it again. Nothing needs a restart.
		*actions = append(*actions, "installed t3@"+version+" into "+
			adapterSpecString(fresh, "npm_prefix")+" beside the running server; no restart needed")
		return "updated", nil
	}
	if busy {
		*actions = append(*actions, "T3 installed beside the running server; "+
			"restart deferred: steward cannot prove zero running threads")
		return "deferred", nil
	}
	if !t3Idle(fresh) {
		*actions = append(*actions,
			"T3 installed; restart deferred because idle evidence changed")
		return "deferred", nil
	}
	if _, err := c.Must([]string{"systemctl", "--user", "restart", service},
		0, "", "restart idle T3"); err != nil {
		return "", err
	}
	after, err := t3Observe(c, spec)
	if err != nil {
		return "", err
	}
	afterInstalled, _ := after["installed"].(string)
	afterServed, _ := after["served"].(string)
	afterActive, _ := after["service_active"].(bool)
	if afterInstalled != version || afterServed != version || !afterActive {
		return "", Failf("T3 did not verify after restart; observe before retry")
	}
	*actions = append(*actions, "installed and serving t3@"+version)
	return "updated", nil
}
