package agent

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"
)

const fleetCoordinatorMaxBytes = 4 << 20

func fleetReadRetirementCoordinator(path string, info os.FileInfo) ([]byte, error) {
	if info.Size() > fleetCoordinatorMaxBytes {
		return nil, errors.New("coordinator fleet document exceeds 4 MiB")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) ||
		opened.Mode().Perm() != 0o600 || opened.Size() > fleetCoordinatorMaxBytes {
		return nil, errors.New("coordinator fleet document changed before bounded read")
	}
	// The descriptor can grow after stat. Read at most one byte beyond the
	// accepted bound so even that race cannot allocate an unbounded document.
	body, err := io.ReadAll(io.LimitReader(file, fleetCoordinatorMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > fleetCoordinatorMaxBytes {
		return nil, errors.New("coordinator fleet document exceeds 4 MiB")
	}
	return body, nil
}

// Retirement accepts only the documented v1 child field sets and value shapes.
// A future child extension is not safe to delete merely because the outer
// version and field names still look familiar.
func fleetKnownCoordinatorChildren(entry map[string]any) bool {
	workers, workersOK := entry["workers"].(map[string]any)
	projects, projectsOK := entry["projects"].(map[string]any)
	if _, ok := entry["coordinator_id"].(string); !ok || !workersOK || !projectsOK {
		return false
	}
	stringsOnly := func(value any) bool {
		items, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	}
	for _, value := range workers {
		worker, ok := value.(map[string]any)
		if !ok || !workerSameFields(worker, []string{"worker_id", "cpu_class", "executor_slots",
			"capabilities", "provider_instances", "desired_models", "quota_pools"}) {
			return false
		}
		if _, ok := worker["worker_id"].(string); !ok {
			return false
		}
		if _, ok := worker["cpu_class"].(string); !ok {
			return false
		}
		slots, ok := worker["executor_slots"].(json.Number)
		if !ok {
			return false
		}
		if _, err := slots.Int64(); err != nil {
			return false
		}
		for _, key := range []string{"capabilities", "provider_instances", "quota_pools"} {
			if !stringsOnly(worker[key]) {
				return false
			}
		}
		models, ok := worker["desired_models"].(map[string]any)
		if !ok {
			return false
		}
		for _, models := range models {
			if !stringsOnly(models) {
				return false
			}
		}
	}
	for _, value := range projects {
		project, ok := value.(map[string]any)
		if !ok || !workerSameFields(project, []string{"repository", "default_ref", "setup_profile", "eligible_workers"}) {
			return false
		}
		for _, key := range []string{"repository", "default_ref", "setup_profile"} {
			if _, ok := project[key].(string); !ok {
				return false
			}
		}
		if !stringsOnly(project["eligible_workers"]) {
			return false
		}
	}
	return true
}
