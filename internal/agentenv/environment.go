// Package agentenv holds the frozen agent-environment contract and the
// owned-file projections that put it on a target machine.
//
// The contract itself is immutable: UpKeeper owns deployment progression and
// this package supplies the payload that is folded into the plan hash, plus
// fail-closed drift, probe and recovery inventory projections. The spec type in
// this file never writes a managed target; assets.go does that.
//
// The revision of a spec is a durable identity. It is the SHA-256 of the
// compact JSON encoding, which is deliberately different from the indented
// encoding the release manifest uses, and it may never drift.
package agentenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/pyfmt"
)

// Adapter is the only environment adapter this contract supports.
const Adapter = "local-single-worker-v1"

// SecretRefPrefix marks an opaque, target-local credential reference. A release
// carries the reference and never the secret it names.
const SecretRefPrefix = "secretref:"

// Phases is the fixed transition order every environment deployment follows.
var Phases = []string{"install", "activate", "probe", "rollback"}

var assetKinds = map[string]bool{
	"mcp-service":        true,
	"mcp-client":         true,
	"skill":              true,
	"plugin":             true,
	"harness-setting":    true,
	"global-instruction": true,
}

var recoveryKinds = map[string]bool{
	"lease": true, "t3-thread": true, "huyang-journal": true, "worktree": true,
}

// requiredSafetyClaims are the claims the fixed probes must cover between them.
// An environment whose probes prove less than this is refused.
var requiredSafetyClaims = []string{
	"explicit-effect-authority",
	"instruction-safety",
	"preserve-unexplained-changes",
	"skill-safety",
}

// Drift states. A distinct state per failure mode is deliberate: an observer
// that cannot answer must not be confused with a target that is missing.
const (
	DriftMatch       = "MATCH"
	DriftMismatch    = "MISMATCH"
	DriftMissing     = "MISSING"
	DriftUnavailable = "UNAVAILABLE"
	DriftUnexplained = "UNEXPLAINED"
)

// ManagedAsset is one file the environment owns, in both its staged and its
// active location.
type ManagedAsset struct {
	AssetID         string `json:"asset_id"`
	Kind            string `json:"kind"`
	Provenance      string `json:"provenance"`
	SourceRevision  string `json:"source_revision"`
	SHA256          string `json:"sha256"`
	SizeBytes       int64  `json:"size_bytes"`
	StagedPath      string `json:"staged_path"`
	ActivePath      string `json:"active_path"`
	ActivationOrder int    `json:"activation_order"`
}

// Validate reports whether the asset is a usable managed identity.
func (a ManagedAsset) Validate() error {
	if err := validateSafeText(a.AssetID, "asset_id"); err != nil {
		return err
	}
	if !assetKinds[a.Kind] {
		return fmt.Errorf("unsupported managed asset kind")
	}
	if err := validateSafeText(a.Provenance, "provenance"); err != nil {
		return err
	}
	if err := validateSafeText(a.SourceRevision, "source_revision"); err != nil {
		return err
	}
	if err := validateDigest(a.SHA256, "asset sha256"); err != nil {
		return err
	}
	if a.SizeBytes < 0 {
		return fmt.Errorf("asset size_bytes must be nonnegative")
	}
	if err := validateAbsolute(a.StagedPath, "staged_path"); err != nil {
		return err
	}
	if err := validateAbsolute(a.ActivePath, "active_path"); err != nil {
		return err
	}
	if filepath.Clean(a.StagedPath) == filepath.Clean(a.ActivePath) {
		return fmt.Errorf("install staging must be distinct from activation")
	}
	if a.ActivationOrder < 0 {
		return fmt.Errorf("activation_order must be nonnegative")
	}
	return nil
}

// PriorAsset is the exact active identity in the coherent rollback environment.
type PriorAsset struct {
	AssetID    string `json:"asset_id"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	ActivePath string `json:"active_path"`
}

// Validate reports whether the prior asset can restore a coherent environment.
func (a PriorAsset) Validate() error {
	if err := validateSafeText(a.AssetID, "prior asset_id"); err != nil {
		return err
	}
	if err := validateDigest(a.SHA256, "prior asset sha256"); err != nil {
		return err
	}
	if a.SizeBytes < 0 {
		return fmt.Errorf("prior asset size_bytes must be nonnegative")
	}
	return validateAbsolute(a.ActivePath, "prior active_path")
}

// ProjectInstructionRef pins the repository revision an instruction file came
// from, so that a target can prove which instructions it is running.
type ProjectInstructionRef struct {
	Repository         string `json:"repository"`
	RepositoryRevision string `json:"repository_revision"`
	Path               string `json:"path"`
	SHA256             string `json:"sha256"`
}

// Validate reports whether the instruction reference is fully pinned.
func (r ProjectInstructionRef) Validate() error {
	if err := validateSafeText(r.Repository, "instruction repository"); err != nil {
		return err
	}
	if len(r.RepositoryRevision) != 40 || !isLowerHex(r.RepositoryRevision) {
		return fmt.Errorf("repository_revision must be a full Git object id")
	}
	if err := validateRelative(r.Path, "instruction path"); err != nil {
		return err
	}
	return validateDigest(r.SHA256, "instruction sha256")
}

// ID is the identity an instruction reference is sorted and compared by.
func (r ProjectInstructionRef) ID() string {
	return r.Repository + "@" + r.RepositoryRevision + ":" + r.Path
}

// BehavioralProbe is a fixed command whose success stands for a safety claim.
type BehavioralProbe struct {
	ProbeID        string   `json:"probe_id"`
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	SafetyClaims   []string `json:"safety_claims"`
}

// Validate reports whether the probe is a bounded, attributable check.
func (p BehavioralProbe) Validate() error {
	if err := validateSafeText(p.ProbeID, "probe_id"); err != nil {
		return err
	}
	if len(p.Argv) == 0 || !filepath.IsAbs(p.Argv[0]) {
		return fmt.Errorf(
			"probe argv must be a nonempty argument vector with absolute executable")
	}
	for _, item := range p.Argv {
		if err := validateSafeText(item, "probe argv item"); err != nil {
			return err
		}
	}
	if p.TimeoutSeconds < 1 || p.TimeoutSeconds > 300 {
		return fmt.Errorf("probe timeout must be between 1 and 300 seconds")
	}
	if len(p.SafetyClaims) == 0 || !isSortedUnique(p.SafetyClaims) {
		return fmt.Errorf("safety_claims must be nonempty, unique, and sorted")
	}
	for _, claim := range p.SafetyClaims {
		if err := validateSafeText(claim, "safety claim"); err != nil {
			return err
		}
	}
	return nil
}

// RecoverySource is a directory another tool owns, named here only so that a
// convergence can report on it without taking responsibility for it.
type RecoverySource struct {
	Kind  string `json:"kind"`
	Owner string `json:"owner"`
	Root  string `json:"root"`
}

// Validate reports whether the recovery source is attributable.
func (s RecoverySource) Validate() error {
	if !recoveryKinds[s.Kind] {
		return fmt.Errorf("unsupported recovery source kind")
	}
	if err := validateSafeText(s.Owner, "recovery owner"); err != nil {
		return err
	}
	return validateAbsolute(s.Root, "recovery root")
}

// Spec is the complete, frozen agent-environment contract for one host.
type Spec struct {
	SchemaVersion               int                     `json:"schema_version"`
	Adapter                     string                  `json:"adapter"`
	HostProfile                 string                  `json:"host_profile"`
	WorkerIDs                   []string                `json:"worker_ids"`
	Phases                      []string                `json:"phases"`
	Assets                      []ManagedAsset          `json:"assets"`
	ProjectInstructions         []ProjectInstructionRef `json:"project_instructions"`
	Probes                      []BehavioralProbe       `json:"probes"`
	RecoverySources             []RecoverySource        `json:"recovery_sources"`
	SecretRefs                  []string                `json:"secret_refs"`
	PreviousEnvironmentRevision string                  `json:"previous_environment_revision"`
	PriorAssets                 []PriorAsset            `json:"prior_assets"`
}

// specFields is the exact field set a serialized spec must carry, no more and
// no fewer, so that an unknown field can never be silently ignored.
var specFields = []string{
	"schema_version", "adapter", "host_profile", "worker_ids", "phases", "assets",
	"project_instructions", "probes", "recovery_sources", "secret_refs",
	"previous_environment_revision", "prior_assets",
}

// Validate enforces every invariant of the frozen contract. It is called on
// construction and on decoding, so a Spec value in hand is always valid.
func (s *Spec) Validate() error {
	if s.SchemaVersion != 1 {
		return fmt.Errorf("AgentEnvironmentSpec schema_version must be 1")
	}
	if s.Adapter != Adapter {
		return fmt.Errorf("F05 supports only the explicit %s adapter", Adapter)
	}
	if err := validateSafeText(s.HostProfile, "host_profile"); err != nil {
		return err
	}
	if len(s.WorkerIDs) != 1 {
		return fmt.Errorf("the provisional F05 adapter requires exactly one worker")
	}
	if err := validateSafeText(s.WorkerIDs[0], "worker_id"); err != nil {
		return err
	}
	if !equalStrings(s.Phases, Phases) {
		return fmt.Errorf("environment phases must be install, activate, probe, rollback")
	}
	if err := s.validateMembers(); err != nil {
		return err
	}
	if err := s.validateIdentities(); err != nil {
		return err
	}
	if err := s.validatePaths(); err != nil {
		return err
	}
	if err := validateDigest(
		s.PreviousEnvironmentRevision, "previous_environment_revision"); err != nil {
		return err
	}
	if err := s.validatePriorAssets(); err != nil {
		return err
	}
	if err := s.validateInstructionsAndProbes(); err != nil {
		return err
	}
	return s.validateSecretRefs()
}

func (s *Spec) validateMembers() error {
	for _, asset := range s.Assets {
		if err := asset.Validate(); err != nil {
			return err
		}
	}
	for _, reference := range s.ProjectInstructions {
		if err := reference.Validate(); err != nil {
			return err
		}
	}
	for _, probe := range s.Probes {
		if err := probe.Validate(); err != nil {
			return err
		}
	}
	for _, source := range s.RecoverySources {
		if err := source.Validate(); err != nil {
			return err
		}
	}
	for _, prior := range s.PriorAssets {
		if err := prior.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Spec) validateIdentities() error {
	groups := []struct {
		label  string
		values []string
	}{
		{"asset", mapSlice(s.Assets, func(a ManagedAsset) string { return a.AssetID })},
		{"probe", mapSlice(s.Probes, func(p BehavioralProbe) string { return p.ProbeID })},
		{"recovery source",
			mapSlice(s.RecoverySources, func(r RecoverySource) string { return r.Kind })},
	}
	for _, group := range groups {
		if len(group.values) == 0 || !isSortedUnique(group.values) {
			return fmt.Errorf("%s identities must be nonempty, unique, and sorted", group.label)
		}
	}
	return nil
}

func (s *Spec) validatePaths() error {
	orders := make([]int, len(s.Assets))
	for index, asset := range s.Assets {
		orders[index] = asset.ActivationOrder
	}
	if !sort.IntsAreSorted(orders) {
		return fmt.Errorf("assets must be sorted by activation_order")
	}
	active := map[string]bool{}
	staged := map[string]bool{}
	for _, asset := range s.Assets {
		active[asset.ActivePath] = true
		staged[asset.StagedPath] = true
	}
	if len(active) != len(s.Assets) {
		return fmt.Errorf("active paths must be unique")
	}
	if len(staged) != len(s.Assets) {
		return fmt.Errorf("staged paths must be unique")
	}
	// Staging that sits inside an active target, or contains one, would make an
	// install step change the environment it is supposed to leave alone.
	for _, one := range s.Assets {
		for _, other := range s.Assets {
			if pathsOverlap(one.StagedPath, other.ActivePath) {
				return fmt.Errorf("all install staging paths must be distinct from active paths")
			}
		}
	}
	return nil
}

func (s *Spec) validatePriorAssets() error {
	priorIDs := mapSlice(s.PriorAssets, func(a PriorAsset) string { return a.AssetID })
	desiredIDs := mapSlice(s.Assets, func(a ManagedAsset) string { return a.AssetID })
	if !equalStrings(priorIDs, desiredIDs) {
		return fmt.Errorf("prior assets must exactly cover desired assets in canonical order")
	}
	priorPaths := mapSlice(s.PriorAssets, func(a PriorAsset) string { return a.ActivePath })
	desiredPaths := mapSlice(s.Assets, func(a ManagedAsset) string { return a.ActivePath })
	if !equalStrings(priorPaths, desiredPaths) {
		return fmt.Errorf("prior assets must restore the same active paths")
	}
	return nil
}

func (s *Spec) validateInstructionsAndProbes() error {
	instructionIDs := mapSlice(
		s.ProjectInstructions, func(r ProjectInstructionRef) string { return r.ID() })
	if len(instructionIDs) == 0 || !isSortedUnique(instructionIDs) {
		return fmt.Errorf("project instruction identities must be nonempty, unique, and sorted")
	}
	claims := map[string]bool{}
	for _, probe := range s.Probes {
		for _, claim := range probe.SafetyClaims {
			claims[claim] = true
		}
	}
	for _, required := range requiredSafetyClaims {
		if !claims[required] {
			return fmt.Errorf("fixed probes do not cover required instruction and skill safety")
		}
	}
	return nil
}

func (s *Spec) validateSecretRefs() error {
	for _, reference := range s.SecretRefs {
		if err := validateSafeText(reference, "secret reference"); err != nil {
			return err
		}
		suffix := strings.TrimPrefix(reference, SecretRefPrefix)
		if !strings.HasPrefix(reference, SecretRefPrefix) || suffix == "" ||
			!isOpaqueReference(suffix) {
			return fmt.Errorf("only opaque secretref: identifiers are allowed")
		}
	}
	if !isSortedUnique(s.SecretRefs) {
		return fmt.Errorf("secret_refs must be unique and sorted")
	}
	return nil
}

// normalized returns a copy whose slices are never nil, so that the JSON
// encoding writes [] where Python wrote an empty tuple rather than null.
func (s *Spec) normalized() Spec {
	copied := *s
	copied.WorkerIDs = nonNilStrings(copied.WorkerIDs)
	copied.Phases = nonNilStrings(copied.Phases)
	copied.SecretRefs = nonNilStrings(copied.SecretRefs)
	if copied.Assets == nil {
		copied.Assets = []ManagedAsset{}
	}
	if copied.ProjectInstructions == nil {
		copied.ProjectInstructions = []ProjectInstructionRef{}
	}
	if copied.RecoverySources == nil {
		copied.RecoverySources = []RecoverySource{}
	}
	if copied.PriorAssets == nil {
		copied.PriorAssets = []PriorAsset{}
	}
	probes := make([]BehavioralProbe, len(copied.Probes))
	for index, probe := range copied.Probes {
		probe.Argv = nonNilStrings(probe.Argv)
		probe.SafetyClaims = nonNilStrings(probe.SafetyClaims)
		probes[index] = probe
	}
	copied.Probes = probes
	return copied
}

// CanonicalBytes renders the spec the way the revision is computed from: the
// compact, key-sorted JSON encoding followed by one newline.
//
// This is not the encoding the release manifest uses. The difference is
// deliberate and load-bearing: every environment revision ever published was
// computed from these exact bytes.
func (s *Spec) CanonicalBytes() ([]byte, error) {
	normalized := s.normalized()
	encoded, err := canonjson.Compact(normalized)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// Revision is the durable identity of the environment: the SHA-256 of its
// canonical bytes.
func (s *Spec) Revision() (string, error) {
	encoded, err := s.CanonicalBytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ToMap renders the spec as the generic document a release manifest carries.
func (s *Spec) ToMap() (map[string]any, error) {
	normalized := s.normalized()
	generic, err := canonjson.Normalize(normalized)
	if err != nil {
		return nil, err
	}
	object, ok := generic.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the environment spec did not encode as an object")
	}
	return object, nil
}

// SpecFromMap decodes and validates a serialized spec. An unknown or a missing
// field is refused rather than ignored.
func SpecFromMap(raw map[string]any) (*Spec, error) {
	if err := strictFields(raw, specFields, "AgentEnvironmentSpec"); err != nil {
		return nil, err
	}
	spec := &Spec{}
	var err error
	if spec.SchemaVersion, err = fieldInt(raw, "schema_version"); err != nil {
		return nil, err
	}
	if spec.Adapter, err = fieldString(raw, "adapter"); err != nil {
		return nil, err
	}
	if spec.HostProfile, err = fieldString(raw, "host_profile"); err != nil {
		return nil, err
	}
	if spec.WorkerIDs, err = fieldStrings(raw, "worker_ids"); err != nil {
		return nil, err
	}
	if spec.Phases, err = fieldStrings(raw, "phases"); err != nil {
		return nil, err
	}
	if spec.SecretRefs, err = fieldStrings(raw, "secret_refs"); err != nil {
		return nil, err
	}
	if spec.PreviousEnvironmentRevision, err = fieldString(
		raw, "previous_environment_revision"); err != nil {
		return nil, err
	}
	if spec.Assets, err = decodeList(raw, "assets", managedAssetFromMap); err != nil {
		return nil, err
	}
	if spec.ProjectInstructions, err = decodeList(
		raw, "project_instructions", instructionFromMap); err != nil {
		return nil, err
	}
	if spec.Probes, err = decodeList(raw, "probes", probeFromMap); err != nil {
		return nil, err
	}
	if spec.RecoverySources, err = decodeList(
		raw, "recovery_sources", recoverySourceFromMap); err != nil {
		return nil, err
	}
	if spec.PriorAssets, err = decodeList(raw, "prior_assets", priorAssetFromMap); err != nil {
		return nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

var managedAssetFields = []string{
	"asset_id", "kind", "provenance", "source_revision", "sha256", "size_bytes",
	"staged_path", "active_path", "activation_order",
}

func managedAssetFromMap(raw map[string]any) (ManagedAsset, error) {
	var asset ManagedAsset
	if err := strictFields(raw, managedAssetFields, "ManagedAsset"); err != nil {
		return asset, err
	}
	var err error
	if asset.AssetID, err = fieldString(raw, "asset_id"); err != nil {
		return asset, err
	}
	if asset.Kind, err = fieldString(raw, "kind"); err != nil {
		return asset, err
	}
	if asset.Provenance, err = fieldString(raw, "provenance"); err != nil {
		return asset, err
	}
	if asset.SourceRevision, err = fieldString(raw, "source_revision"); err != nil {
		return asset, err
	}
	if asset.SHA256, err = fieldString(raw, "sha256"); err != nil {
		return asset, err
	}
	if asset.SizeBytes, err = fieldInt64(raw, "size_bytes"); err != nil {
		return asset, err
	}
	if asset.StagedPath, err = fieldString(raw, "staged_path"); err != nil {
		return asset, err
	}
	if asset.ActivePath, err = fieldString(raw, "active_path"); err != nil {
		return asset, err
	}
	if asset.ActivationOrder, err = fieldInt(raw, "activation_order"); err != nil {
		return asset, err
	}
	return asset, asset.Validate()
}

func priorAssetFromMap(raw map[string]any) (PriorAsset, error) {
	var asset PriorAsset
	fields := []string{"asset_id", "sha256", "size_bytes", "active_path"}
	if err := strictFields(raw, fields, "PriorAsset"); err != nil {
		return asset, err
	}
	var err error
	if asset.AssetID, err = fieldString(raw, "asset_id"); err != nil {
		return asset, err
	}
	if asset.SHA256, err = fieldString(raw, "sha256"); err != nil {
		return asset, err
	}
	if asset.SizeBytes, err = fieldInt64(raw, "size_bytes"); err != nil {
		return asset, err
	}
	if asset.ActivePath, err = fieldString(raw, "active_path"); err != nil {
		return asset, err
	}
	return asset, asset.Validate()
}

func instructionFromMap(raw map[string]any) (ProjectInstructionRef, error) {
	var reference ProjectInstructionRef
	fields := []string{"repository", "repository_revision", "path", "sha256"}
	if err := strictFields(raw, fields, "ProjectInstructionRef"); err != nil {
		return reference, err
	}
	var err error
	if reference.Repository, err = fieldString(raw, "repository"); err != nil {
		return reference, err
	}
	if reference.RepositoryRevision, err = fieldString(raw, "repository_revision"); err != nil {
		return reference, err
	}
	if reference.Path, err = fieldString(raw, "path"); err != nil {
		return reference, err
	}
	if reference.SHA256, err = fieldString(raw, "sha256"); err != nil {
		return reference, err
	}
	return reference, reference.Validate()
}

func probeFromMap(raw map[string]any) (BehavioralProbe, error) {
	var probe BehavioralProbe
	fields := []string{"probe_id", "argv", "timeout_seconds", "safety_claims"}
	if err := strictFields(raw, fields, "BehavioralProbe"); err != nil {
		return probe, err
	}
	var err error
	if probe.ProbeID, err = fieldString(raw, "probe_id"); err != nil {
		return probe, err
	}
	if probe.Argv, err = fieldStrings(raw, "argv"); err != nil {
		return probe, err
	}
	if probe.TimeoutSeconds, err = fieldInt(raw, "timeout_seconds"); err != nil {
		return probe, err
	}
	if probe.SafetyClaims, err = fieldStrings(raw, "safety_claims"); err != nil {
		return probe, err
	}
	return probe, probe.Validate()
}

func recoverySourceFromMap(raw map[string]any) (RecoverySource, error) {
	var source RecoverySource
	if err := strictFields(raw, []string{"kind", "owner", "root"}, "RecoverySource"); err != nil {
		return source, err
	}
	var err error
	if source.Kind, err = fieldString(raw, "kind"); err != nil {
		return source, err
	}
	if source.Owner, err = fieldString(raw, "owner"); err != nil {
		return source, err
	}
	if source.Root, err = fieldString(raw, "root"); err != nil {
		return source, err
	}
	return source, source.Validate()
}

// DriftObservation is one projected statement about a subject. It is a
// projection only: nothing in this file reads or changes a managed target.
type DriftObservation struct {
	Subject        string  `json:"subject"`
	State          string  `json:"state"`
	ExpectedSHA256 *string `json:"expected_sha256"`
	ObservedSHA256 *string `json:"observed_sha256"`
	Evidence       string  `json:"evidence"`
}

// Observed is what an authoritative observer reported about one asset. A nil
// SHA256 means the observer reports the target missing, which is a different
// statement from having no observation at all.
type Observed struct {
	SHA256   *string
	Evidence string
}

// ClassifyAssetDrift projects observations onto the spec without reading or
// changing any target.
//
// An asset with no observation is unavailable rather than missing, and every
// unrelated path stays in the projection as unexplained so that a later phase
// preserves it instead of reclaiming it.
func ClassifyAssetDrift(
	spec *Spec, observed map[string]Observed, unexplainedPaths []string,
) []DriftObservation {
	results := make([]DriftObservation, 0, len(spec.Assets)+len(unexplainedPaths))
	for _, asset := range spec.Assets {
		expected := asset.SHA256
		item, present := observed[asset.AssetID]
		switch {
		case !present:
			results = append(results, DriftObservation{
				Subject: asset.AssetID, State: DriftUnavailable, ExpectedSHA256: &expected,
			})
		case item.SHA256 == nil:
			results = append(results, DriftObservation{
				Subject: asset.AssetID, State: DriftMissing, ExpectedSHA256: &expected,
				Evidence: item.Evidence,
			})
		default:
			state := DriftMismatch
			if *item.SHA256 == asset.SHA256 {
				state = DriftMatch
			}
			observedDigest := *item.SHA256
			results = append(results, DriftObservation{
				Subject: asset.AssetID, State: state, ExpectedSHA256: &expected,
				ObservedSHA256: &observedDigest, Evidence: item.Evidence,
			})
		}
	}
	for _, path := range sortedUnique(unexplainedPaths) {
		results = append(results, DriftObservation{
			Subject: path, State: DriftUnexplained, Evidence: "preserved",
		})
	}
	return results
}

// DriftToMaps renders drift observations as the generic documents the agent
// reports back to the controller.
func DriftToMaps(observations []DriftObservation) []map[string]any {
	rendered := make([]map[string]any, 0, len(observations))
	for _, observation := range observations {
		rendered = append(rendered, map[string]any{
			"subject":         observation.Subject,
			"state":           observation.State,
			"expected_sha256": optional(observation.ExpectedSHA256),
			"observed_sha256": optional(observation.ObservedSHA256),
			"evidence":        observation.Evidence,
		})
	}
	return rendered
}

// ProbeResult is the evidence one probe produced on one machine.
type ProbeResult struct {
	ProbeID        string   `json:"probe_id"`
	Passed         bool     `json:"passed"`
	SafetyClaims   []string `json:"safety_claims"`
	EvidenceSHA256 string   `json:"evidence_sha256"`
}

// Validate reports whether the probe result is attributable evidence.
func (r ProbeResult) Validate() error {
	if err := validateSafeText(r.ProbeID, "probe result id"); err != nil {
		return err
	}
	if !isSortedUnique(r.SafetyClaims) {
		return fmt.Errorf("probe result safety_claims must be unique and sorted")
	}
	for _, claim := range r.SafetyClaims {
		if err := validateSafeText(claim, "probe result safety claim"); err != nil {
			return err
		}
	}
	return validateDigest(r.EvidenceSHA256, "probe evidence sha256")
}

// RecoveryObservation is owner-attributed startup evidence for one recovery
// source.
type RecoveryObservation struct {
	Kind           string   `json:"kind"`
	Owner          string   `json:"owner"`
	Status         string   `json:"status"`
	Identities     []string `json:"identities"`
	EvidenceSHA256 string   `json:"evidence_sha256"`
}

// Validate reports whether the recovery observation is attributable.
func (o RecoveryObservation) Validate() error {
	if !recoveryKinds[o.Kind] {
		return fmt.Errorf("unsupported recovery observation kind")
	}
	return validateDigest(o.EvidenceSHA256, "recovery evidence sha256")
}

// BehaviorIsEquivalentOrStricter reports whether the candidate environment
// proves at least everything the baseline proved.
//
// Incomplete evidence is never equivalence: a probe that is missing from either
// side makes the whole comparison false.
func BehaviorIsEquivalentOrStricter(
	probes []BehavioralProbe, baseline, candidate []ProbeResult,
) bool {
	required := map[string][]string{}
	for _, probe := range probes {
		required[probe.ProbeID] = probe.SafetyClaims
	}
	old := indexResults(baseline)
	newer := indexResults(candidate)
	if len(old) != len(required) || len(newer) != len(required) {
		return false
	}
	for name, claims := range required {
		oldResult, hasOld := old[name]
		newResult, hasNew := newer[name]
		if !hasOld || !hasNew || !oldResult.Passed || !newResult.Passed {
			return false
		}
		if !isSubset(claims, oldResult.SafetyClaims) ||
			!isSubset(oldResult.SafetyClaims, newResult.SafetyClaims) {
			return false
		}
	}
	return true
}

func indexResults(results []ProbeResult) map[string]ProbeResult {
	indexed := make(map[string]ProbeResult, len(results))
	for _, result := range results {
		indexed[result.ProbeID] = result
	}
	return indexed
}

// ProjectRecoveryInventory returns owner-attributed startup evidence. A source
// with no observation is reported as explicitly unavailable rather than
// silently dropped, and an observation attributed to the wrong owner is an
// error.
func ProjectRecoveryInventory(
	spec *Spec, observations map[string]RecoveryObservation,
) ([]RecoveryObservation, error) {
	projected := make([]RecoveryObservation, 0, len(spec.RecoverySources))
	for _, source := range spec.RecoverySources {
		observation, present := observations[source.Kind]
		switch {
		case !present:
			projected = append(projected, RecoveryObservation{
				Kind: source.Kind, Owner: source.Owner, Status: "unavailable",
				Identities: []string{}, EvidenceSHA256: strings.Repeat("0", 64),
			})
		case observation.Owner != source.Owner:
			return nil, fmt.Errorf(
				"recovery observation owner does not match the frozen source")
		default:
			projected = append(projected, observation)
		}
	}
	return projected, nil
}

var verdictPhases = map[string]bool{
	"install": true, "activate": true, "run": true, "observe": true, "rollback": true,
}

var evidenceFields = []string{
	"schema_version", "host_profile", "worker_id", "installed_environment_revision",
	"active_environment_revision", "worker_ready", "installed_assets", "active_assets",
	"project_instructions", "baseline_probes", "candidate_probes", "unexplained_paths",
	"preserved_unexplained_paths",
}

// PhaseVerdict validates exact, phase-specific environment evidence.
//
// A nil verdict is ambiguity: missing or malformed evidence, or an unexplained
// path, must retain the target lock for observation rather than allow a
// transition. A false verdict is an explained mismatch, which may use the
// separately journaled rollback transition.
func PhaseVerdict(spec *Spec, phase string, raw any) (*bool, error) {
	if !verdictPhases[phase] {
		return nil, fmt.Errorf("unsupported environment phase: %s", phase)
	}
	evidence, ok := raw.(map[string]any)
	if !ok {
		return nil, nil
	}
	if err := strictFields(evidence, evidenceFields, "environment evidence"); err != nil {
		return nil, nil
	}
	if version, err := fieldInt(evidence, "schema_version"); err != nil || version != 1 {
		return nil, nil
	}
	hostProfile, _ := evidence["host_profile"].(string)
	workerID, _ := evidence["worker_id"].(string)
	if hostProfile != spec.HostProfile || workerID != spec.WorkerIDs[0] {
		return boolPointer(false), nil
	}
	unexplained, unexplainedOK := pathList(evidence["unexplained_paths"])
	preserved, preservedOK := pathList(evidence["preserved_unexplained_paths"])
	if !unexplainedOK || !preservedOK {
		return nil, nil
	}
	if len(unexplained) > 0 && (phase != "rollback" || !equalStrings(preserved, unexplained)) {
		return nil, nil
	}
	if phase != "rollback" && len(preserved) > 0 {
		return nil, nil
	}
	baseline, baselineOK := probeResults(evidence["baseline_probes"])
	candidate, candidateOK := probeResults(evidence["candidate_probes"])
	if !baselineOK || !candidateOK {
		return nil, nil
	}
	if !instructionsMatch(evidence["project_instructions"], spec) {
		return boolPointer(false), nil
	}
	return phaseTransitionVerdict(spec, phase, evidence, baseline, candidate)
}

func phaseTransitionVerdict(
	spec *Spec, phase string, evidence map[string]any, baseline, candidate []ProbeResult,
) (*bool, error) {
	forward := phase != "rollback"
	revision, err := spec.Revision()
	if err != nil {
		return nil, err
	}
	installedRevision := spec.PreviousEnvironmentRevision
	if forward {
		installedRevision = revision
	}
	activeRevision := spec.PreviousEnvironmentRevision
	if forward && phase != "install" {
		activeRevision = revision
	}
	installedAssets := priorDigests(spec.PriorAssets)
	if forward {
		installedAssets = managedDigests(spec.Assets)
	}
	activeAssets := installedAssets
	if phase == "install" {
		activeAssets = priorDigests(spec.PriorAssets)
	}
	installedMatch, installedKnown := assetMapMatches(evidence["installed_assets"], installedAssets)
	activeMatch, activeKnown := assetMapMatches(evidence["active_assets"], activeAssets)
	if !installedKnown || !activeKnown {
		return nil, nil
	}
	revisionsMatch := evidence["installed_environment_revision"] == installedRevision &&
		evidence["active_environment_revision"] == activeRevision
	if !revisionsMatch || !installedMatch || !activeMatch {
		return boolPointer(false), nil
	}
	ready, isBool := evidence["worker_ready"].(bool)
	// The worker is deliberately down while the activation swaps its files, and
	// deliberately up in every other phase.
	if !isBool || ready == (phase == "activate") {
		return boolPointer(false), nil
	}
	if phase == "rollback" {
		return boolPointer(baselinePasses(spec.Probes, baseline)), nil
	}
	return boolPointer(BehaviorIsEquivalentOrStricter(spec.Probes, baseline, candidate)), nil
}

func baselinePasses(probes []BehavioralProbe, baseline []ProbeResult) bool {
	required := map[string][]string{}
	for _, probe := range probes {
		required[probe.ProbeID] = probe.SafetyClaims
	}
	observed := indexResults(baseline)
	if len(observed) != len(required) {
		return false
	}
	for name, claims := range required {
		result, present := observed[name]
		if !present || !result.Passed || !isSubset(claims, result.SafetyClaims) {
			return false
		}
	}
	return true
}

func managedDigests(assets []ManagedAsset) map[string]string {
	digests := make(map[string]string, len(assets))
	for _, asset := range assets {
		digests[asset.AssetID] = asset.SHA256
	}
	return digests
}

func priorDigests(assets []PriorAsset) map[string]string {
	digests := make(map[string]string, len(assets))
	for _, asset := range assets {
		digests[asset.AssetID] = asset.SHA256
	}
	return digests
}

// assetMapMatches reports whether the evidence agrees with the expected
// digests. The second result is false when the evidence is malformed, which is
// ambiguity rather than disagreement.
func assetMapMatches(raw any, expected map[string]string) (matches bool, known bool) {
	object, ok := raw.(map[string]any)
	if !ok || len(object) != len(expected) {
		return false, false
	}
	for name := range object {
		if _, present := expected[name]; !present {
			return false, false
		}
	}
	for name, value := range object {
		if value == nil {
			return false, true
		}
		text, isText := value.(string)
		if !isText {
			return false, false
		}
		if text != expected[name] {
			return false, true
		}
	}
	return true, true
}

func instructionsMatch(raw any, spec *Spec) bool {
	object, ok := raw.(map[string]any)
	if !ok || len(object) != len(spec.ProjectInstructions) {
		return false
	}
	for _, reference := range spec.ProjectInstructions {
		value, present := object[reference.ID()]
		if !present || value != any(reference.SHA256) {
			return false
		}
	}
	return true
}

func pathList(raw any) ([]string, bool) {
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	paths := make([]string, 0, len(items))
	for _, item := range items {
		text, isText := item.(string)
		if !isText || text == "" {
			return nil, false
		}
		paths = append(paths, text)
	}
	if !equalStrings(paths, sortedUnique(paths)) {
		return nil, false
	}
	for _, path := range paths {
		if err := validateAbsolute(path, "unexplained path"); err != nil {
			return nil, false
		}
	}
	return paths, true
}

func probeResults(raw any) ([]ProbeResult, bool) {
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	results := make([]ProbeResult, 0, len(items))
	for _, item := range items {
		object, isObject := item.(map[string]any)
		if !isObject {
			return nil, false
		}
		fields := []string{"probe_id", "passed", "safety_claims", "evidence_sha256"}
		if err := strictFields(object, fields, "probe evidence"); err != nil {
			return nil, false
		}
		var result ProbeResult
		var err error
		if result.ProbeID, err = fieldString(object, "probe_id"); err != nil {
			return nil, false
		}
		passed, isBool := object["passed"].(bool)
		if !isBool {
			return nil, false
		}
		result.Passed = passed
		if result.SafetyClaims, err = fieldStrings(object, "safety_claims"); err != nil {
			return nil, false
		}
		if result.EvidenceSHA256, err = fieldString(object, "evidence_sha256"); err != nil {
			return nil, false
		}
		if err := result.Validate(); err != nil {
			return nil, false
		}
		results = append(results, result)
	}
	identities := mapSlice(results, func(r ProbeResult) string { return r.ProbeID })
	if !equalStrings(identities, sortedUnique(identities)) {
		return nil, false
	}
	return results, true
}

// validateDigest accepts only a lowercase SHA-256 digest, so that a digest can
// never be compared case-insensitively by accident.
func validateDigest(value, field string) error {
	if len(value) != 64 || !isLowerHex(value) {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", field)
	}
	return nil
}

func validateSafeText(value, field string) error {
	if value == "" || strings.ContainsRune(value, 0) || utf8.RuneCountInString(value) > 1024 {
		return fmt.Errorf("%s must be nonempty bounded text", field)
	}
	return nil
}

func validateAbsolute(value, field string) error {
	if !filepath.IsAbs(value) || hasComponent(value, "..") || filepath.Clean(value) != value {
		return fmt.Errorf("%s must be an absolute normalized path", field)
	}
	return nil
}

func validateRelative(value, field string) error {
	if value == "" || filepath.IsAbs(value) || hasComponent(value, "..") ||
		hasComponent(value, ".") || filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a normalized relative path", field)
	}
	return nil
}

// hasComponent reports whether a path contains an exact component, which is a
// different question from the path containing the text.
func hasComponent(value, component string) bool {
	for _, part := range strings.Split(value, string(filepath.Separator)) {
		if part == component {
			return true
		}
	}
	return false
}

// pathsOverlap reports whether two paths are the same file or whether one
// contains the other.
func pathsOverlap(left, right string) bool {
	cleanLeft := filepath.Clean(left)
	cleanRight := filepath.Clean(right)
	if cleanLeft == cleanRight {
		return true
	}
	return isAncestor(cleanLeft, cleanRight) || isAncestor(cleanRight, cleanLeft)
}

func isAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return false
	}
	prefix := ancestor
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(descendant, prefix)
}

func isLowerHex(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		digit := character >= '0' && character <= '9'
		letter := character >= 'a' && character <= 'f'
		if !digit && !letter {
			return false
		}
	}
	return true
}

// isOpaqueReference reports whether a secret reference suffix names a secret
// without carrying one: alphanumerics and a few separators only.
func isOpaqueReference(value string) bool {
	for _, character := range value {
		digit := character >= '0' && character <= '9'
		lower := character >= 'a' && character <= 'z'
		upper := character >= 'A' && character <= 'Z'
		separator := strings.ContainsRune("-._/", character)
		if !digit && !lower && !upper && !separator {
			return false
		}
	}
	return true
}

func isSortedUnique(values []string) bool {
	return equalStrings(values, sortedUnique(values))
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
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

func isSubset(subset, superset []string) bool {
	members := make(map[string]bool, len(superset))
	for _, value := range superset {
		members[value] = true
	}
	for _, value := range subset {
		if !members[value] {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func mapSlice[T any](values []T, project func(T) string) []string {
	projected := make([]string, len(values))
	for index, value := range values {
		projected[index] = project(value)
	}
	return projected
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func boolPointer(value bool) *bool { return &value }

func optional(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// strictFields refuses a document with any unknown or missing field, reporting
// both sets the way the Python implementation did.
func strictFields(raw map[string]any, fields []string, context string) error {
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	var unknown, missing []string
	for key := range raw {
		if !allowed[key] {
			unknown = append(unknown, key)
		}
	}
	for _, field := range fields {
		if _, present := raw[field]; !present {
			missing = append(missing, field)
		}
	}
	if len(unknown) == 0 && len(missing) == 0 {
		return nil
	}
	sort.Strings(unknown)
	sort.Strings(missing)
	return fmt.Errorf("%s fields mismatch: missing=%s unknown=%s",
		context, pythonList(missing), pythonList(unknown))
}

func pythonList(values []string) string {
	rendered := make([]string, len(values))
	for index, value := range values {
		rendered[index] = pyfmt.Repr(value)
	}
	return "[" + strings.Join(rendered, ", ") + "]"
}

func fieldString(raw map[string]any, key string) (string, error) {
	value, present := raw[key]
	if !present {
		return "", fmt.Errorf("%s is missing", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return text, nil
}

func fieldStrings(raw map[string]any, key string) ([]string, error) {
	value, present := raw[key]
	if !present {
		return nil, fmt.Errorf("%s is missing", key)
	}
	switch typed := value.(type) {
	case []string:
		return typed, nil
	case []any:
		texts := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a list of strings", key)
			}
			texts = append(texts, text)
		}
		return texts, nil
	default:
		return nil, fmt.Errorf("%s must be a list of strings", key)
	}
}

func fieldInt(raw map[string]any, key string) (int, error) {
	value, err := fieldInt64(raw, key)
	return int(value), err
}

// fieldInt64 accepts every shape a whole number arrives in: a native Go int, a
// float64 from the standard decoder, or a json.Number when the document was
// decoded with its literals preserved.
func fieldInt64(raw map[string]any, key string) (int64, error) {
	value, present := raw[key]
	if !present {
		return 0, fmt.Errorf("%s is missing", key)
	}
	switch typed := value.(type) {
	case int:
		return int64(typed), nil
	case int64:
		return typed, nil
	case float64:
		if typed != float64(int64(typed)) {
			return 0, fmt.Errorf("%s must be a whole number", key)
		}
		return int64(typed), nil
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be a whole number", key)
		}
		return number, nil
	default:
		return 0, fmt.Errorf("%s must be a whole number", key)
	}
}

func decodeList[T any](
	raw map[string]any, key string, decode func(map[string]any) (T, error),
) ([]T, error) {
	value, present := raw[key]
	if !present {
		return nil, fmt.Errorf("%s is missing", key)
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", key)
	}
	decoded := make([]T, 0, len(items))
	for _, item := range items {
		object, isObject := item.(map[string]any)
		if !isObject {
			return nil, fmt.Errorf("every %s entry must be an object", key)
		}
		value, err := decode(object)
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, value)
	}
	return decoded, nil
}
