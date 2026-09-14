package agent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
)

// acquireLock takes the target-side half of the deployment lock.
//
// It returns the current owner when the host is locked by someone else, nil
// when the run may proceed. When the controller is this same machine it already
// holds the lock for this run in the same directory, so re-entering our own run
// is not contention and the controller, not the agent, releases it.
func (c *Context) acquireLock() (map[string]any, error) {
	if c.DryRun {
		if _, err := os.Stat(c.lockDirectory); errors.Is(err, fs.ErrNotExist) {
			// A dry run observes; it must not leave a lock behind.
			return nil, nil
		}
	}
	if err := fsatomic.EnsureDirectory(filepath.Dir(c.lockDirectory), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(c.lockDirectory, 0o700); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		var owner map[string]any
		if data, readErr := os.ReadFile(filepath.Join(c.lockDirectory, "owner.json")); readErr == nil {
			if decoded, decodeErr := canonjson.DecodeObject(data); decodeErr == nil {
				owner = decoded
			}
		}
		if owner != nil {
			if runID, ok := owner["run_id"].(string); ok && runID == c.RunID {
				return nil, nil
			}
			return owner, nil
		}
		return map[string]any{"run_id": "unknown"}, nil
	}
	payload, err := json.Marshal(c.Payload.Lock)
	if err != nil {
		_ = os.Remove(c.lockDirectory)
		return nil, err
	}
	generic, err := canonjson.Decode(payload)
	if err != nil {
		_ = os.Remove(c.lockDirectory)
		return nil, err
	}
	owner, err := canonjson.MarshalSpaced(generic)
	if err != nil {
		_ = os.Remove(c.lockDirectory)
		return nil, err
	}
	if err := os.WriteFile(
		filepath.Join(c.lockDirectory, "owner.json"), append(owner, '\n'), 0o600,
	); err != nil {
		_ = os.Remove(c.lockDirectory)
		return nil, err
	}
	c.lockOwned = true
	return nil, nil
}

// releaseLock drops only a lock this agent created.
func (c *Context) releaseLock() {
	if !c.lockOwned {
		return
	}
	_ = os.Remove(filepath.Join(c.lockDirectory, "owner.json"))
	_ = os.Remove(c.lockDirectory)
	c.lockOwned = false
}
