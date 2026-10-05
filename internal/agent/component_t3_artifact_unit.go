package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/release"
)

// Bind the complete unit command and its selected launcher. The bin link can
// change from the supported legacy entrypoint to the modern entrypoint.
func t3UnitBinding(c *Context, service, prefix string) (string, error) {
	result := c.Run([]string{"systemctl", "--user", "show", service, "-p", "ExecStart", "--value"}, 10*time.Second, "")
	match := t3ExecStartPattern.FindStringSubmatch(result.Output)
	if !result.OK || match == nil {
		return "", errors.New("unknown T3 service launcher")
	}
	selected := strings.ReplaceAll(match[1], "%h", c.Home)
	resolved, e := filepath.EvalSymlinks(selected)
	if e == nil && filepath.Base(resolved) == "mise" {
		resultWhich := c.Run([]string{resolved, "which", "t3"}, 15*time.Second, "")
		if !resultWhich.OK {
			return "", errors.New("unknown mise T3 service launcher")
		}
		selected = firstLine(resultWhich.Output)
		resolved, e = filepath.EvalSymlinks(selected)
	}
	root := filepath.Join(prefix, "lib/node_modules/t3")
	if !filepath.IsAbs(selected) {
		return "", errors.New("relative T3 service launcher")
	}
	if e != nil {
		// Only an entirely absent installation may use the future owned bin link.
		if selected != filepath.Join(prefix, "bin/t3") || !t3Absent(root) || !t3Absent(selected) {
			return "", errors.New("unresolvable T3 service launcher")
		}
	} else if resolved != filepath.Join(root, "bin/t3.js") &&
		!(selected == filepath.Join(prefix, "bin/t3") && resolved == filepath.Join(root, "dist/bin.mjs")) &&
		resolved != filepath.Join(root, "node_modules/@t3code/t3-"+release.T3Target()+"/t3") {
		return "", errors.New("T3 service launcher is outside supported owned package")
	}
	return result.Output + "\x00" + selected, nil
}

func t3UnitSelectsOwned(c *Context, service, prefix string) bool {
	_, e := t3UnitBinding(c, service, prefix)
	return e == nil
}
