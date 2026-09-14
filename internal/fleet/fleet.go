// Package fleet is the Steward fleet intent that UpKeeper owns.
//
// One authored document describes the whole Steward fleet: the coordinator and
// its client endpoint, reusable host profiles, one entry per worker host with
// its explicit exceptions, and the projects a worker may be offered. Three
// deterministic projections are rendered from that one source: the worker
// bootstrap a host runs on, the client endpoint a non-coordinator CLI talks
// to, and the catalog input the coordinator is asked to accept.
//
// The boundary this package exists to hold is that a model allowlist is
// authorization and never evidence. UpKeeper reports what intent desires and,
// where it genuinely has one, what a host was observed to have. It never
// computes an effective set, because only the coordinator holds the accepted
// snapshot a planner reads, and it never enrolls anything.
//
// The contract is frozen in docs/steward-fleet-configuration-v1.md and its
// reasoning is in docs/architecture/adr-h2-steward-fleet-configuration.md.
package fleet

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
)

// Component is the release component name this package owns.
const Component = "steward-fleet-configuration"

// SchemaVersion is the only fleet intent schema this binary understands. An
// unknown version fails closed rather than being partially applied.
const SchemaVersion = 1

const (
	// WorkerDestination is the host-relative path of the worker projection. It
	// is the same file worker-configuration v1 writes, on purpose: for a host
	// whose intent has not changed the bytes are identical.
	WorkerDestination = ".config/t3-steward/worker-bootstrap.json"
	// ClientDestination is the host-relative path of the client projection. It
	// is UpKeeper's own file rather than a section of the configuration
	// t3-steward authors, so that one document never has two authors: the
	// runtime reads this file the way it reads the worker bootstrap, and an
	// explicit backlog_v2.coordinator_client block overrides it.
	ClientDestination = ".config/t3-steward/coordinator-client.json"
	// CoordinatorKind labels the proposed catalog input. It is a proposal: no
	// UpKeeper code path applies it.
	CoordinatorKind = "steward-coordinator-catalog-input"
	// StateDirectory is the run-private directory the previous document is
	// retained under, mirroring worker-configuration v1's prior.json.
	StateDirectory = ".local/state/dev-fleet/fleet-config"
)

// ComponentFields is the closed top-level field set. observed is the only
// optional key, and it is written by capture rather than authored.
var ComponentFields = []string{
	"kind", "schema_version", "coordinator", "profiles", "hosts", "projects",
}

// ClientFields is the closed field set of the client projection. It is closed
// for the same reason the worker bootstrap's is: a document that may carry
// anything is a document a credential value can be smuggled into.
var ClientFields = []string{
	"schema_version",
	"coordinator_id",
	"address",
	"connection",
	"remote_command",
	"credential_ref",
	"request_timeout",
	"message_limits",
}

// MessageLimitFields is the closed field set of a client's message limits.
var MessageLimitFields = []string{"max_request_bytes", "max_response_bytes"}

// DefaultMessageLimits are the limits a client endpoint gets when the authored
// document does not state them. They are explicit values rather than "no
// limit", because an unbounded client is one malformed response away from
// exhausting the memory of whatever host ran the command.
var DefaultMessageLimits = MessageLimits{
	MaxRequestBytes:  1 << 20,
	MaxResponseBytes: 8 << 20,
}

// maximumMessageBytes bounds an authored limit. A limit larger than this is
// not a limit.
const maximumMessageBytes = 1 << 30

// ProfileFields, HostFields and ProjectFields are the keys every entry of that
// kind must carry. Additional keys are preserved rather than refused, because
// silently dropping a field a newer controller wrote turns a rollback into
// data loss; see roundTrips for the condition preservation depends on.
var (
	ProfileFields = []string{
		"cpu_class", "executor_slots", "capabilities",
		"provider_instances", "models", "quota_pools",
	}
	HostFields = []string{
		"profile", "worker_id", "transport", "credential_ref", "admin_credential_ref",
	}
	ProjectFields = []string{
		"repository", "default_ref", "setup_profile", "eligible_workers",
	}
)

// RequiredProfiles are the profiles the contract insists every fleet document
// defines, so that a host always has a named class to belong to.
var RequiredProfiles = []string{"build", "standard"}

// RequiredCapabilities are what a Steward worker cannot run without.
var RequiredCapabilities = []string{"git", "huyang"}

const (
	// WorkerSecretPrefix and AdminSecretPrefix keep the two credential
	// namespaces apart, so that distributing one never distributes the other.
	WorkerSecretPrefix = "secretref:f02-protocol/"
	AdminSecretPrefix  = "secretref:f03-admin/"
)

var (
	namePattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	modelPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	gitRefPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	// repositoryPattern accepts the canonical remote spellings the fleet uses.
	// UpKeeper never fetches this remote; it hands the string to the
	// coordinator, so the check is that the value cannot be mistaken for an
	// option or carry anything but a repository path.
	repositoryPattern = regexp.MustCompile(
		`^(?:https|ssh)://(?:git@)?[A-Za-z0-9.-]+(?::[0-9]{1,5})?/[A-Za-z0-9][A-Za-z0-9._/-]*\.git$`)
	commandPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._/-]{0,199}$`)
)

// MessageLimits bound what a client will send and accept.
type MessageLimits struct {
	MaxRequestBytes  int
	MaxResponseBytes int
}

// ClientEndpoint is how a non-coordinator CLI reaches the coordinator.
type ClientEndpoint struct {
	Address        string
	Connection     string
	RemoteCommand  string
	RequestTimeout int
	MessageLimits  MessageLimits
}

// Coordinator is the fleet's single coordinator identity.
type Coordinator struct {
	ID       string
	Host     string
	Endpoint ClientEndpoint
}

// Profile is a reusable class of host.
type Profile struct {
	CPUClass          string
	ExecutorSlots     int
	Capabilities      []string
	ProviderInstances []string
	// Models maps a provider instance to its explicit desired model allowlist.
	// It authorizes a model. It never proves one is installed, authenticated
	// or advertised.
	Models     map[string][]string
	QuotaPools []string
}

// Host is one worker host's intent.
type Host struct {
	Name               string
	Profile            string
	WorkerID           string
	Transport          string
	CredentialRef      string
	AdminCredentialRef string
	// Exceptions override profile keys for this host alone. They are explicit
	// on purpose: an exception that has to be written down is an exception
	// somebody reviewed.
	Exceptions map[string]any
}

// Project is one Steward project.
type Project struct {
	Name            string
	Repository      string
	DefaultRef      string
	SetupProfile    string
	EligibleWorkers []string
}

// Document is a parsed and validated fleet intent component.
type Document struct {
	raw         map[string]any
	rawProfiles map[string]map[string]any
	Coordinator Coordinator
	Profiles    map[string]Profile
	Hosts       map[string]Host
	Projects    map[string]Project
	observed    map[string]any
}

// Raw returns the component exactly as it was authored, so that a caller which
// has to write the manifest back preserves every field this binary does not
// understand.
func (d *Document) Raw() map[string]any { return d.raw }

// HostNames, ProfileNames and ProjectNames are sorted, so that every report and
// every projection this package renders is in one deterministic order.
func (d *Document) HostNames() []string    { return sortedKeys(d.Hosts) }
func (d *Document) ProfileNames() []string { return sortedKeys(d.Profiles) }
func (d *Document) ProjectNames() []string { return sortedKeys(d.Projects) }

// Observed is what capture recorded, or nil when nothing was ever observed.
// A nil observation and an empty one are different answers and must stay
// distinguishable, which is why this returns the second result.
func (d *Document) Observed() (map[string]any, bool) {
	if d.observed == nil {
		return nil, false
	}
	return d.observed, true
}

// Parse validates a fleet intent component and returns it.
//
// Everything is checked here rather than at the point of use, so that a
// document which parses can be rendered, projected and reported on without any
// further validation and without any chance of a partially applied change.
func Parse(component map[string]any) (*Document, error) {
	if component == nil {
		return nil, failure.Value("steward fleet configuration is missing")
	}
	if !roundTrips(component) {
		return nil, failure.Value(
			"steward fleet configuration cannot be preserved byte-for-byte")
	}
	if component["kind"] != Component {
		return nil, failure.Value("steward fleet configuration has the wrong kind")
	}
	version, numbered := intValue(component["schema_version"])
	if !numbered || version != SchemaVersion {
		return nil, failure.Value(
			"unsupported steward-fleet-configuration schema_version")
	}
	if err := exactKeys(component, ComponentFields, []string{"observed"}); err != nil {
		return nil, failure.Value(
			"steward fleet configuration carries an unknown top-level field")
	}
	document := &Document{raw: component}
	if err := document.parseCoordinator(component["coordinator"]); err != nil {
		return nil, err
	}
	if err := document.parseProfiles(component["profiles"]); err != nil {
		return nil, err
	}
	if err := document.parseHosts(component["hosts"]); err != nil {
		return nil, err
	}
	if err := document.parseProjects(component["projects"]); err != nil {
		return nil, err
	}
	if err := document.parseObserved(component); err != nil {
		return nil, err
	}
	return document, nil
}

// CheckInventory refuses a document whose hosts disagree with the inventory.
// It is separate from Parse because the inventory is not available everywhere
// a document is read: the target-side agent has the document and its own name,
// not the fleet's host list.
func (d *Document) CheckInventory(known map[string]bool) error {
	if known == nil {
		return nil
	}
	for _, name := range d.HostNames() {
		if !known[name] {
			return failure.Value("fleet host %s is not in the inventory", name)
		}
	}
	if !known[d.Coordinator.Host] {
		return failure.Value(
			"fleet coordinator host %s is not in the inventory", d.Coordinator.Host)
	}
	return nil
}

func (d *Document) parseCoordinator(value any) error {
	entry, ok := value.(map[string]any)
	if !ok {
		return failure.Value("fleet coordinator must be an object")
	}
	if err := exactKeys(entry,
		[]string{"coordinator_id", "host", "client_endpoint"}, nil); err != nil {
		return failure.Value("fleet coordinator is missing %s", err.Error())
	}
	identity, _ := entry["coordinator_id"].(string)
	host, _ := entry["host"].(string)
	if !namePattern.MatchString(identity) || !namePattern.MatchString(host) {
		return failure.Value("invalid fleet coordinator identity")
	}
	endpoint, ok := entry["client_endpoint"].(map[string]any)
	if !ok {
		return failure.Value("fleet coordinator client_endpoint must be an object")
	}
	// message_limits is optional so that a document authored against the
	// frozen four-key shape still validates; an absent one takes the explicit
	// default rather than meaning "unbounded".
	if err := exactKeys(endpoint,
		[]string{"address", "connection", "remote_command", "request_timeout"},
		[]string{"message_limits"}); err != nil {
		return failure.Value("fleet client endpoint is missing %s", err.Error())
	}
	limits, err := parseMessageLimits(endpoint["message_limits"])
	if err != nil {
		return err
	}
	address, _ := endpoint["address"].(string)
	command, _ := endpoint["remote_command"].(string)
	timeout, numbered := intValue(endpoint["request_timeout"])
	if !namePattern.MatchString(address) && !hostAddressPattern.MatchString(address) {
		return failure.Value("invalid fleet client endpoint address")
	}
	if endpoint["connection"] != "ssh" {
		return failure.Value("fleet client endpoint connection must be ssh")
	}
	if !commandPattern.MatchString(command) {
		return failure.Value("invalid fleet client endpoint remote_command")
	}
	if !numbered || timeout <= 0 || timeout > 3600 {
		return failure.Value(
			"fleet client endpoint request_timeout must be 1 through 3600 seconds")
	}
	d.Coordinator = Coordinator{
		ID:   identity,
		Host: host,
		Endpoint: ClientEndpoint{
			Address:        address,
			Connection:     "ssh",
			RemoteCommand:  command,
			RequestTimeout: timeout,
			MessageLimits:  limits,
		},
	}
	return nil
}

func parseMessageLimits(value any) (MessageLimits, error) {
	if value == nil {
		return DefaultMessageLimits, nil
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return MessageLimits{}, failure.Value("fleet client message_limits must be an object")
	}
	if err := exactKeys(entry, MessageLimitFields, nil); err != nil {
		return MessageLimits{}, failure.Value(
			"fleet client message_limits must name only %s",
			strings.Join(MessageLimitFields, " and "))
	}
	limits := MessageLimits{}
	for _, pair := range []struct {
		key   string
		store *int
	}{
		{"max_request_bytes", &limits.MaxRequestBytes},
		{"max_response_bytes", &limits.MaxResponseBytes},
	} {
		size, numbered := intValue(entry[pair.key])
		if !numbered || size < 1 || size > maximumMessageBytes {
			return MessageLimits{}, failure.Value(
				"fleet client %s must be 1 through %d bytes", pair.key, maximumMessageBytes)
		}
		*pair.store = size
	}
	return limits, nil
}

var hostAddressPattern = regexp.MustCompile(
	`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

func (d *Document) parseProfiles(value any) error {
	profiles, ok := value.(map[string]any)
	if !ok || len(profiles) == 0 {
		return failure.Value("fleet profiles must be a non-empty object")
	}
	d.Profiles = map[string]Profile{}
	d.rawProfiles = map[string]map[string]any{}
	for _, name := range sortedKeys(profiles) {
		entry, isObject := profiles[name].(map[string]any)
		if !namePattern.MatchString(name) || !isObject {
			return failure.Value("invalid fleet profile name")
		}
		if err := exactKeys(entry, ProfileFields, anyKey); err != nil {
			return failure.Value("fleet profile %s is missing %s", name, err.Error())
		}
		profile, err := parseProfileBody(name, entry)
		if err != nil {
			return err
		}
		d.Profiles[name] = profile
		d.rawProfiles[name] = entry
	}
	for _, required := range RequiredProfiles {
		if _, present := d.Profiles[required]; !present {
			return failure.Value("fleet profiles must define %s", required)
		}
	}
	return nil
}

func parseProfileBody(name string, entry map[string]any) (Profile, error) {
	class, _ := entry["cpu_class"].(string)
	if !identifierPattern.MatchString(class) {
		return Profile{}, failure.Value("fleet profile %s has an invalid cpu_class", name)
	}
	slots, numbered := intValue(entry["executor_slots"])
	if !numbered || slots < 1 || slots > 64 {
		return Profile{}, failure.Value(
			"fleet profile %s must declare 1 through 64 executor slots", name)
	}
	capabilities, err := identifierList(entry["capabilities"], name+".capabilities")
	if err != nil {
		return Profile{}, err
	}
	instances, err := identifierList(entry["provider_instances"], name+".provider_instances")
	if err != nil {
		return Profile{}, err
	}
	pools, err := identifierList(entry["quota_pools"], name+".quota_pools")
	if err != nil {
		return Profile{}, err
	}
	models, err := parseModels(entry["models"], instances, name)
	if err != nil {
		return Profile{}, err
	}
	return Profile{
		CPUClass:          class,
		ExecutorSlots:     slots,
		Capabilities:      capabilities,
		ProviderInstances: instances,
		Models:            models,
		QuotaPools:        pools,
	}, nil
}

// parseModels reads a desired model allowlist. Every provider instance it
// names must be one the profile actually runs, because authorizing a model on
// an instance that is not there is authorization nobody can act on and nobody
// can see is dead.
func parseModels(value any, instances []string, context string) (map[string][]string, error) {
	entry, ok := value.(map[string]any)
	if !ok {
		return nil, failure.Value("fleet profile %s models must be an object", context)
	}
	allowed := map[string]bool{}
	for _, instance := range instances {
		allowed[instance] = true
	}
	models := map[string][]string{}
	for _, instance := range sortedKeys(entry) {
		if !allowed[instance] {
			return nil, failure.Value(
				"fleet profile %s allows models on an instance it does not run: %s",
				context, instance)
		}
		values, err := stringList(entry[instance])
		if err != nil || !sortedAndUnique(values) {
			return nil, failure.Value(
				"fleet profile %s model allowlist must be sorted and unique", context)
		}
		for _, model := range values {
			if !modelPattern.MatchString(model) {
				return nil, failure.Value(
					"fleet profile %s has an invalid model identifier", context)
			}
		}
		models[instance] = values
	}
	return models, nil
}

func (d *Document) parseHosts(value any) error {
	hosts, ok := value.(map[string]any)
	if !ok {
		return failure.Value("fleet hosts must be an object")
	}
	d.Hosts = map[string]Host{}
	for _, name := range sortedKeys(hosts) {
		entry, isObject := hosts[name].(map[string]any)
		if !namePattern.MatchString(name) || !isObject {
			return failure.Value("invalid fleet host")
		}
		if err := exactKeys(entry, HostFields, anyKey); err != nil {
			return failure.Value("fleet host %s is missing %s", name, err.Error())
		}
		host, err := d.parseHostBody(name, entry)
		if err != nil {
			return err
		}
		d.Hosts[name] = host
	}
	return nil
}

func (d *Document) parseHostBody(name string, entry map[string]any) (Host, error) {
	profile, _ := entry["profile"].(string)
	if _, present := d.Profiles[profile]; !present {
		return Host{}, failure.Value("fleet host %s names an unknown profile", name)
	}
	if entry["worker_id"] != name {
		return Host{}, failure.Value("fleet worker identity must match its inventory host")
	}
	if entry["transport"] != "ssh" {
		return Host{}, failure.Value("fleet worker transport must be ssh")
	}
	if entry["credential_ref"] != WorkerSecretPrefix+name {
		return Host{}, failure.Value(
			"fleet worker needs its own opaque f02-protocol secret reference")
	}
	if entry["admin_credential_ref"] != AdminSecretPrefix+name {
		return Host{}, failure.Value(
			"fleet worker needs its own opaque f03-admin secret reference")
	}
	exceptions, err := d.parseExceptions(name, profile, entry["exceptions"])
	if err != nil {
		return Host{}, err
	}
	return Host{
		Name:               name,
		Profile:            profile,
		WorkerID:           name,
		Transport:          "ssh",
		CredentialRef:      WorkerSecretPrefix + name,
		AdminCredentialRef: AdminSecretPrefix + name,
		Exceptions:         exceptions,
	}, nil
}

// parseExceptions insists that every override names a key the profile already
// has. An exception for a key nothing reads is a silent no-op, and a silent
// no-op in authorization is the failure this component exists to remove.
func (d *Document) parseExceptions(name, profile string, value any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	entry, ok := value.(map[string]any)
	if !ok || len(entry) == 0 {
		return nil, failure.Value(
			"fleet host %s exceptions must be a non-empty object when present", name)
	}
	base := d.rawProfiles[profile]
	for _, key := range sortedKeys(entry) {
		if _, present := base[key]; !present {
			return nil, failure.Value(
				"fleet host %s overrides a key its profile does not have: %s", name, key)
		}
	}
	// The override has to produce a profile that is itself valid, so the
	// merged body is parsed exactly as an authored profile would be.
	merged := map[string]any{}
	for key, item := range base {
		merged[key] = item
	}
	for key, item := range entry {
		merged[key] = item
	}
	if _, err := parseProfileBody(profile, merged); err != nil {
		return nil, failure.Value("fleet host %s: %s", name, err.Error())
	}
	return entry, nil
}

func (d *Document) parseProjects(value any) error {
	projects, ok := value.(map[string]any)
	if !ok {
		return failure.Value("fleet projects must be an object")
	}
	d.Projects = map[string]Project{}
	for _, name := range sortedKeys(projects) {
		entry, isObject := projects[name].(map[string]any)
		if !namePattern.MatchString(name) || !isObject {
			return failure.Value("invalid fleet project name")
		}
		if err := exactKeys(entry, ProjectFields, anyKey); err != nil {
			return failure.Value("fleet project %s is missing %s", name, err.Error())
		}
		repository, _ := entry["repository"].(string)
		if !repositoryPattern.MatchString(repository) {
			return failure.Value(
				"fleet project %s needs a canonical https or ssh repository", name)
		}
		ref, _ := entry["default_ref"].(string)
		if !gitRefPattern.MatchString(ref) {
			return failure.Value("fleet project %s has an invalid default_ref", name)
		}
		setup, _ := entry["setup_profile"].(string)
		if !identifierPattern.MatchString(setup) {
			return failure.Value("fleet project %s has an invalid setup_profile", name)
		}
		workers, err := identifierList(entry["eligible_workers"], name+".eligible_workers")
		if err != nil {
			return err
		}
		for _, worker := range workers {
			if _, present := d.Hosts[worker]; !present {
				return failure.Value(
					"fleet project %s is eligible for an undeclared worker: %s", name, worker)
			}
		}
		d.Projects[name] = Project{
			Name:            name,
			Repository:      repository,
			DefaultRef:      ref,
			SetupProfile:    setup,
			EligibleWorkers: workers,
		}
	}
	return nil
}

// parseObserved checks the one field capture may write. Observations are never
// authorization, so the only thing validated here is that they describe hosts
// this document actually declares.
func (d *Document) parseObserved(component map[string]any) error {
	value, present := component["observed"]
	if !present || value == nil {
		return nil
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return failure.Value("fleet observed must be an object")
	}
	if err := exactKeys(entry, nil, []string{"hosts", "coordinator"}); err != nil {
		return failure.Value("fleet observed carries an unknown field")
	}
	hosts, _ := entry["hosts"].(map[string]any)
	for _, name := range sortedKeys(hosts) {
		if _, declared := d.Hosts[name]; !declared {
			return failure.Value("fleet observed names an undeclared host: %s", name)
		}
		if _, isObject := hosts[name].(map[string]any); !isObject {
			return failure.Value("fleet observation for %s must be an object", name)
		}
	}
	d.observed = entry
	return nil
}

// roundTrips reports whether a value survives the canonical encoding unchanged.
// Unknown fields are preserved only when this holds; anything else is refused,
// because a field that cannot be written back byte-for-byte cannot be rolled
// back to either.
func roundTrips(value any) bool {
	encoded, err := canonjson.Compact(value)
	if err != nil {
		return false
	}
	decoded, err := canonjson.Decode(encoded)
	if err != nil {
		return false
	}
	again, err := canonjson.Compact(decoded)
	if err != nil {
		return false
	}
	return string(encoded) == string(again)
}

// anyKey marks a field set that accepts additional keys beyond the required
// ones. It is distinguishable from an empty optional list, which accepts none.
var anyKey = []string{"*"}

// exactKeys checks that every required key is present and that any other key
// is either explicitly optional or allowed by anyKey. The error names the
// first missing key, which is what the caller reports.
func exactKeys(value map[string]any, required, optional []string) error {
	for _, key := range required {
		if _, present := value[key]; !present {
			return failure.Value("%s", key)
		}
	}
	if len(optional) == 1 && optional[0] == "*" {
		return nil
	}
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for _, key := range sortedKeys(value) {
		if !allowed[key] {
			return failure.Value("%s", key)
		}
	}
	return nil
}

func identifierList(value any, context string) ([]string, error) {
	values, err := stringList(value)
	if err != nil || !sortedAndUnique(values) {
		return nil, failure.Value("fleet %s must be a sorted, unique string list", context)
	}
	for _, item := range values {
		if !identifierPattern.MatchString(item) {
			return nil, failure.Value("fleet %s has an invalid identifier", context)
		}
	}
	return values, nil
}

func stringList(value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, failure.Value("not a list")
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, failure.Value("not a string list")
		}
		values = append(values, text)
	}
	return values, nil
}

func sortedAndUnique(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func intValue(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	if err != nil {
		return 0, false
	}
	return int(parsed), true
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
