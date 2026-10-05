package agent

import (
	"path/filepath"
	"strings"
	"time"
)

func t3UnitSelectsOwned(c *Context, service, prefix string) bool {
	result := c.Run([]string{"systemctl", "--user", "show", service, "-p", "ExecStart", "--value"}, 10*time.Second, "")
	match := t3ExecStartPattern.FindStringSubmatch(result.Output)
	if !result.OK || match == nil {
		return false
	}
	selected := strings.ReplaceAll(match[1], "%h", c.Home)
	resolved, e := filepath.EvalSymlinks(selected)
	if e != nil {
		return false
	}
	if filepath.Base(resolved) == "mise" {
		result = c.Run([]string{resolved, "which", "t3"}, 15*time.Second, "")
		if !result.OK {
			return false
		}
		selected = firstLine(result.Output)
		resolved, e = filepath.EvalSymlinks(selected)
		if e != nil {
			return false
		}
	}
	return resolved == filepath.Join(prefix, "lib/node_modules/t3/bin/t3.js") ||
		resolved == filepath.Join(prefix, "lib/node_modules/t3/node_modules/@t3code/t3-linux-x64/t3") ||
		resolved == filepath.Join(prefix, "lib/node_modules/t3/node_modules/@t3code/t3-linux-arm64/t3")
}
