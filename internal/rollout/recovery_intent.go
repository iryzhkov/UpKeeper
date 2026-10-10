package rollout

import (
	"fmt"
	"os"
	"time"
)

// Every named restore has a fresh permanent intent. A failed previous restore
// stays in the inventory and can never authorize forward enrollment.
func beginRecovery(c Config, host string, now time.Time) (func() error, error) {
	return beginRecoveryFor(c, "dropin", host, now)
}
func beginRecoveryFor(c Config, kind, host string, now time.Time) (func() error, error) {
	ack, _, err := beginRecoveryInvocation(c, kind, host, now)
	return ack, err
}
func beginRecoveryInvocation(c Config, kind, host string, now time.Time) (func() error, string, error) {
	for n := 1; ; n++ {
		action := fmt.Sprintf("recovery:%s:%s:%d", kind, host, n)
		_, err := os.Lstat(attemptPath(c, action))
		if os.IsNotExist(err) {
			ack, err := beginAttempt(c, action, host, now)
			return ack, action, err
		}
		if err != nil {
			return nil, "", err
		}
		if _, _, err := readAttempt(c, attemptPath(c, action), now); err != nil {
			return nil, "", err
		}
	}
}
