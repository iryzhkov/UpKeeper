package agent

import (
	"errors"
	"strconv"
	"time"
)

var errT3BoundaryIdle = errors.New("T3 idle proof changed at mutation boundary")

// Avoid expensive full installed-tree verification between the final explicit
// zero-thread check and mutation/restart. This is still not an admission lock.
func t3BoundaryIdle(c *Context) bool {
	check := c.Run([]string{t3StewardBinary(c), "check"}, 30*time.Second, "")
	count := t3ThreadsPattern.FindStringSubmatch(check.Output)
	if !check.OK || count == nil {
		return false
	}
	n, e := strconv.Atoi(count[1])
	return e == nil && n == 0
}
