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
		if !ok {
			return false
		}
		fields := []string{"worker_id", "cpu_class", "executor_slots",
			"capabilities", "provider_instances", "desired_models", "quota_pools"}
		// quota_bindings is the one optional v1 child: rendered only when a
		// binding is authored, as a string per instance.
		if bindings, present := worker["quota_bindings"]; present {
			bound, ok := bindings.(map[string]any)
			if !ok {
				return false
			}
			for _, pool := range bound {
				if _, ok := pool.(string); !ok {
					return false
				}
			}
			fields = append(fields, "quota_bindings")
		}
		if !workerSameFields(worker, fields) {
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
		if !ok || !stringsOnly(project["eligible_workers"]) {
			return false
		}
		projectType, typed := project["type"].(string)
		if !typed {
			if !workerSameFields(project, []string{"repository", "default_ref", "setup_profile", "eligible_workers"}) {
				return false
			}
		} else if projectType == "git" {
			if !workerSameFields(project, []string{"type", "repository", "default_ref", "setup_profile", "eligible_workers"}) {
				return false
			}
		} else if projectType == "fresh" {
			allowed := []string{"type", "eligible_workers"}
			for _, key := range []string{"repository", "default_ref", "setup_profile"} {
				if _, present := project[key]; present {
					allowed = append(allowed, key)
				}
			}
			if !workerSameFields(project, allowed) {
				return false
			}
			for _, key := range []string{"repository", "default_ref"} {
				if value, present := project[key]; present && value != "" {
					return false
				}
			}
		} else {
			return false
		}
		for _, key := range []string{"repository", "default_ref", "setup_profile"} {
			if value, present := project[key]; present {
				if _, ok := value.(string); !ok {
					return false
				}
			}
		}
	}
	return true
}
