package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/iryzhkov/UpKeeper/internal/buildinfo"
)

// Identity is what a host says about the UpKeeper binary it runs: the version
// and source commit compiled into this process, and the generation that
// ~/.local/bin/upkeeper currently runs, or nil when it is not managed.
func Identity(home string) map[string]any {
	return map[string]any{
		"version":    buildinfo.Version,
		"commit":     buildinfo.SourceCommit(),
		"generation": nullable(ActiveGeneration(home)),
	}
}

// Status describes the installation without changing it.
func (i *Installer) Status() map[string]any {
	layout := i.Layout
	link, linkErr := os.Readlink(layout.Binary())
	binary := map[string]any{"path": layout.Binary(), "link": nil, "managed": false}
	if linkErr == nil {
		binary["link"] = link
		binary["managed"] = link == binaryLink
	} else if _, err := os.Lstat(layout.Binary()); err != nil {
		binary["missing"] = true
	}
	current := layout.CurrentGeneration()
	previous := layout.PreviousGeneration()
	generations := []any{}
	entries, _ := os.ReadDir(layout.Generations())
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		entry := map[string]any{
			"name":     name,
			"valid":    layout.validGeneration(name),
			"current":  name == current,
			"previous": name == previous,
		}
		if raw, err := os.ReadFile(filepath.Join(layout.Generation(name), "identity.json")); err == nil {
			var record map[string]any
			if json.Unmarshal(raw, &record) == nil {
				entry["identity"] = record
			}
		}
		generations = append(generations, entry)
	}
	return map[string]any{
		"root":        layout.Root(),
		"binary":      binary,
		"current":     nullable(current),
		"previous":    nullable(previous),
		"generations": generations,
		"running":     map[string]any{"version": buildinfo.Version, "commit": buildinfo.SourceCommit()},
	}
}

// ExecFunc replaces this process with another program. It is syscall.Exec in
// production and a recorder in tests.
type ExecFunc func(path string, argv []string, env []string) error

// Exec is the production ExecFunc.
var Exec ExecFunc = syscall.Exec

// AfterUpdateFlag is the hidden self-pull option a re-executed binary is given.
const AfterUpdateFlag = "--after-update"

// ReexecArguments is the command line a freshly installed binary is started
// with: a self-pull that skips the refresh, because the checkout was refreshed
// already, and must find HEAD at exactly the commit that was gated.
func ReexecArguments(path, commit string) []string {
	return []string{path, "self-pull", AfterUpdateFlag, commit}
}

// Reexec replaces this process with the installed generation. It returns only
// when the replacement could not start.
func Reexec(exec ExecFunc, path, commit string) error {
	if exec == nil {
		exec = Exec
	}
	return exec(path, ReexecArguments(path, commit), os.Environ())
}
