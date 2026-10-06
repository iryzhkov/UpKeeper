package release

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/pyfmt"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// EnvironmentValidator checks the agent-environment component.
//
// It is injected rather than imported so that the manifest model stays
// independent of the projection that applies it; the command layer wires the
// real implementation in once, at start-up.
var EnvironmentValidator func(component map[string]any) error

// ValidateRef accepts a Git ref that is safe to pass to git as an argument.
func ValidateRef(value, context string) (string, error) {
	if !refPattern.MatchString(value) ||
		strings.Contains(value, "..") ||
		strings.HasSuffix(value, "/") ||
		strings.Contains(value, "@{") {
		return "", failure.Validation("%s.ref is not a safe git ref: %s", context, pyfmt.Repr(value))
	}
	return value, nil
}

// ValidateVersion accepts a published release version and refuses anything that
// looks like a local build, because a local build cannot be reinstalled later
// from its published artifacts.
func ValidateVersion(value, context string) (string, error) {
	if !versionPattern.MatchString(value) {
		return "", failure.Validation(
			"%s.version is not a release version: %s", context, pyfmt.Repr(value))
	}
	if localBuildPattern.MatchString(value) {
		return "", failure.Validation(
			"%s.version is a local build, not a release: %s", context, pyfmt.Repr(value))
	}
	return value, nil
}

// Validate checks a whole manifest and returns it as a Release.
func Validate(manifest map[string]any) (Release, error) {
	if !isSchemaVersion(manifest["schema_version"]) {
		return Release{}, failure.Validation("unsupported release schema_version")
	}
	for _, key := range []string{"captured_at", "captured_from"} {
		if _, err := requiredString(manifest, key, "release"); err != nil {
			return Release{}, err
		}
	}
	capturedFrom, _ := manifest["captured_from"].(string)
	if _, err := safety.ValidateHostName(capturedFrom, nil); err != nil {
		return Release{}, err
	}
	components, ok := manifest["components"].(map[string]any)
	if !ok || !isKnownComponentSet(components) {
		return Release{}, failure.Validation(
			"release components must be exactly %s", strings.Join(ComponentOrder, ", "))
	}
	for _, name := range validationOrder(components) {
		component, ok := components[name].(map[string]any)
		if !ok {
			return Release{}, failure.Validation("component %s must be an object", name)
		}
		if err := validateComponent(name, component, components); err != nil {
			return Release{}, err
		}
	}
	if _, present := components["agent-environment"]; present {
		agentConfig, _ := components["agent-config"].(map[string]any)
		if files, _ := agentConfig["files"].([]any); len(files) > 0 {
			return Release{}, failure.Validation(
				"agent-config must be empty when agent-environment owns its files")
		}
		setup, _ := components["omarchy-setup"].(map[string]any)
		if !stringListEquals(setup["modules"], []string{"28-scripts"}) {
			return Release{}, failure.Validation(
				"release environment replaces the 45-agents module")
		}
	}
	return Release{Manifest: manifest}, nil
}

func validateComponent(name string, component, components map[string]any) error {
	kind, err := requiredString(component, "kind", name)
	if err != nil {
		return err
	}
	switch name {
	case ProgramComponent:
		_, err := DecodeProgramPin(component)
		return err
	case JocastaComponent:
		_, err := DecodeJocasta(component)
		return err
	case ControlledCodexComponent:
		_, err := DecodeControlledCodex(component)
		return err
	case ToolingComponent:
		_, err := DecodeTooling(component)
		return err
	case "agent-environment":
		if EnvironmentValidator == nil {
			return failure.Validation(
				"invalid agent-environment: no environment validator is wired in")
		}
		if err := EnvironmentValidator(component); err != nil {
			return failure.Validation("invalid agent-environment: %s", err.Error())
		}
		return nil
	case "worker-configuration":
		if err := ValidateWorkers(component); err != nil {
			return failure.Validation("invalid worker configuration: %s", err.Error())
		}
		return nil
	case fleet.Component:
		if err := ValidateFleetConfiguration(component); err != nil {
			return failure.Validation(
				"invalid steward fleet configuration: %s", err.Error())
		}
		return nil
	case "t3":
		return validateT3(component, components, kind, name)
	}
	switch kind {
	case "git-checkout":
		return validateCheckout(name, component)
	case "github-release":
		return validateGithubRelease(name, component, components)
	case "files":
		return validateFiles(name, component, components)
	default:
		return failure.Validation("%s.kind %s is not supported", name, pyfmt.Repr(kind))
	}
}

func validateT3(component, components map[string]any, kind, name string) error {
	if !hasExactKeys(component, "kind", "package", "version", "compatibility", "service") && !hasExactKeys(component, "kind", "package", "version", "compatibility", "service", "artifact") {
		return failure.Validation("invalid T3 component fields") // Legacy and artifact pins have exact shapes.
	}
	if kind != "npm-package" || component["package"] != "t3" ||
		component["service"] != "t3code.service" {
		return failure.Validation("T3 must be the pinned t3 npm package and service")
	}
	version, err := requiredString(component, "version", name)
	if err != nil {
		return err
	}
	if _, err := ValidateVersion(version, name); err != nil {
		return err
	}
	bounds, ok := component["compatibility"].(map[string]any)
	if !ok || !hasExactKeys(bounds, "min", "max", "steward_version") {
		return failure.Validation("T3 compatibility must name min, max and steward_version")
	}
	for _, bound := range []string{"min", "max"} {
		if !numericVersionPattern.MatchString(scalarString(bounds[bound])) {
			return failure.Validation("T3 compatibility requires numeric release bounds")
		}
	}
	steward, _ := components["t3-steward"].(map[string]any)
	if bounds["steward_version"] != steward["version"] {
		return failure.Validation("T3 is outside the captured steward compatibility range")
	}
	minimum, err := versionTuple(scalarString(bounds["min"]))
	if err != nil {
		return err
	}
	maximum, err := versionTuple(scalarString(bounds["max"]))
	if err != nil {
		return err
	}
	current, err := versionTuple(version)
	if err != nil {
		return err
	}
	if compareVersions(minimum, current) > 0 || compareVersions(current, maximum) > 0 {
		return failure.Validation("T3 is outside the captured steward compatibility range")
	}
	if raw, present := component["artifact"]; present {
		if _, err := DecodeT3Artifact(raw, version); err != nil {
			return err
		}
	}
	return nil
}

func validateCheckout(name string, component map[string]any) error {
	source, err := requiredString(component, "source", name)
	if err != nil {
		return err
	}
	if _, err := safety.ValidateSourceURL(source); err != nil {
		return err
	}
	ref, err := requiredString(component, "ref", name)
	if err != nil {
		return err
	}
	if _, err := ValidateRef(ref, name); err != nil {
		return err
	}
	commit, err := requiredString(component, "commit", name)
	if err != nil {
		return err
	}
	if _, err := safety.ValidateGitCommit(commit); err != nil {
		return err
	}
	path, err := requiredString(component, "path", name)
	if err != nil {
		return err
	}
	if _, err := safety.ValidateHomeRelativePath(path, HomeRoots); err != nil {
		return err
	}
	for _, key := range []string{"build", "modules"} {
		value, present := component[key]
		if !present || value == nil {
			continue
		}
		if _, ok := stringList(value); !ok {
			return failure.Validation("%s.%s must be a string list", name, key)
		}
	}
	modules, _ := stringList(component["modules"])
	for _, module := range modules {
		if !modulePattern.MatchString(module) {
			return failure.Validation(
				"%s.modules contains an unsafe name: %s", name, pyfmt.Repr(module))
		}
	}
	build, _ := stringList(component["build"])
	for _, word := range build {
		if !buildWordPattern.MatchString(word) {
			return failure.Validation(
				"%s.build contains an unsafe word: %s", name, pyfmt.Repr(word))
		}
	}
	if service, present := component["service"]; present {
		if !unitNamePattern.MatchString(scalarString(service)) {
			return failure.Validation("%s.service is not a user unit name", name)
		}
	}
	probe, present := component["probe"]
	if !present || probe == nil {
		return nil
	}
	typed, ok := probe.(map[string]any)
	if !ok || typed["kind"] != "huyang-mcp" || !isPositiveInteger(typed["tools"]) {
		return failure.Validation(
			"%s.probe must be a huyang-mcp probe with a tool count", name)
	}
	return nil
}

func validateGithubRelease(name string, component, components map[string]any) error {
	repository, err := requiredString(component, "repository", name)
	if err != nil {
		return err
	}
	if !githubRepositoryPattern.MatchString(repository) {
		return failure.Validation("%s.repository must be owner/name", name)
	}
	version, err := requiredString(component, "version", name)
	if err != nil {
		return err
	}
	if _, err := ValidateVersion(version, name); err != nil {
		return err
	}
	if _, present := component["session_hook_sha256"]; present {
		hook, err := requiredString(component, "session_hook_sha256", name)
		if err != nil {
			return err
		}
		if !sha256Pattern.MatchString(hook) {
			return failure.Validation("invalid steward session hook checksum")
		}
	}
	path, err := requiredString(component, "path", name)
	if err != nil {
		return err
	}
	if _, err := safety.ValidateHomeRelativePath(path, HomeRoots); err != nil {
		return err
	}
	// A release that carries the T3 pair is installed by UpKeeper itself; the
	// older releases delegated to the legacy t3-update installer.
	installer := "t3-update"
	if _, present := components["t3"]; present {
		installer = "upkeeper"
	}
	if component["installer"] != installer {
		return failure.Validation("%s.installer must be %s", name, installer)
	}
	service, err := requiredString(component, "service", name)
	if err != nil {
		return err
	}
	if !unitNamePattern.MatchString(service) {
		return failure.Validation("%s.service is not a user unit name", name)
	}
	return nil
}

func validateFiles(name string, component, components map[string]any) error {
	files, ok := component["files"].([]any)
	if !ok {
		return failure.Validation("%s.files must be a non-empty list", name)
	}
	if len(files) == 0 {
		if _, present := components["agent-environment"]; !present {
			return failure.Validation("%s.files must be a non-empty list", name)
		}
	}
	for _, item := range files {
		entry, ok := item.(map[string]any)
		if !ok {
			return failure.Validation("%s.files entries must be objects", name)
		}
		path, err := requiredString(entry, "path", name)
		if err != nil {
			return err
		}
		if _, err := safety.ValidateHomeRelativePath(path, HomeRoots); err != nil {
			return err
		}
		store, err := requiredString(entry, "store", name)
		if err != nil {
			return err
		}
		if !storePattern.MatchString(store) {
			return failure.Validation("%s.files store path is unsafe", name)
		}
		digest, err := requiredString(entry, "sha256", name)
		if err != nil {
			return err
		}
		if !sha256Pattern.MatchString(digest) {
			return failure.Validation("%s.files sha256 is invalid", name)
		}
	}
	return nil
}

// validationOrder walks the components in convergence order and then any names
// the release carries that this binary does not know, so that a refusal is
// always reported at the same component regardless of map iteration order.
func validationOrder(components map[string]any) []string {
	ordered := make([]string, 0, len(components))
	seen := map[string]bool{}
	if _, present := components[ProgramComponent]; present {
		ordered = append(ordered, ProgramComponent)
		seen[ProgramComponent] = true
	}
	for _, name := range ComponentOrder {
		if _, present := components[name]; present {
			ordered = append(ordered, name)
			seen[name] = true
		}
	}
	remaining := make([]string, 0)
	for name := range components {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(ordered, remaining...)
}

// isKnownComponentSet accepts the component set of a release this binary can
// converge: the legacy set exactly, or the current set with any optional
// component absent. A release that omits an optional component is not an
// incomplete release; it is a release captured before that component existed,
// and it must converge unchanged.
func isKnownComponentSet(components map[string]any) bool {
	if sameNameSet(components, LegacyComponentOrder) {
		return true
	}
	known := make(map[string]bool, len(ComponentOrder)+1)
	for _, name := range ComponentOrder {
		known[name] = true
	}
	known[ProgramComponent] = true
	for name := range components {
		if !known[name] {
			return false
		}
	}
	for _, name := range ComponentOrder {
		if _, present := components[name]; !present && !OptionalComponents[name] {
			return false
		}
	}
	return true
}

func sameNameSet(components map[string]any, names []string) bool {
	if len(components) != len(names) {
		return false
	}
	for _, name := range names {
		if _, present := components[name]; !present {
			return false
		}
	}
	return true
}

func hasExactKeys(value map[string]any, keys ...string) bool {
	if len(value) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, present := value[key]; !present {
			return false
		}
	}
	return true
}

func requiredString(mapping map[string]any, key, context string) (string, error) {
	value, ok := mapping[key].(string)
	if !ok || value == "" {
		return "", failure.Validation("%s.%s must be a non-empty string", context, key)
	}
	return value, nil
}

func stringList(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		values = append(values, text)
	}
	return values, true
}

func stringListEquals(value any, expected []string) bool {
	items, ok := stringList(value)
	if !ok || len(items) != len(expected) {
		return false
	}
	for index := range items {
		if items[index] != expected[index] {
			return false
		}
	}
	return true
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	default:
		return ""
	}
}

func isSchemaVersion(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	parsed, err := number.Int64()
	return err == nil && parsed == SchemaVersion
}

func isPositiveInteger(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	if strings.ContainsAny(number.String(), ".eE") {
		return false
	}
	parsed, err := number.Int64()
	return err == nil && parsed > 0
}

// versionTuple parses a strictly numeric version, which is what a T3 pair bound
// must be: a prerelease bound would make the range meaningless.
func versionTuple(value string) ([]int, error) {
	if !numericVersionPattern.MatchString(value) {
		return nil, failure.Validation("T3 pair requires a stable numeric npm version")
	}
	parts := strings.Split(value, ".")
	numbers := make([]int, 0, len(parts))
	for _, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, failure.Validation("T3 pair requires a stable numeric npm version")
		}
		numbers = append(numbers, number)
	}
	return numbers, nil
}

func compareVersions(left, right []int) int {
	for index := 0; index < len(left) && index < len(right); index++ {
		if left[index] != right[index] {
			if left[index] < right[index] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	default:
		return 0
	}
}
