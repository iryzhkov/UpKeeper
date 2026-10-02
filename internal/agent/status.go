package agent

import (
	"os"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/hostevidence"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// ObserveRelease deliberately never calls an Apply handler, acquires a lock,
// writes a run record, installs a package or retires a timer. Missing evidence
// is drift, not success, and a broken component does not hide later findings.
func ObserveRelease(c *Context) map[string]any {
	entries := []any{}
	worst := "converged"
	for _, name := range c.Selected {
		entry := map[string]any{"name": name}
		handler, ok := handlers[name]
		if !ok {
			entry["state"] = "failed"
			entry["error"] = "unsupported component"
			worst = "failed"
			entries = append(entries, entry)
			continue
		}
		observed, err := observeQuietly(c, handler, c.Spec(name))
		state := "converged"
		if err != nil {
			state = "failed"
			entry["error"] = err.Error()
		} else {
			entry["observed"] = observed
			if !observationMatches(c, name, c.Spec(name), observed) {
				state = "drift"
			}
		}
		entry["state"] = state
		if state == "failed" || worst != "failed" && state == "drift" {
			worst = state
		}
		entries = append(entries, entry)
	}
	return map[string]any{
		"host": c.Host.Name, "name": c.Host.Name, "run_id": c.RunID, "observe_only": true,
		"reachable": true, "components": entries, "result": worst,
		// This host's own convergence evidence, read the same way the probe
		// reads it, so the controller's status explains this host's drift
		// from the host's run store, hold markers and timers.
		"upkeeper": hostevidence.Collect(c.Home, c.Runner, c.now()),
	}
}

func observationMatches(c *Context, name string, spec, observed map[string]any) bool {
	switch name {
	case "omarchy-setup", "huyang":
		if observed["commit"] != spec["commit"] || observed["dirty"] != false {
			return false
		}
		if name == "huyang" {
			return observed["service_active"] == true && observed["socket"] == true && observed["binary_sha256"] != nil &&
				observed["unit_ordering_cycle"] != true && !huyangUserUnitCyclesObserved(observed)
		}
		stale, ok := observed["stale_files"].([]string)
		return ok && len(stale) == 0
	case "t3-steward":
		v, _ := observed["version"].(string)
		return strings.TrimPrefix(v, "v") == spec["version"] && observed["service_active"] == true && stewardSessionHookConverged(c, spec)
	case "t3":
		return observed["installed"] == spec["version"] && observed["served"] == spec["version"] && observed["service_active"] == true && observed["check_ok"] == true
	case "agent-config":
		for _, f := range agentConfigFiles(spec) {
			if observed[adapterSpecString(f, "path")] != f["sha256"] {
				return false
			}
		}
		return true
	case "agent-environment":
		drift, ok := observed["drift"].([]map[string]any)
		if !ok {
			return false
		}
		for _, d := range drift {
			if d["state"] != agentenv.DriftMatch {
				return false
			}
		}
		component, err := agentenv.DecodeComponent(spec)
		if err != nil {
			return false
		}
		for _, f := range component.Files {
			path, err := agentenv.TargetPath(c.Home, f.Path)
			if err != nil {
				return false
			}
			info, err := os.Stat(path)
			if err != nil || int64(info.Mode().Perm()) != f.Permissions {
				return false
			}
		}
		return true
	case release.JocastaComponent, release.ControlledCodexComponent:
		return observed["converged"] == true
	case release.ToolingComponent:
		drift, ok := observed["drift"].([]string)
		return ok && len(drift) == 0
	case "worker-configuration", "steward-fleet-configuration":
		state, _ := observed["state"].(string)
		return state == "converged" || state == "skipped"
	}
	return false
}
