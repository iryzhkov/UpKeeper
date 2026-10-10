package rollout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Persist every newly created directory name in its parent before relying on it.
func durableDirectory(path string) error {
	st, err := os.Lstat(path)
	if err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe journal directory")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err := durableDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	d, err := os.Open(parent)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
func validArgv(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, arg := range args {
		if strings.TrimSpace(arg) == "" || strings.ContainsRune(arg, 0) {
			return false
		}
	}
	return true
}
func validPhaseIntent(action, host string) bool {
	parts := strings.Split(action, ":")
	if len(parts) == 4 && parts[0] == "recovery" && parts[2] == host && ((parts[1] == "dropin" && (host == "agent-a" || host == "agent-b")) || (parts[1] == "rollback" && contains(rc122Order, host))) {
		n, err := strconv.Atoi(parts[3])
		return err == nil && n > 0 && strconv.Itoa(n) == parts[3]
	}
	if host != "" || !strings.HasPrefix(action, "phase:") {
		return false
	}
	phase := strings.TrimPrefix(action, "phase:")
	switch phase {
	case "fence", "prepare", "publish", "release-ci", "backup", "config-patch", "verify":
		return true
	}
	for _, h := range rc122Order {
		for _, prefix := range []string{"accept:", "observe:", "dropin:", "enroll-worker:"} {
			if phase == prefix+h {
				return true
			}
		}
	}
	return false
}
func requireAttempt(c Config, action string, now time.Time) error {
	intent, done, err := readAttempt(c, attemptPath(c, action), now)
	if err != nil {
		return fmt.Errorf("required intent %s: %w", action, err)
	}
	if intent.Action != action || !done {
		return fmt.Errorf("required acknowledgement absent: %s", action)
	}
	return nil
}

// Check every acceptance reader, not merely the entries still present on disk.
func requiredAttempts(c Config, s *State, now time.Time) error {
	if _, err := os.Lstat(c.StatePath); err == nil {
		if _, err := os.Lstat(c.StatePath + ".attempts"); err != nil {
			return fmt.Errorf("journal requires intent inventory: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for action, done := range s.Completed {
		if !done {
			continue
		}
		if action == "soak" {
			action = "host:" + c.Canary
		}
		if strings.HasPrefix(action, "host:") {
			h := strings.TrimPrefix(action, "host:")
			for _, a := range []string{"run:" + h, "phase:observe:" + h, "phase:accept:" + h} {
				if err := requireAttempt(c, a, now); err != nil {
					return err
				}
			}
		} else {
			if !validPhaseIntent("phase:"+action, "") {
				return fmt.Errorf("unknown completed phase %s", action)
			}
			if err := requireAttempt(c, "phase:"+action, now); err != nil {
				return err
			}
			if strings.HasPrefix(action, "enroll-worker:") {
				h := strings.TrimPrefix(action, "enroll-worker:")
				if err := requireAttempt(c, "enroll:"+h, now); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
