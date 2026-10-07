// Package inventory loads the two sources of truth a controller reads before
// it does anything: which machines exist, and which components they should run.
//
// Both files are named *.yml for historical reasons but contain JSON, which is
// deliberate: a JSON subset has one unambiguous parse, so the fleet needs no
// YAML implementation on any host. Every field is validated on the way in, so
// that nothing loaded here can later become a shell fragment on a target.
package inventory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// HostSpec is one machine as the inventory describes it.
type HostSpec struct {
	Name                  string  `json:"name"`
	Address               string  `json:"address"`
	User                  string  `json:"user"`
	CanonicalHostname     string  `json:"canonical_hostname"`
	Architecture          string  `json:"architecture"`
	OSID                  string  `json:"os_id"`
	OSVersion             string  `json:"os_version"`
	MachineIDSHA256       string  `json:"machine_id_sha256"`
	SSHHostED25519SHA256  string  `json:"ssh_host_ed25519_sha256"`
	StableAddressVerified bool    `json:"stable_address_verified"`
	SSHAlias              *string `json:"ssh_alias"`
	StableName            *string `json:"stable_name"`
	NebulaAddress         *string `json:"nebula_address"`
	SetupCheckout         *string `json:"setup_checkout"`
	SelfManaged           bool    `json:"self_managed"`
	// InstallOSPackages opts the host in to having missing OS prerequisites
	// installed by the agent, which it does only with passwordless sudo.
	InstallOSPackages bool `json:"install_os_packages"`
}

// Config is the validated pair of sources of truth.
type Config struct {
	Root     string
	Hosts    map[string]HostSpec
	Manifest map[string]any
}

// Components returns the desired component definitions.
func (c *Config) Components() map[string]any {
	components, _ := c.Manifest["components"].(map[string]any)
	return components
}

// HostNames returns every inventory host in report order.
func (c *Config) HostNames() []string {
	names := make([]string, 0, len(c.Hosts))
	for name := range c.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// KnownHosts returns the host set that selectors are validated against.
func (c *Config) KnownHosts() map[string]bool {
	known := make(map[string]bool, len(c.Hosts))
	for name := range c.Hosts {
		known[name] = true
	}
	return known
}

// Load reads and validates both files below root.
func Load(root string) (*Config, error) {
	resolved, err := filepath.Abs(root)
	if err != nil {
		return nil, failure.Validation("cannot resolve the configuration root: %v", err)
	}
	hosts, err := LoadInventory(filepath.Join(resolved, "inventory", "hosts.yml"))
	if err != nil {
		return nil, err
	}
	manifest, err := LoadManifest(filepath.Join(resolved, "desired", "components.yml"))
	if err != nil {
		return nil, err
	}
	return &Config{Root: resolved, Hosts: hosts, Manifest: manifest}, nil
}

func loadJSONDocument(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, failure.Validation("cannot load %s: %v", filepath.Base(path), err)
	}
	return decodeJSONDocument(data, filepath.Base(path))
}

func decodeJSONDocument(data []byte, name string) (map[string]any, error) {
	value, err := canonjson.Decode(data)
	if err != nil {
		return nil, failure.Validation("cannot load %s: %v", name, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, failure.Validation("%s must contain an object", name)
	}
	return object, nil
}

// LoadInventory validates inventory/hosts.yml.
func LoadInventory(path string) (map[string]HostSpec, error) {
	document, err := loadJSONDocument(path)
	if err != nil {
		return nil, err
	}
	return inventoryHosts(document)
}

// ParseInventory validates the bytes of an inventory/hosts.yml, such as the
// version a Git commit holds.
func ParseInventory(data []byte) (map[string]HostSpec, error) {
	document, err := decodeJSONDocument(data, "hosts.yml")
	if err != nil {
		return nil, err
	}
	return inventoryHosts(document)
}

func inventoryHosts(document map[string]any) (map[string]HostSpec, error) {
	all, _ := document["all"].(map[string]any)
	vars, _ := all["vars"].(map[string]any)
	if !isSchemaVersion(vars["fleet_inventory_schema_version"]) {
		return nil, failure.Validation("unsupported inventory schema_version")
	}
	rawHosts, ok := all["hosts"].(map[string]any)
	if !ok || len(rawHosts) == 0 {
		return nil, failure.Validation("inventory all.hosts must be a non-empty object")
	}
	hosts := make(map[string]HostSpec, len(rawHosts))
	for _, name := range safety.SortedKeys(rawHosts) {
		if _, err := safety.ValidateHostName(name, nil); err != nil {
			return nil, err
		}
		raw, ok := rawHosts[name].(map[string]any)
		if !ok {
			return nil, failure.Validation("inventory host %s must be an object", name)
		}
		spec, err := hostSpec(name, raw)
		if err != nil {
			return nil, err
		}
		hosts[name] = spec
	}
	return hosts, nil
}

func hostSpec(name string, raw map[string]any) (HostSpec, error) {
	required := func(key string) (string, error) {
		return requiredString(raw, key, name)
	}
	rawAddress, err := required("ansible_host")
	if err != nil {
		return HostSpec{}, err
	}
	address, err := safety.ValidateAddress(rawAddress)
	if err != nil {
		return HostSpec{}, err
	}
	user, err := required("ansible_user")
	if err != nil {
		return HostSpec{}, err
	}
	if !safety.ValidateUnixUser(user) {
		return HostSpec{}, failure.Validation("%s.ansible_user is invalid", name)
	}
	canonical, err := required("canonical_hostname")
	if err != nil {
		return HostSpec{}, err
	}
	if _, err := safety.ValidateAddress(canonical); err != nil {
		return HostSpec{}, err
	}
	machineHash, err := required("machine_id_sha256")
	if err != nil {
		return HostSpec{}, err
	}
	if !safety.MachineHashPattern.MatchString(machineHash) {
		return HostSpec{}, failure.Validation("%s.machine_id_sha256 is invalid", name)
	}
	fingerprint, err := required("ssh_host_ed25519_sha256")
	if err != nil {
		return HostSpec{}, err
	}
	if !safety.SSHFingerprintPattern.MatchString(fingerprint) {
		return HostSpec{}, failure.Validation("%s.ssh_host_ed25519_sha256 is invalid", name)
	}
	stable, ok := raw["stable_address_verified"].(bool)
	if !ok {
		return HostSpec{}, failure.Validation("%s.stable_address_verified must be boolean", name)
	}
	spec := HostSpec{
		Name:                  name,
		Address:               address,
		User:                  user,
		CanonicalHostname:     canonical,
		MachineIDSHA256:       machineHash,
		SSHHostED25519SHA256:  fingerprint,
		StableAddressVerified: stable,
	}
	for _, key := range []string{"stable_name", "nebula_address"} {
		value, present := optionalString(raw, key)
		if !present {
			continue
		}
		if _, err := safety.ValidateAddress(value); err != nil {
			return HostSpec{}, err
		}
		if key == "stable_name" {
			spec.StableName = &value
		} else {
			spec.NebulaAddress = &value
		}
	}
	if alias, present := optionalString(raw, "ssh_alias"); present {
		if _, err := safety.ValidateHostName(alias, nil); err != nil {
			return HostSpec{}, err
		}
		spec.SSHAlias = &alias
	}
	if checkout, present := optionalString(raw, "setup_checkout"); present {
		if _, err := safety.ValidateHomeRelativePath(
			checkout, map[string]bool{".local": true, "Work": true},
		); err != nil {
			return HostSpec{}, err
		}
		spec.SetupCheckout = &checkout
	}
	if value, present := raw["self_managed"]; present && value != nil {
		managed, ok := value.(bool)
		if !ok {
			return HostSpec{}, failure.Validation("%s.self_managed must be boolean", name)
		}
		spec.SelfManaged = managed
	}
	if value, present := raw["install_os_packages"]; present && value != nil {
		install, ok := value.(bool)
		if !ok {
			return HostSpec{}, failure.Validation("%s.install_os_packages must be boolean", name)
		}
		spec.InstallOSPackages = install
	}
	for _, key := range []string{"architecture", "os_id", "os_version"} {
		value, err := required(key)
		if err != nil {
			return HostSpec{}, err
		}
		switch key {
		case "architecture":
			spec.Architecture = value
		case "os_id":
			spec.OSID = value
		case "os_version":
			spec.OSVersion = value
		}
	}
	return spec, nil
}

// LoadManifest validates desired/components.yml.
func LoadManifest(path string) (map[string]any, error) {
	document, err := loadJSONDocument(path)
	if err != nil {
		return nil, err
	}
	if !isSchemaVersion(document["schema_version"]) {
		return nil, failure.Validation("unsupported desired-state schema_version")
	}
	enabled, ok := document["deployment_enabled"].(bool)
	if !ok {
		return nil, failure.Validation("deployment_enabled must be boolean")
	}
	components, ok := document["components"].(map[string]any)
	if !ok || len(components) == 0 {
		return nil, failure.Validation("components must be a non-empty object")
	}
	names := make(map[string]bool, len(components))
	for name := range components {
		names[name] = true
	}
	for _, name := range safety.SortedKeys(components) {
		if _, err := safety.ValidateComponentName(name, nil); err != nil {
			return nil, err
		}
		component, ok := components[name].(map[string]any)
		if !ok {
			return nil, failure.Validation("component %s must be an object", name)
		}
		if err := validateComponent(name, component, names, enabled); err != nil {
			return nil, err
		}
	}
	if err := validateDependencyGraph(components); err != nil {
		return nil, err
	}
	return document, nil
}

func validateComponent(name string, component map[string]any, names map[string]bool, enabled bool) error {
	source, ok := component["source"].(map[string]any)
	if !ok {
		return failure.Validation("%s.source must be an object", name)
	}
	canonical, err := requiredString(source, "canonical", name)
	if err != nil {
		return err
	}
	if _, err := safety.ValidateSourceURL(canonical); err != nil {
		return err
	}
	mirrors, ok := source["allowed_mirrors"].([]any)
	if !ok {
		return failure.Validation("%s.source.allowed_mirrors must be a string list", name)
	}
	for _, mirror := range mirrors {
		text, ok := mirror.(string)
		if !ok {
			return failure.Validation("%s.source.allowed_mirrors must be a string list", name)
		}
		if _, err := safety.ValidateSourceURL(text); err != nil {
			return err
		}
	}

	switch pin := component["pin"].(type) {
	case nil:
		if enabled {
			return failure.Validation(
				"%s has no immutable pin while deployment is enabled", name)
		}
	case map[string]any:
		if pin["kind"] != "git" {
			return failure.Validation("%s.pin must be a git pin or null", name)
		}
		value, err := requiredString(pin, "value", name)
		if err != nil {
			return err
		}
		if _, err := safety.ValidateGitCommit(value); err != nil {
			return err
		}
	default:
		return failure.Validation("%s.pin must be a git pin or null", name)
	}

	paths, ok := component["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		return failure.Validation("%s.paths must be a non-empty object", name)
	}
	for _, key := range safety.SortedKeys(paths) {
		text, ok := paths[key].(string)
		if !ok {
			return failure.Validation("%s.paths values must be strings", name)
		}
		if _, err := safety.ValidateHomeRelativePath(text, nil); err != nil {
			return err
		}
	}

	architectures, ok := component["architectures"].([]any)
	if !ok || len(architectures) == 0 {
		return failure.Validation("%s.architectures must be non-empty", name)
	}
	for _, item := range architectures {
		if item != "aarch64" && item != "x86_64" {
			return failure.Validation("%s.architectures contains an unsupported value", name)
		}
	}

	dependencies, ok := component["dependencies"].([]any)
	if !ok {
		return failure.Validation("%s.dependencies contains an unknown component", name)
	}
	for _, item := range dependencies {
		text, ok := item.(string)
		if !ok || !names[text] {
			return failure.Validation("%s.dependencies contains an unknown component", name)
		}
	}

	order, ok := component["activation_order"].(json.Number)
	if !ok {
		return failure.Validation("%s.activation_order must be a non-negative integer", name)
	}
	value, err := order.Int64()
	if err != nil || value < 0 {
		return failure.Validation("%s.activation_order must be a non-negative integer", name)
	}
	return nil
}

func validateDependencyGraph(components map[string]any) error {
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(name string) error
	visit = func(name string) error {
		if visiting[name] {
			return failure.Validation("component dependency graph contains a cycle")
		}
		if visited[name] {
			return nil
		}
		visiting[name] = true
		component, _ := components[name].(map[string]any)
		dependencies, _ := component["dependencies"].([]any)
		for _, dependency := range dependencies {
			text, _ := dependency.(string)
			if err := visit(text); err != nil {
				return err
			}
		}
		delete(visiting, name)
		visited[name] = true
		return nil
	}
	for _, name := range safety.SortedKeys(components) {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func requiredString(mapping map[string]any, key, context string) (string, error) {
	value, ok := mapping[key].(string)
	if !ok || value == "" {
		return "", failure.Validation("%s.%s must be a non-empty string", context, key)
	}
	return value, nil
}

func optionalString(mapping map[string]any, key string) (string, bool) {
	value, present := mapping[key]
	if !present || value == nil {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func isSchemaVersion(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	parsed, err := number.Int64()
	return err == nil && parsed == exitcode.SchemaVersion
}
