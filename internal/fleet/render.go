package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
)

// Resolved is one host's effective intent: its profile with its explicit
// exceptions applied.
//
// Resolution is a pure function of the document, so the same intent resolves to
// the same values on every host and on every run. Nothing about the machine
// this runs on takes part.
func (d *Document) Resolved(name string) (Profile, error) {
	host, present := d.Hosts[name]
	if !present {
		return Profile{}, failure.Value("fleet host %s is not declared", name)
	}
	base := d.rawProfiles[host.Profile]
	if len(host.Exceptions) == 0 {
		return d.Profiles[host.Profile], nil
	}
	merged := make(map[string]any, len(base))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range host.Exceptions {
		merged[key] = value
	}
	return parseProfileBody(host.Profile, merged)
}

// WorkerProjection renders the bootstrap document for one host.
//
// Its field set is exactly worker-configuration v1's, and the values come from
// the same places v1 got them, so a host whose intent has not changed receives
// byte-identical bytes. That is the migration safety property: adding this
// component changes nothing on a host until its intent is actually edited.
func (d *Document) WorkerProjection(name string) (map[string]any, error) {
	host, present := d.Hosts[name]
	if !present {
		return nil, failure.Value("fleet host %s is not declared", name)
	}
	profile, err := d.Resolved(name)
	if err != nil {
		return nil, err
	}
	capabilities := sortedUnique(profile.Capabilities)
	for _, required := range RequiredCapabilities {
		if !contains(capabilities, required) {
			return nil, failure.Value("fleet worker must declare git and huyang")
		}
	}
	return map[string]any{
		"schema_version":  1,
		"worker_id":       host.WorkerID,
		"coordinator_id":  d.Coordinator.ID,
		"transport":       host.Transport,
		"capabilities":    capabilities,
		"provider_routes": sortedUnique(profile.ProviderInstances),
		"credential_ref":  host.CredentialRef,
	}, nil
}

// WorkerBytes are the exact bytes of the worker projection on disk: the
// indented, key-sorted JSON with a trailing newline that worker-configuration
// v1 already writes.
func (d *Document) WorkerBytes(name string) ([]byte, error) {
	projection, err := d.WorkerProjection(name)
	if err != nil {
		return nil, err
	}
	return canonjson.MarshalFile(projection)
}

// ClientProjection renders the coordinator client one host's CLI talks to. The
// host contributes only its own admin credential reference; everything else is
// the fleet's single coordinator endpoint.
func (d *Document) ClientProjection(name string) (map[string]any, error) {
	host, present := d.Hosts[name]
	if !present {
		return nil, failure.Value("fleet host %s is not declared", name)
	}
	return map[string]any{
		"address":              d.Coordinator.Endpoint.Address,
		"admin_credential_ref": host.AdminCredentialRef,
		"connection":           d.Coordinator.Endpoint.Connection,
		"coordinator_id":       d.Coordinator.ID,
		"remote_command":       d.Coordinator.Endpoint.RemoteCommand,
		"request_timeout":      d.Coordinator.Endpoint.RequestTimeout,
	}, nil
}

// ClientBytes render the client projection as the YAML fragment that belongs
// under backlog_v2.coordinator_client in a host's t3-steward configuration.
//
// The emitter handles exactly the shape this projection has, which is a map of
// string and integer scalars: every string is written as a double-quoted
// scalar, so no value can ever be reinterpreted as a YAML keyword, and the keys
// are sorted, so the bytes are the same on every run.
func (d *Document) ClientBytes(name string) ([]byte, error) {
	projection, err := d.ClientProjection(name)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	out.WriteString("backlog_v2:\n  coordinator_client:\n")
	keys := make([]string, 0, len(projection))
	for key := range projection {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out.WriteString("    " + key + ": ")
		switch typed := projection[key].(type) {
		case string:
			out.WriteString(canonjson.EscapeASCII(typed))
		case int:
			out.WriteString(strconv.Itoa(typed))
		default:
			return nil, failure.Value("client projection carries an unrenderable value")
		}
		out.WriteString("\n")
	}
	return []byte(out.String()), nil
}

// CoordinatorProjection renders the catalog input the coordinator is asked to
// accept.
//
// It is a proposal and nothing else. UpKeeper never applies it: the coordinator
// owns the accepted catalog, and giving one document two authors is how a
// reload that failed validation would leave the fleet's authorization state
// ambiguous. Credential references stay out of it, because a catalog is
// authorization and a reference is host-local distribution.
func (d *Document) CoordinatorProjection() (map[string]any, error) {
	workers := map[string]any{}
	for _, name := range d.HostNames() {
		profile, err := d.Resolved(name)
		if err != nil {
			return nil, err
		}
		models := map[string]any{}
		for instance, allowed := range profile.Models {
			models[instance] = sortedUnique(allowed)
		}
		workers[name] = map[string]any{
			"worker_id":          name,
			"cpu_class":          profile.CPUClass,
			"executor_slots":     profile.ExecutorSlots,
			"capabilities":       sortedUnique(profile.Capabilities),
			"provider_instances": sortedUnique(profile.ProviderInstances),
			"desired_models":     models,
			"quota_pools":        sortedUnique(profile.QuotaPools),
		}
	}
	projects := map[string]any{}
	for _, name := range d.ProjectNames() {
		project := d.Projects[name]
		projects[name] = map[string]any{
			"repository":       project.Repository,
			"default_ref":      project.DefaultRef,
			"setup_profile":    project.SetupProfile,
			"eligible_workers": sortedUnique(project.EligibleWorkers),
		}
	}
	return map[string]any{
		"kind":           CoordinatorKind,
		"schema_version": SchemaVersion,
		"coordinator_id": d.Coordinator.ID,
		"workers":        workers,
		"projects":       projects,
	}, nil
}

// CoordinatorBytes are the canonical bytes of the proposed catalog input. Their
// digest is what an enrollment plan proposes.
func (d *Document) CoordinatorBytes() ([]byte, error) {
	projection, err := d.CoordinatorProjection()
	if err != nil {
		return nil, err
	}
	return canonjson.MarshalFile(projection)
}

// Digest is the SHA-256 of rendered bytes, in the lowercase hexadecimal form
// every identity in UpKeeper is written in.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sortedUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
