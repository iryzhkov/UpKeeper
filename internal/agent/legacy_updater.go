package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// updaterUnits are the legacy self-update units UpKeeper replaces.
var updaterUnits = []string{"t3-update.timer", "t3-update.service"}

// unitStatus reads the properties of a user unit.
//
// systemctl exits non-zero for a unit it does not know but still reports
// LoadState=not-found, which is an answer rather than a failure.
func unitStatus(c *Context, name string) (map[string]string, error) {
	result := c.Run([]string{
		"systemctl", "--user", "show", name,
		"--property=LoadState,ActiveState,UnitFileState,FragmentPath",
	}, 15*time.Second, "")
	fields := map[string]string{}
	for _, line := range strings.Split(result.Output, "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			fields[key] = value
		}
	}
	if fields["LoadState"] == "not-found" {
		return fields, nil
	}
	if !result.OK || fields["LoadState"] == "" || fields["ActiveState"] == "" {
		return nil, fmt.Errorf("cannot observe user unit %s", name)
	}
	return fields, nil
}

// RetireLegacyUpdater removes the superseded t3-update timer and service.
//
// Retirement is irreversible release policy, so it runs even during a
// historical pull. New activations are stopped before anything is deleted, and
// an updater that is still mid-installation defers rather than being killed.
func RetireLegacyUpdater(c *Context) map[string]any {
	state, err := retireLegacyUpdater(c)
	if err != nil {
		return map[string]any{"state": "failed", "actions": []string{}, "error": err.Error()}
	}
	return state
}

func retireLegacyUpdater(c *Context) (map[string]any, error) {
	directory := c.Path(".config/systemd/user")
	states := map[string]map[string]string{}
	for _, name := range updaterUnits {
		status, err := unitStatus(c, name)
		if err != nil {
			return nil, err
		}
		states[name] = status
	}
	paths := make([]string, 0, len(updaterUnits))
	for _, name := range updaterUnits {
		paths = append(paths, filepath.Join(directory, name))
	}
	present := false
	for _, path := range paths {
		if pathExists(path) {
			present = true
		}
	}
	for _, status := range states {
		if status["LoadState"] != "not-found" {
			present = true
		}
	}
	if !present {
		return map[string]any{"state": "converged", "actions": []string{}}, nil
	}
	for name, status := range states {
		fragment := status["FragmentPath"]
		if fragment != "" && filepath.Clean(fragment) != filepath.Join(directory, name) {
			return nil, fmt.Errorf("legacy updater has an unexpected unit path: %s", name)
		}
	}
	if c.DryRun {
		return map[string]any{
			"state":   "updated",
			"actions": []string{"would disable and delete legacy updater units"},
		}, nil
	}
	// Stop new activations first. Never kill an in-flight package installation.
	if states["t3-update.timer"]["LoadState"] != "not-found" {
		if _, err := c.Must(
			[]string{"systemctl", "--user", "disable", "--now", "t3-update.timer"},
			0, "", "disable legacy updater timer",
		); err != nil {
			return nil, err
		}
	}
	service, err := unitStatus(c, "t3-update.service")
	if err != nil {
		return nil, err
	}
	active := service["ActiveState"]
	if active != "" && active != "inactive" && active != "failed" {
		return map[string]any{"state": "deferred", "actions": []string{
			"legacy timer disabled; updater service still running, observe before retry",
		}}, nil
	}
	if service["LoadState"] != "not-found" {
		if _, err := c.Must([]string{"systemctl", "--user", "disable", "t3-update.service"},
			0, "", "disable legacy updater service"); err != nil {
			return nil, err
		}
		if _, err := c.Must([]string{"systemctl", "--user", "stop", "t3-update.service"},
			0, "", "stop legacy updater service"); err != nil {
			return nil, err
		}
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil && info.IsDir() {
			return nil, errors.New("legacy updater unit is a directory")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if _, err := c.Must([]string{"systemctl", "--user", "daemon-reload"},
		0, "", "reload after updater retirement"); err != nil {
		return nil, err
	}
	for _, name := range updaterUnits {
		status, err := unitStatus(c, name)
		if err != nil {
			return nil, err
		}
		state := status["ActiveState"]
		if status["LoadState"] != "not-found" ||
			(state != "" && state != "inactive" && state != "failed") {
			return nil, fmt.Errorf("legacy updater retirement did not verify: %s", name)
		}
	}
	return map[string]any{
		"state":   "updated",
		"actions": []string{"disabled and deleted legacy updater units"},
	}, nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
