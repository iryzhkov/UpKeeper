package release

import (
	"regexp"
	"sort"

	"github.com/iryzhkov/UpKeeper/internal/failure"
)

var (
	workerNamePattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	workerIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// WorkerBootstrapDestination is where a host's worker bootstrap is projected.
const WorkerBootstrapDestination = ".config/t3-steward/worker-bootstrap.json"

// WorkerFields are the only fields a worker bootstrap entry may carry. The set
// is closed on purpose: catalogs and secrets must never be distributed through
// the release manifest, so an unexpected field is a refusal rather than a
// value that is quietly ignored.
var WorkerFields = []string{
	"schema_version",
	"worker_id",
	"coordinator_id",
	"transport",
	"capabilities",
	"provider_routes",
	"credential_ref",
}

// ValidateWorkers checks the worker-configuration component of a release.
func ValidateWorkers(component map[string]any) error {
	if !hasExactKeys(component, "kind", "hosts") || component["kind"] != "worker-configuration" {
		return failure.Value("worker-configuration must contain only kind and hosts")
	}
	hosts, ok := component["hosts"].(map[string]any)
	if !ok {
		return failure.Value("worker hosts must be an object")
	}
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, host := range names {
		entry, ok := hosts[host].(map[string]any)
		if !workerNamePattern.MatchString(host) || !ok {
			return failure.Value("invalid worker host")
		}
		if err := validateWorkerEntry(host, entry); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkerEntry(host string, entry map[string]any) error {
	if !hasExactKeys(entry, WorkerFields...) {
		return failure.Value("unknown worker bootstrap fields; catalogs and secrets are forbidden")
	}
	if !isSchemaVersion(entry["schema_version"]) || entry["worker_id"] != host {
		return failure.Value("worker identity must match its inventory host")
	}
	coordinator, _ := entry["coordinator_id"].(string)
	if !workerNamePattern.MatchString(coordinator) {
		return failure.Value("invalid coordinator identity")
	}
	if entry["transport"] != "ssh" {
		return failure.Value("remote worker bootstrap transport must be ssh")
	}
	if entry["credential_ref"] != "secretref:f02-protocol/"+host {
		return failure.Value("worker needs its own opaque f02-protocol secret reference")
	}
	for _, field := range []string{"capabilities", "provider_routes"} {
		values, ok := stringList(entry[field])
		if !ok || !isSortedAndUnique(values) {
			return failure.Value("capabilities and provider routes must be sorted and unique")
		}
		for _, value := range values {
			if !workerIdentifierPattern.MatchString(value) {
				return failure.Value("invalid capability or route identifier")
			}
		}
	}
	capabilities, _ := stringList(entry["capabilities"])
	if !containsAll(capabilities, "git", "huyang") {
		return failure.Value("worker must declare git and huyang")
	}
	return nil
}

func isSortedAndUnique(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func containsAll(values []string, required ...string) bool {
	present := make(map[string]bool, len(values))
	for _, value := range values {
		present[value] = true
	}
	for _, name := range required {
		if !present[name] {
			return false
		}
	}
	return true
}
