package agent

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A target is implicitly ordered after every unit it Wants=, so a user unit
// that is wanted by default.target and also ordered After=default.target is
// already in an ordering cycle with default.target on its own, and so is
// every unit ordered after it. systemd breaks such a cycle by deleting one of
// the jobs in it: at boot it deleted t3-steward-worker.service/start ("Job
// t3-steward-worker.service/start deleted to break ordering cycle") and at
// shutdown huyang.service/stop, so the unit whose job was deleted simply does
// not run. `systemd-analyze --user verify` does not report it, which is why the
// agent checks the unit files itself.

// userUnitDir is where a user's own unit files live, relative to home.
const userUnitDir = ".config/systemd/user"

// userUnitBootTarget is the target user services are enabled into.
const userUnitBootTarget = "default.target"

var userUnitSuffixes = []string{
	".service", ".socket", ".timer", ".path", ".mount", ".automount",
	".swap", ".target", ".slice", ".scope",
}

func userUnitName(name string) bool {
	for _, suffix := range userUnitSuffixes {
		if strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			return true
		}
	}
	return false
}

func unitComment(trimmed string) bool {
	return strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";")
}

// unitDirective splits a unit-file line into its key and value when the line
// is an assignment, and reports the section header when it is one.
func unitDirective(line string) (section, key, value string, assignment bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return trimmed[1 : len(trimmed)-1], "", "", false
	}
	if trimmed == "" || unitComment(trimmed) {
		return "", "", "", false
	}
	key, value, found := strings.Cut(trimmed, "=")
	if !found {
		return "", "", "", false
	}
	return "", strings.TrimSpace(key), strings.TrimSpace(value), true
}

// unitContinued reports whether a physical line continues on the next one.
func unitContinued(line string) bool {
	trimmed := strings.TrimRight(line, " \t\r\n")
	return strings.HasSuffix(trimmed, "\\") && !unitComment(strings.TrimSpace(trimmed))
}

// unitLogicalLines joins lines that end in a backslash with the lines that
// continue them, the way systemd reads a unit file. Comment lines inside a
// continuation are skipped.
func unitLogicalLines(raw []byte) []string {
	lines := []string{}
	joined := ""
	continuing := false
	for _, physical := range strings.Split(string(raw), "\n") {
		line := strings.TrimRight(physical, " \t\r")
		if continuing {
			trimmed := strings.TrimSpace(line)
			if unitComment(trimmed) {
				continue
			}
			line = joined + " " + trimmed
		}
		if unitContinued(line) {
			joined = strings.TrimSuffix(line, "\\")
			continuing = true
			continue
		}
		continuing = false
		lines = append(lines, line)
	}
	if continuing {
		lines = append(lines, joined)
	}
	return lines
}

// unitListDirective adds every value a list directive is given in one section
// to list. An empty assignment does not reset a dependency list such as
// After= or WantedBy= (checked on systemd 252), so it adds nothing and keeps
// what is already there.
func unitListDirective(list []string, raw []byte, section, key string) []string {
	current := ""
	for _, line := range unitLogicalLines(raw) {
		header, name, value, assignment := unitDirective(line)
		if header != "" {
			current = header
			continue
		}
		if !assignment || current != section || name != key {
			continue
		}
		list = append(list, strings.Fields(value)...)
	}
	return list
}

func unitListContains(list []string, wanted string) bool {
	for _, value := range list {
		if value == wanted {
			return true
		}
	}
	return false
}

// unitDropOrderingAfter removes target from every After= line of the [Unit]
// section. Every line it does not rewrite is kept byte for byte; a rewritten
// line is normalised to "After=" and the remaining names separated by single
// spaces. A line that named only the target is removed. A line continued with
// a backslash, and its continuation, are never rewritten; the ordering check
// reports such a unit instead.
func unitDropOrderingAfter(raw []byte, target string) ([]byte, bool) {
	var out strings.Builder
	changed := false
	current := ""
	continuation := false
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		// systemd skips a comment line inside a continued value, so the value
		// goes on after it; nothing inside a continued value is rewritten.
		if continuation && unitComment(strings.TrimSpace(line)) {
			out.WriteString(line)
			continue
		}
		if continuation || unitContinued(line) {
			continuation = unitContinued(line)
			out.WriteString(line)
			continue
		}
		header, key, value, assignment := unitDirective(line)
		if header != "" {
			current = header
		}
		if !assignment || current != "Unit" || key != "After" {
			out.WriteString(line)
			continue
		}
		kept := []string{}
		for _, field := range strings.Fields(value) {
			if field != target {
				kept = append(kept, field)
			}
		}
		if len(kept) == len(strings.Fields(value)) {
			out.WriteString(line)
			continue
		}
		changed = true
		if len(kept) > 0 {
			ending := ""
			if strings.HasSuffix(line, "\n") {
				ending = "\n"
			}
			out.WriteString("After=" + strings.Join(kept, " ") + ending)
		}
	}
	return []byte(out.String()), changed
}

// userUnitOrderingCycle reports whether one unit in the user unit directory
// is both wanted by default.target and ordered after it. A unit is wanted
// when [Install] WantedBy= of the unit file or a drop-in names default.target,
// or when default.target.wants/ holds an entry for it. pending holds content a
// dry run would have written in place of the unit file on disk.
func userUnitOrderingCycle(directory, name string, pending map[string][]byte) (bool, error) {
	raw, planned := pending[name]
	if !planned {
		var err error
		raw, err = os.ReadFile(filepath.Join(directory, name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	dropIns, err := filepath.Glob(filepath.Join(directory, name+".d", "*.conf"))
	if err != nil {
		return false, err
	}
	sort.Strings(dropIns)
	files := [][]byte{raw}
	for _, dropIn := range dropIns {
		content, err := os.ReadFile(dropIn)
		if err != nil {
			return false, err
		}
		files = append(files, content)
	}
	var after, wantedBy []string
	for _, content := range files {
		after = unitListDirective(after, content, "Unit", "After")
		wantedBy = unitListDirective(wantedBy, content, "Install", "WantedBy")
	}
	if !unitListContains(after, userUnitBootTarget) {
		return false, nil
	}
	if unitListContains(wantedBy, userUnitBootTarget) {
		return true, nil
	}
	_, err = os.Lstat(filepath.Join(directory, userUnitBootTarget+".wants", name))
	return err == nil, nil
}

// userUnitOrderingCycles lists every installed user unit with an ordering
// cycle on default.target.
func userUnitOrderingCycles(c *Context, pending map[string][]byte) ([]string, error) {
	directory := c.Path(userUnitDir)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	cycles := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if !userUnitName(name) {
			continue
		}
		// A linked unit is followed; a directory or a dangling link is not a
		// unit file systemd could load.
		if info, err := os.Stat(filepath.Join(directory, name)); err != nil || !info.Mode().IsRegular() {
			continue
		}
		cycle, err := userUnitOrderingCycle(directory, name, pending)
		if err != nil {
			return nil, err
		}
		if cycle {
			cycles = append(cycles, name)
		}
	}
	sort.Strings(cycles)
	return cycles, nil
}

// userUnitOrderingProblem describes a cycle in one unit and how to remove it.
func userUnitOrderingProblem(c *Context, name string) string {
	return "user unit " + name + " is wanted by default.target and ordered " +
		"After=default.target, an ordering cycle that makes systemd delete start jobs at boot; " +
		"remove default.target from its After= lines under " + c.Path(userUnitDir) +
		" (unit file and drop-ins), then run systemctl --user daemon-reload"
}

// checkUserUnitOrdering fails the step when the component's own unit is in an
// ordering cycle on default.target. A cycle in any other user unit is not the
// component's to fix, so it is reported as a warning action, and as drift by
// observe, without stopping the components after this one.
func checkUserUnitOrdering(
	c *Context, own string, pending map[string][]byte, actions *[]string,
) error {
	cycles, err := userUnitOrderingCycles(c, pending)
	if err != nil {
		return Failf("cannot check user unit ordering: %s", err.Error())
	}
	ownCycle := false
	for _, name := range cycles {
		if name == own {
			ownCycle = true
			continue
		}
		*actions = append(*actions, "warning: "+userUnitOrderingProblem(c, name))
	}
	if ownCycle {
		return Failf("%s", userUnitOrderingProblem(c, own))
	}
	return nil
}
