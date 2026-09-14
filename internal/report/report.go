// Package report builds the stable doctor and status documents from raw probe
// observations.
//
// The documents are a public contract: their field names, their check names,
// their state strings and the exit code each result maps to are consumed by
// operators, timers and stored run records. Nothing here may be renamed for
// tidiness, and the schema version is bumped rather than edited in place.
package report

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/runid"
	"github.com/iryzhkov/UpKeeper/internal/safety"
	"github.com/iryzhkov/UpKeeper/internal/transport"
)

// stewardCommitPattern reads the commit out of the version line t3-steward
// prints, which is the only place it reports which build is installed.
var stewardCommitPattern = regexp.MustCompile(`commit ([0-9a-f]{7,40})`)

// manifestCommitTimeout bounds the one command a report runs. A report that
// hangs on Git is a report nobody sees.
const manifestCommitTimeout = 3 * time.Second

// Doctor renders the prerequisite report and the exit code that goes with it.
//
// The observations are keyed by host name and order is the selection they were
// gathered for: the hosts appear in the report in exactly that order, because a
// Go map has none of its own.
func Doctor(
	config *inventory.Config, observations map[string]any, order []string,
) (map[string]any, int) {
	return DoctorWith(config, observations, order, procx.Run)
}

// Status renders the convergence report and the exit code that goes with it.
// Its observations and order are read exactly as Doctor reads them.
func Status(
	config *inventory.Config, observations map[string]any, order []string,
) (map[string]any, int) {
	return StatusWith(config, observations, order, procx.Run)
}

// DoctorWith is Doctor with the runner named explicitly, so that a test can
// build a report without running Git.
func DoctorWith(
	config *inventory.Config, observations map[string]any, order []string, runner procx.Runner,
) (map[string]any, int) {
	document := base("doctor", config, runner)
	hosts := []any{}
	anyOffline := false
	anyFailed := false

	for _, name := range order {
		observed := observation(observations, name)
		spec := config.Hosts[name]
		entry := map[string]any{
			"name":      name,
			"address":   spec.Address,
			"transport": observed["transport"],
			"reachable": reachable(observed),
			"checks":    []any{},
		}
		if !truthy(observed["reachable"]) {
			anyOffline = true
			entry["result"] = "offline"
			entry["error"] = errorOf(observed)
			hosts = append(hosts, entry)
			continue
		}

		checks := hostChecks(spec, observed)
		failed := []any{}
		for _, item := range checks {
			check := item.(map[string]any)
			if passed, _ := check["passed"].(bool); !passed {
				failed = append(failed, check["name"])
			}
		}
		entry["checks"] = checks
		entry["locks"] = list(observed, "locks")
		entry["failed_checks"] = failed
		if len(failed) > 0 {
			entry["result"] = "failed"
			anyFailed = true
		} else {
			entry["result"] = "healthy"
		}
		hosts = append(hosts, entry)
	}

	// An offline host outranks a failed one: a fleet that could not be looked
	// at in full may never be reported as merely unhealthy.
	result, code := "healthy", exitcode.Success
	switch {
	case anyOffline:
		result, code = "partial", exitcode.Partial
	case anyFailed:
		result, code = "failed", exitcode.Preflight
	}
	document["result"] = result
	document["exit_code"] = code
	document["hosts"] = hosts
	return document, code
}

// StatusWith is Status with the runner named explicitly.
func StatusWith(
	config *inventory.Config, observations map[string]any, order []string, runner procx.Runner,
) (map[string]any, int) {
	document := base("status", config, runner)
	hosts := []any{}
	anyOffline := false
	anyDrift := false
	desired := config.Components()

	for _, name := range order {
		observed := observation(observations, name)
		if !truthy(observed["reachable"]) {
			anyOffline = true
			hosts = append(hosts, map[string]any{
				"name":       name,
				"reachable":  false,
				"result":     "offline",
				"error":      errorOf(observed),
				"components": []any{},
				"locks":      []any{},
			})
			continue
		}

		observedComponents := object(observed, "components")
		components := []any{}
		hostDrift := false
		for _, componentName := range safety.SortedKeys(desired) {
			definition, _ := desired[componentName].(map[string]any)
			component := componentStatus(
				componentName, definition, object(observedComponents, componentName),
			)
			if component["state"] != "converged" {
				hostDrift = true
			}
			components = append(components, component)
		}
		anyDrift = anyDrift || hostDrift
		result := "converged"
		if hostDrift {
			result = "drift"
		}
		hosts = append(hosts, map[string]any{
			"name":      name,
			"reachable": true,
			"result":    result,
			"identity": map[string]any{
				"hostname":          observed["hostname"],
				"user":              observed["user"],
				"architecture":      observed["architecture"],
				"machine_id_sha256": observed["machine_id_sha256"],
			},
			"components": components,
			"locks":      list(observed, "locks"),
		})
	}

	result, code := "converged", exitcode.Success
	switch {
	case anyOffline:
		result, code = "partial", exitcode.Partial
	case anyDrift:
		result, code = "drift", exitcode.Drift
	}
	document["result"] = result
	document["exit_code"] = code
	document["hosts"] = hosts
	return document, code
}

// base is the header every report carries, whatever it went on to find.
func base(command string, config *inventory.Config, runner procx.Runner) map[string]any {
	return map[string]any{
		"schema_version":          exitcode.SchemaVersion,
		"command":                 command,
		"run_id":                  newRunID(),
		"generated_at":            pytime.ISO8601Z(time.Now()),
		"controller":              controller(config),
		"desired_manifest_commit": manifestCommit(config.Root, runner),
	}
}

// newRunID mints the identifier that ties this report to its durable record.
//
// The system random source is the only thing that can fail here. When it does,
// the report still needs an identifier of the documented shape, so it falls
// back to one whose suffix is visibly not random rather than printing nothing.
func newRunID() string {
	if value, err := runid.New(); err == nil {
		return value
	}
	degraded, err := runid.At(time.Now(), "00000000")
	if err != nil {
		return ""
	}
	return degraded
}

// controller names the inventory host this report was produced on.
func controller(config *inventory.Config) string {
	current := transport.LocalMachineIDSHA256()
	if current == "" {
		return "unknown"
	}
	for _, name := range safety.SortedKeys(config.Hosts) {
		if config.Hosts[name].MachineIDSHA256 == current {
			return name
		}
	}
	return "unknown"
}

// manifestCommit records which revision of the desired state this report was
// judged against, or nil when the root is not a checkout.
func manifestCommit(root string, runner procx.Runner) any {
	if runner == nil {
		runner = procx.Run
	}
	result := runner(procx.Command{
		Argv:    []string{"git", "-C", root, "rev-parse", "HEAD"},
		Timeout: manifestCommitTimeout,
	})
	if result.Err != nil {
		return nil
	}
	value := strings.TrimSpace(result.Stdout)
	if result.Code != 0 || len(value) != 40 {
		return nil
	}
	return value
}

// check records one judgement about a host. An expectation of nil is omitted,
// because a check that only asks for a value to be present has nothing to
// compare against.
func check(name string, passed bool, actual any, expected any) map[string]any {
	value := map[string]any{"name": name, "passed": passed, "actual": actual}
	if expected != nil {
		value["expected"] = expected
	}
	return value
}

// hostChecks is every prerequisite a reachable host is judged on.
func hostChecks(spec inventory.HostSpec, observed map[string]any) []any {
	operatingSystem := object(observed, "os")
	ansible := object(observed, "ansible")
	systemd := object(observed, "systemd_user")
	checks := []any{
		check("canonical-hostname",
			observed["hostname"] == spec.CanonicalHostname,
			observed["hostname"], spec.CanonicalHostname),
		check("machine-identity",
			observed["machine_id_sha256"] == spec.MachineIDSHA256,
			observed["machine_id_sha256"], spec.MachineIDSHA256),
		check("user", observed["user"] == spec.User, observed["user"], spec.User),
		check("architecture",
			observed["architecture"] == spec.Architecture,
			observed["architecture"], spec.Architecture),
		check("os",
			operatingSystem["id"] == spec.OSID,
			observed["os"],
			map[string]any{"id": spec.OSID, "version": spec.OSVersion}),
		check("python", truthy(object(observed, "python")["version"]), observed["python"], nil),
		check("ansible-controller", ansible["available"] == true, observed["ansible"], nil),
		check("systemd-user", systemd["state"] == "running", systemd["state"], "running"),
		check("linger", systemd["linger"] == "yes", systemd["linger"], "yes"),
		check("forgejo-access",
			observed["forgejo_access"] == true, observed["forgejo_access"], true),
		check("stable-address",
			spec.StableAddressVerified, spec.StableAddressVerified, true),
	}
	// The source checks follow in the order the probe reported them, which is
	// sorted, so that two reports of the same fleet compare line for line.
	sources := object(observed, "source_access")
	for _, component := range safety.SortedKeys(sources) {
		access := object(sources, component)
		checks = append(checks, check(
			"source-access:"+component, access["reachable"] == true, sources[component], true,
		))
	}
	return checks
}

// componentStatus judges one component on one host.
//
// The three sources of truth are kept apart on purpose: what the manifest
// desires, what is installed on disk, and what is actually activated and
// running. A component can be installed and not running, and reporting that as
// convergence is how a fleet drifts silently.
func componentStatus(name string, desired map[string]any, observed map[string]any) map[string]any {
	pin, _ := desired["pin"].(map[string]any)
	var desiredCommit any
	var desiredVersion any
	if len(pin) > 0 {
		desiredCommit = pin["value"]
		desiredVersion = pin["version"]
	}

	var installedCommit any
	var installedVersion any
	var activated any
	var running any

	switch name {
	case "t3-steward":
		installedVersion = observed["version"]
		if line, ok := installedVersion.(string); ok {
			if match := stewardCommitPattern.FindStringSubmatch(line); match != nil {
				installedCommit = match[1]
			}
		}
		service := object(observed, "service")
		activated = map[string]any{
			"service_state": service["ActiveState"],
			"unit_state":    service["UnitFileState"],
		}
		// The running commit is deliberately null: a running service reports
		// its unit state, not the revision its process was started from.
		running = map[string]any{"service_state": service["ActiveState"], "commit": nil}
	case "agent99", "nvim-configuration", "homelab-cli":
		checkout := object(observed, "checkout")
		installedCommit = checkout["head"]
		present, found := checkout["present"]
		if !found {
			present = false
		}
		activated = map[string]any{"checkout_present": present}
		if name == "agent99" {
			bridges, _ := observed["active_bridges"].([]any)
			running = map[string]any{"active_processes": len(bridges), "commit": nil}
		}
	default:
		exists, found := object(observed, "config")["exists"]
		if !found {
			exists = false
		}
		activated = map[string]any{"config_present": exists}
	}

	return map[string]any{
		"name":          name,
		"state":         convergenceState(desiredCommit, installedCommit),
		"desired":       map[string]any{"commit": desiredCommit, "version": desiredVersion},
		"installed":     map[string]any{"commit": installedCommit, "version": installedVersion},
		"activated":     activated,
		"running":       running,
		"dirty_entries": object(observed, "checkout")["dirty_entries"],
	}
}

// convergenceState compares what is desired with what is installed.
//
// A component with no pin is not drift: nobody has decided what it should be
// yet, and a report that called that drift would train operators to ignore it.
func convergenceState(desiredCommit any, installedCommit any) string {
	if desiredCommit == nil {
		return "decision-required"
	}
	if installedCommit == nil {
		return "unknown"
	}
	if desiredCommit == installedCommit {
		return "converged"
	}
	// A host may report an abbreviated commit. Seven characters is the shortest
	// prefix Git itself will hand out, and anything shorter is not evidence.
	desired, desiredIsText := desiredCommit.(string)
	installed, installedIsText := installedCommit.(string)
	if desiredIsText && installedIsText &&
		len(installed) >= 7 && strings.HasPrefix(desired, installed) {
		return "converged"
	}
	return "drift"
}

// observation returns one host's raw document, or an empty one when the host
// was never observed at all.
func observation(observations map[string]any, name string) map[string]any {
	document, _ := observations[name].(map[string]any)
	if document == nil {
		return map[string]any{}
	}
	return document
}

// object returns a nested document, or an empty one when the key is absent or
// holds something else.
func object(document map[string]any, key string) map[string]any {
	nested, _ := document[key].(map[string]any)
	if nested == nil {
		return map[string]any{}
	}
	return nested
}

// list returns a nested array, or an empty one when the key is absent.
func list(document map[string]any, key string) []any {
	items, _ := document[key].([]any)
	if items == nil {
		return []any{}
	}
	return items
}

// reachable returns the reachability a host reported, defaulting to false for a
// host that reported nothing.
func reachable(observed map[string]any) any {
	value, found := observed["reachable"]
	if !found {
		return false
	}
	return value
}

// errorOf names why a host is offline. A host that is simply absent reports no
// error of its own, so the report supplies one rather than printing null.
func errorOf(observed map[string]any) any {
	value, found := observed["error"]
	if !found || value == nil {
		return "unreachable"
	}
	return value
}

// truthy judges a probed value the way the Python implementation did, because
// a probe document may carry a missing key, a null, an empty string or a number
// where a boolean is expected.
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		number, err := typed.Float64()
		return err != nil || number != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}
