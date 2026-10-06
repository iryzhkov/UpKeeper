package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/release"
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
	files := t3RunningFiles(c, service)
	observed["running_file_count"] = nil
	if files != nil {
		observed["running_file_count"] = len(files)
	}
	observed["npm_prefix"] = nil
	if prefix := c.Run([]string{"npm", "prefix", "-g"}, 30*time.Second, ""); prefix.OK {
		if path := firstLine(prefix.Output); filepath.IsAbs(path) {
			// A prefix reached through a symlink is the directory it names; an
			// unresolvable one is unknown, which never counts as spared.
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				observed["npm_prefix"] = resolved
			}
		}
	}
	// The verdict travels in the observation rather than the file list, which
	// is hundreds of paths on a busy host.
	observed["install_spares_running"] = t3InstallSparesRunning(
		map[string]any{"npm_prefix": observed["npm_prefix"], "running_files": files})
	warnings := []string{}
	if exists, known := observed["installed_path_exists"].(bool); known && !exists {
		// A server that still answers from a deleted program keeps working until
		// it is restarted; the warning travels with the observation so a report
		// shows it even when nothing is refused.
		warnings = append(warnings, "T3 program missing on disk: "+
			adapterSpecString(observed, "installed_path")+
			"; a restart will fail until T3 is reinstalled")
	}
	if launcher := t3LauncherProblem(c, service); launcher != "" {
		observed["launcher_problem"] = launcher
		warnings = append(warnings, launcher+"; a restart of "+service+
			" will fail until T3 is installed for the selected Node")
	}
	if len(warnings) > 0 {
		observed["warning"] = strings.Join(warnings, "; ")
	}
	t3ArtifactObservation(c, spec, observed)
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

// t3RunningFiles lists, resolved, the files the running server was started
// from: its executable, every file-path argument on its command line (the T3
// entry script), and every file it has mapped (Node itself, libraries and
// native modules such as node-pty). The Node binary alone is not enough: npm's
// global prefix can be configured away from Node, and then T3's own code lives
// somewhere the executable's path says nothing about. nil means the answer is
// unknown, because there is no running process or /proc could not be read.
func t3RunningFiles(c *Context, service string) []any {
	if service == "" {
		return nil
	}
	result := c.Run(
		[]string{"systemctl", "--user", "show", service, "-p", "MainPID", "--value"},
		10*time.Second, "")
	pid, err := strconv.Atoi(strings.TrimSpace(result.Output))
	if !result.OK || err != nil || pid <= 0 {
		return nil
	}
	main := filepath.Join(t3ProcRoot, strconv.Itoa(pid))
	if exe, err := os.Readlink(filepath.Join(main, "exe")); err != nil || filepath.Base(exe) == "mise" {
		return nil
	}
	// The server's children (a PTY helper, a worker) can load files of their
	// own, so the whole process tree is read; any process that cannot be read
	// makes the answer unknown.
	candidates := []string{}
	pending := []string{strconv.Itoa(pid)}
	visited := map[string]bool{}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		if visited[current] {
			continue
		}
		visited[current] = true
		process := filepath.Join(t3ProcRoot, current)
		files, children, ok := t3ProcessFiles(process)
		if !ok {
			// A child that exited while the tree was read holds no files; the
			// server's sessions start and end short commands all the time.
			if _, err := os.Stat(process); os.IsNotExist(err) && current != strconv.Itoa(pid) {
				continue
			}
			return nil
		}
		candidates = append(candidates, files...)
		pending = append(pending, children...)
	}
	seen := map[string]bool{}
	files := []any{}
	for _, candidate := range candidates {
		path := candidate
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			path = resolved
		}
		if !seen[path] {
			seen[path] = true
			files = append(files, path)
		}
	}
	return files
}

// t3ProcessFiles reads one process's executable, the file-path arguments on
// its command line (a relative one resolved against its working directory)
// and its mapped files, plus the ids of its children. ok is false when any of
// that cannot be read.
func t3ProcessFiles(process string) (files, children []string, ok bool) {
	exe, err := os.Readlink(filepath.Join(process, "exe"))
	if err != nil {
		return nil, nil, false
	}
	cmdline, err := os.ReadFile(filepath.Join(process, "cmdline"))
	if err != nil {
		return nil, nil, false
	}
	maps, err := os.ReadFile(filepath.Join(process, "maps"))
	if err != nil {
		return nil, nil, false
	}
	cwd, _ := os.Readlink(filepath.Join(process, "cwd"))
	files = []string{strings.TrimSuffix(exe, " (deleted)")}
	for _, argument := range strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")[1:] {
		switch {
		case filepath.IsAbs(argument):
			files = append(files, argument)
		case cwd != "" && argument != "" && !strings.HasPrefix(argument, "-"):
			// Only an argument naming an existing file is a path; "serve" is not.
			if _, err := os.Stat(filepath.Join(cwd, argument)); err == nil {
				files = append(files, filepath.Join(cwd, argument))
			}
		}
	}
	for _, line := range strings.Split(string(maps), "\n") {
		// The path is everything after the fifth field, so it may hold spaces;
		// the kernel appends " (deleted)" to a removed file.
		fields := strings.SplitN(line, " ", 6)
		if len(fields) < 6 {
			continue
		}
		path := strings.TrimSuffix(strings.TrimLeft(fields[5], " "), " (deleted)")
		if filepath.IsAbs(path) {
			files = append(files, path)
		}
	}
	tasks, err := os.ReadDir(filepath.Join(process, "task"))
	if err != nil {
		// A /proc without per-thread children lists has no children to report.
		return files, nil, true
	}
	for _, task := range tasks {
		listed, err := os.ReadFile(filepath.Join(process, "task", task.Name(), "children"))
		if err != nil {
			return nil, nil, false
		}
		children = append(children, strings.Fields(string(listed))...)
	}
	return files, children, true
}

// t3InstallSparesRunning reports whether installing T3 into npm's global
// prefix provably writes none of the files the running server's process tree
// uses. The install then needs no idle proof; only a restart does. Any unknown
// answer is false.
func t3InstallSparesRunning(observed map[string]any) bool {
	prefix, _ := observed["npm_prefix"].(string)
	files, _ := observed["running_files"].([]any)
	if prefix == "" || len(files) == 0 {
		return false
	}
	// "npm install -g t3" writes the package directory and its bin link and
	// nothing else in the prefix, so those are the paths a running file must
	// stay out of. Node itself and other packages in the prefix are untouched,
	// which matters because the server's children (agent sessions and the MCP
	// servers they start) routinely run the Node mise selects now.
	written := []string{
		filepath.Join(prefix, "lib", "node_modules", "t3"),
		filepath.Join(prefix, "bin", "t3"),
	}
	for _, file := range files {
		path, _ := file.(string)
		if path == "" {
			return false
		}
		for _, target := range written {
			relative, err := filepath.Rel(target, path)
			if err != nil || relative == "." ||
				!(relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
				return false
			}
		}
	}
	return true
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
	return t3QualifyInstalled(c, node, t3, version)
}

func t3Apply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	_, controlled := spec["artifact"] // Identity transitions precede every version-only fast path.
	prefix := adapterSpecString(before, "npm_prefix")
	if controlled || release.T3ControlledEvidence(c.Home, prefix) {
		return t3ControlledApply(c, spec, before, actions)
	}
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
		if spared, _ := before["install_spares_running"].(bool); installed != version && !t3Idle(before) && spared {
			next := "; restart requires a fresh zero-running-thread observation"
			if served == version {
				next = "; no restart needed"
			}
			*actions = append(*actions, "would install t3@"+version+" into "+
				adapterSpecString(before, "npm_prefix")+" beside the running server"+next)
			return "updated", nil
		}
		if installed == version {
			*actions = append(*actions, "would restart onto t3@"+version+
				" once a fresh observation proves zero running threads")
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
	spared, _ := fresh["install_spares_running"].(bool)
	if busy && (freshVersion == version || !spared) {
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
