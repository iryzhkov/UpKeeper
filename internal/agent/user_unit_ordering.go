package agent

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A user unit that is wanted by default.target and also ordered after it is
// harmless on its own, but every unit ordered after it then closes a cycle with
// default.target. systemd breaks that cycle by deleting one of the start jobs
// at boot ("Job t3-steward-worker.service/start deleted to break ordering
// cycle") and one of the stop jobs at shutdown, so the deleted unit simply does
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

// unitDirective splits a unit-file line into its key and value when the line
// is an assignment, and reports the section header when it is one.
func unitDirective(line string) (section, key, value string, assignment bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return trimmed[1 : len(trimmed)-1], "", "", false
	}
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
		return "", "", "", false
	}
	key, value, found := strings.Cut(trimmed, "=")
	if !found {
		return "", "", "", false
	}
	return "", strings.TrimSpace(key), strings.TrimSpace(value), true
}

// unitListDirective folds every assignment of a list directive in one section
// into the list systemd ends up with: values accumulate, and an empty
// assignment resets everything assigned before it.
func unitListDirective(list []string, raw []byte, section, key string) []string {
	current := ""
	for _, line := range strings.Split(string(raw), "\n") {
		header, name, value, assignment := unitDirective(line)
		if header != "" {
			current = header
			continue
		}
		if !assignment || current != section || name != key {
			continue
		}
		if value == "" {
			list = nil
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
// section and leaves every other byte as it was. A line that named only the
// target is removed rather than emptied, because an empty After= would also
// reset the ordering earlier lines declared.
func unitDropOrderingAfter(raw []byte, target string) ([]byte, bool) {
	var out strings.Builder
	changed := false
	current := ""
	for _, line := range strings.SplitAfter(string(raw), "\n") {
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

// userUnitOrderingCycle reports whether one unit file in the user unit
// directory is both wanted by default.target and, after its drop-ins are
// applied, ordered after it. pending holds content a dry run would have
// written instead of what is on disk.
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
	if !unitListContains(unitListDirective(nil, raw, "Install", "WantedBy"), userUnitBootTarget) {
		return false, nil
	}
	after := unitListDirective(nil, raw, "Unit", "After")
	dropIns, err := filepath.Glob(filepath.Join(directory, name+".d", "*.conf"))
	if err != nil {
		return false, err
	}
	sort.Strings(dropIns)
	for _, dropIn := range dropIns {
		content, err := os.ReadFile(dropIn)
		if err != nil {
			return false, err
		}
		after = unitListDirective(after, content, "Unit", "After")
	}
	return unitListContains(after, userUnitBootTarget), nil
}

// userUnitOrderingCycles lists every installed user unit with an ordering
// cycle on default.target.
func userUnitOrderingCycles(c *Context, pending map[string][]byte) ([]string, error) {
	directory := c.Path(userUnitDir)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
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

// checkUserUnitOrdering fails the step when any installed user unit has an
// ordering cycle on default.target, naming the units and the fix.
func checkUserUnitOrdering(c *Context, pending map[string][]byte) error {
	cycles, err := userUnitOrderingCycles(c, pending)
	if err != nil {
		return Failf("cannot check user unit ordering: %s", err.Error())
	}
	if len(cycles) == 0 {
		return nil
	}
	subject := "user unit " + cycles[0] + " is"
	if len(cycles) > 1 {
		subject = "user units " + strings.Join(cycles, ", ") + " are"
	}
	return Failf("%s wanted by default.target and ordered After=default.target, "+
		"an ordering cycle that makes systemd delete start jobs at boot; "+
		"remove default.target from its After= lines under %s (unit file and drop-ins), "+
		"then run systemctl --user daemon-reload",
		subject, c.Path(userUnitDir))
}
