package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func init() {
	Register("omarchy-setup", Handler{Observe: setupObserve, Apply: setupApply})
}

// setupRetiredModules names the setup modules that used to install the fleet
// itself. A historical release may still list them, and running them would put
// a second, unmanaged installer back on the host.
var setupRetiredModules = map[string]bool{"26-t3": true, "45-agents": true}

// adapterSpecString reads a string field from a manifest entry, returning ""
// when the release does not carry the field.
func adapterSpecString(spec map[string]any, key string) string {
	value, _ := spec[key].(string)
	return value
}

// adapterSpecStrings reads a list-of-strings field from a manifest entry.
func adapterSpecStrings(spec map[string]any, key string) []string {
	items, _ := spec[key].([]any)
	values := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			values = append(values, text)
		}
	}
	return values
}

// adapterSpecMap reads a nested object field from a manifest entry.
func adapterSpecMap(spec map[string]any, key string) map[string]any {
	value, _ := spec[key].(map[string]any)
	return value
}

// adapterSpecInt reads a numeric field from a manifest entry.
//
// A decoded manifest carries every number as a json.Number so that a value read
// from a release is written back with its original literal text, so the field
// is almost never a Go numeric type here.
func adapterSpecInt(spec map[string]any, key string) (int, bool) {
	switch typed := spec[key].(type) {
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(number), true
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case string:
		number, err := strconv.Atoi(typed)
		if err != nil {
			return 0, false
		}
		return number, true
	default:
		return 0, false
	}
}

// adapterShortCommit abbreviates a commit the way the reports do.
func adapterShortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

// adapterOrNone renders an absent observation the way the Python agent's report
// text did, because the controller prints these strings verbatim.
func adapterOrNone(value string) string {
	if value == "" {
		return "None"
	}
	return value
}

// adapterLookPath finds an executable on the agent's own PATH, which puts the
// fleet's tool directories in front of the inherited one. It is the equivalent
// of shutil.which(name, path=ENV["PATH"]).
func adapterLookPath(c *Context, name string) string {
	search := ""
	for _, entry := range c.Env {
		if value, found := strings.CutPrefix(entry, "PATH="); found {
			search = value
		}
	}
	for _, directory := range strings.Split(search, ":") {
		if directory == "" {
			continue
		}
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// adapterDirectoryEntries lists a directory in name order, and reports an
// unreadable or absent directory as empty rather than as a failure.
func adapterDirectoryEntries(directory string) []string {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// setupManagedFilePairs lists the (source in the checkout, target in the home
// directory) pairs the setup modules project.
//
// When the release carries agent-environment, that component owns the harness
// instruction file and the skills, so setup must not claim them as well; the
// scripts in config/bin stay with setup either way.
func setupManagedFilePairs(c *Context, checkout string) [][2]string {
	pairs := [][2]string{}
	if !c.Has("agent-environment") {
		pairs = append(pairs, [2]string{
			filepath.Join(checkout, "config/claude/CLAUDE.md"),
			c.Path(".claude/omarchy-setup/CLAUDE.md"),
		})
		skills := filepath.Join(checkout, "config/claude/skills")
		for _, skill := range adapterDirectoryEntries(skills) {
			directory := filepath.Join(skills, skill)
			for _, name := range adapterDirectoryEntries(directory) {
				source := filepath.Join(directory, name)
				if isRegularFile(source) {
					pairs = append(pairs, [2]string{source, c.Path(".claude/skills", skill, name)})
				}
			}
		}
	}
	scripts := filepath.Join(checkout, "config/bin")
	for _, name := range adapterDirectoryEntries(scripts) {
		source := filepath.Join(scripts, name)
		if isRegularFile(source) {
			pairs = append(pairs, [2]string{source, c.Path(".local/bin", name)})
		}
	}
	return pairs
}

// setupStaleManagedFiles names every managed file whose content differs from
// the checkout, as a path relative to the home directory.
func setupStaleManagedFiles(c *Context, checkout string) []string {
	stale := []string{}
	for _, pair := range setupManagedFilePairs(c, checkout) {
		if c.SHA256File(pair[0]) == c.SHA256File(pair[1]) {
			continue
		}
		relative, err := filepath.Rel(c.Home, pair[1])
		if err != nil {
			relative = pair[1]
		}
		stale = append(stale, relative)
	}
	return stale
}

func setupObserve(c *Context, spec map[string]any) (map[string]any, error) {
	checkout := c.Path(adapterSpecString(spec, "path"))
	head := c.GitHead(checkout)
	observed := map[string]any{"commit": nil, "dirty": nil, "stale_files": nil}
	if head == "" {
		return observed, nil
	}
	observed["commit"] = head
	if dirty := c.GitDirty(checkout); dirty != nil {
		observed["dirty"] = *dirty
	}
	observed["stale_files"] = setupStaleManagedFiles(c, checkout)
	return observed, nil
}

func setupApply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	checkout := c.Path(adapterSpecString(spec, "path"))
	commit, _ := before["commit"].(string)
	if commit == "" {
		return "", Failf(
			"%s is not a git checkout; bootstrap omarchy-setup by hand first", checkout)
	}
	if dirty, _ := before["dirty"].(bool); dirty {
		return "", Failf("%s has local changes; refusing to pull over them", checkout)
	}
	wanted := adapterSpecString(spec, "commit")
	state := "converged"
	if commit != wanted {
		if c.DryRun {
			*actions = append(*actions,
				"would git pull --ff-only to "+adapterShortCommit(wanted))
		} else {
			if _, err := c.Must(
				[]string{"git", "-C", checkout, "pull", "--ff-only", "--quiet"},
				120*time.Second, "", "git pull",
			); err != nil {
				return "", err
			}
			*actions = append(*actions, "git pull --ff-only")
		}
		head := c.GitHead(checkout)
		if !c.DryRun && head != wanted {
			// A checkout that already carries the release is ahead, not broken;
			// anything else means the pull did not do what the release asked.
			if head != "" && c.GitContains(checkout, wanted, head) {
				state = "ahead"
				*actions = append(*actions,
					"checkout is ahead of the release at "+adapterShortCommit(head))
			} else {
				return "", Failf("after pull HEAD is %s, wanted %s",
					adapterOrNone(head), wanted)
			}
		}
	}
	stale := setupStaleManagedFiles(c, checkout)
	if len(stale) == 0 && commit == wanted {
		return state, nil
	}
	modules := adapterSpecStrings(spec, "modules")
	if c.DryRun {
		listed := "none"
		if len(stale) > 0 {
			listed = strings.Join(stale, ", ")
		}
		*actions = append(*actions, fmt.Sprintf("would run modules %s (stale: %s)",
			strings.Join(modules, " "), listed))
	} else {
		if err := setupRunModules(c, checkout, modules); err != nil {
			return "", err
		}
		*actions = append(*actions, "ran modules "+strings.Join(modules, " "))
		if remaining := setupStaleManagedFiles(c, checkout); len(remaining) > 0 {
			return "", Failf("managed files still differ after modules: %s",
				strings.Join(remaining, ", "))
		}
	}
	if state == "converged" {
		state = "updated"
	}
	return state, nil
}

// setupRunModules drives the setup modules the release names.
func setupRunModules(c *Context, checkout string, modules []string) error {
	for _, module := range modules {
		if setupRetiredModules[module] {
			return Failf(
				"historical setup release includes retired fleet modules; " +
					"select other components")
		}
	}
	script := filepath.Join(checkout, "run.sh")
	if adapterLookPath(c, "omarchy") != "" && isRegularFile(script) {
		argv := []string{"bash", script, "--yes", "--skip-secrets"}
		for _, module := range modules {
			argv = append(argv, "--only", module)
		}
		_, err := c.Must(argv, 900*time.Second, "", "omarchy-setup run.sh")
		return err
	}
	// A host without Omarchy (homelab) cannot use run.sh; drive the modules with
	// the bootstrap environment instead, without delegating fleet installation.
	saved := c.Env
	c.Env = append(append([]string{}, saved...),
		"OMARCHY_SETUP_ROOT="+checkout,
		"OMARCHY_SETUP_LIB="+filepath.Join(checkout, "lib"),
		"ASSUME_YES=1",
		"SETUP_PROFILE=remote",
	)
	defer func() { c.Env = saved }()
	for _, module := range modules {
		if _, err := c.Must(
			[]string{"bash", filepath.Join(checkout, "modules/common", module+".sh")},
			900*time.Second, "", module,
		); err != nil {
			return err
		}
	}
	return nil
}
