package agentenv

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// fixtureRevision is the revision the Python implementation computes for the
// checked-in fixture. It is a durable identity, so the Go port reproducing it
// byte for byte is the whole point of the compact encoding.
const fixtureRevision = "4d7e4014f9450dde22e186635279e45e6745c8712efd1cb9abd74c48efc98a99"

// fixtureCanonicalLength is the length of json.dumps(spec, separators=(",",
// ":"), sort_keys=True) plus the trailing newline, for the same fixture.
const fixtureCanonicalLength = 3614

const fixturePath = "../../tests/fixtures/agent-environment-v1.json"

func loadFixture(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("cannot read the fixture: %v", err)
	}
	raw, err := canonjson.DecodeObject(data)
	if err != nil {
		t.Fatalf("cannot decode the fixture: %v", err)
	}
	return raw
}

func TestFixtureRoundTripsWithAStableRevision(t *testing.T) {
	spec, err := SpecFromMap(loadFixture(t))
	if err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	rendered, err := spec.ToMap()
	if err != nil {
		t.Fatalf("cannot render the spec: %v", err)
	}
	restored, err := SpecFromMap(rendered)
	if err != nil {
		t.Fatalf("the rendered spec was refused: %v", err)
	}
	revision, err := spec.Revision()
	if err != nil {
		t.Fatalf("cannot compute the revision: %v", err)
	}
	restoredRevision, err := restored.Revision()
	if err != nil {
		t.Fatalf("cannot compute the restored revision: %v", err)
	}
	if revision != restoredRevision {
		t.Errorf("the round trip changed the revision: %s != %s", revision, restoredRevision)
	}
	if revision != fixtureRevision {
		t.Errorf("revision drifted from the published identity:\n got %s\nwant %s",
			revision, fixtureRevision)
	}
	if spec.Adapter != Adapter {
		t.Errorf("adapter = %q, want %q", spec.Adapter, Adapter)
	}
}

// TestTheRevisionUsesTheCompactEncoding guards the one encoding decision that
// cannot be changed later: the revision is compact, while a stored document is
// indented.
func TestTheRevisionUsesTheCompactEncoding(t *testing.T) {
	spec, err := SpecFromMap(loadFixture(t))
	if err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	encoded, err := spec.CanonicalBytes()
	if err != nil {
		t.Fatalf("cannot render the canonical bytes: %v", err)
	}
	if len(encoded) != fixtureCanonicalLength {
		t.Errorf("canonical length = %d, want %d", len(encoded), fixtureCanonicalLength)
	}
	if !bytes.HasSuffix(encoded, []byte("}\n")) {
		t.Errorf("the canonical bytes must end with one newline, got %q", tail(encoded))
	}
	if bytes.Contains(encoded[:len(encoded)-1], []byte("\n")) {
		t.Error("the canonical bytes must not be indented")
	}
	if !bytes.HasPrefix(encoded, []byte(`{"adapter":"local-single-worker-v1","assets":[{`)) {
		t.Errorf("unexpected compact prefix: %q", encoded[:48])
	}
	indented, err := Canonical(mustMap(t, spec))
	if err != nil {
		t.Fatalf("cannot render the indented document: %v", err)
	}
	if bytes.Equal(indented, encoded) {
		t.Error("the stored document and the revision must not share an encoding")
	}
}

func mustMap(t *testing.T, spec *Spec) map[string]any {
	t.Helper()
	rendered, err := spec.ToMap()
	if err != nil {
		t.Fatalf("cannot render the spec: %v", err)
	}
	return rendered
}

func tail(data []byte) string {
	if len(data) < 8 {
		return string(data)
	}
	return string(data[len(data)-8:])
}

func TestInvalidEnvironmentFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		message string
	}{
		{
			name:    "an unknown field",
			mutate:  func(raw map[string]any) { raw["unexpected"] = true },
			message: "fields mismatch",
		},
		{
			name:    "a missing field",
			mutate:  func(raw map[string]any) { delete(raw, "secret_refs") },
			message: "fields mismatch",
		},
		{
			name:    "more than one worker",
			mutate:  func(raw map[string]any) { raw["worker_ids"] = []any{"one", "two"} },
			message: "exactly one worker",
		},
		{
			name: "reordered phases",
			mutate: func(raw map[string]any) {
				raw["phases"] = []any{"activate", "install", "probe", "rollback"}
			},
			message: "phases",
		},
		{
			name: "a plaintext secret",
			mutate: func(raw map[string]any) {
				raw["secret_refs"] = append(raw["secret_refs"].([]any), "plaintext")
			},
			message: "secretref",
		},
		{
			name: "staging that is the active path",
			mutate: func(raw map[string]any) {
				asset := raw["assets"].([]any)[0].(map[string]any)
				asset["staged_path"] = asset["active_path"]
			},
			message: "staging",
		},
		{
			name: "a relative probe executable",
			mutate: func(raw map[string]any) {
				probe := raw["probes"].([]any)[0].(map[string]any)
				probe["argv"] = []any{"sh", "-c", "true"}
			},
			message: "absolute",
		},
		{
			name: "an unsorted asset order",
			mutate: func(raw map[string]any) {
				assets := raw["assets"].([]any)
				assets[0], assets[1] = assets[1], assets[0]
			},
			message: "unique, and sorted",
		},
		{
			name: "a truncated digest",
			mutate: func(raw map[string]any) {
				raw["previous_environment_revision"] = "abc"
			},
			message: "lowercase SHA-256 digest",
		},
		{
			name:    "another adapter",
			mutate:  func(raw map[string]any) { raw["adapter"] = "remote-fleet-v2" },
			message: "local-single-worker-v1 adapter",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			raw := loadFixture(t)
			testCase.mutate(raw)
			_, err := SpecFromMap(raw)
			if err == nil {
				t.Fatal("the malformed environment was accepted")
			}
			if !strings.Contains(err.Error(), testCase.message) {
				t.Errorf("error = %q, want it to mention %q", err, testCase.message)
			}
		})
	}
}

func TestDriftPreservesDistinctFailureStates(t *testing.T) {
	spec, err := SpecFromMap(loadFixture(t))
	if err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	mismatched := strings.Repeat("f", 64)
	observed := map[string]Observed{
		spec.Assets[0].AssetID: {SHA256: &spec.Assets[0].SHA256, Evidence: "evidence:one"},
		spec.Assets[1].AssetID: {SHA256: &mismatched, Evidence: "evidence:two"},
		spec.Assets[2].AssetID: {SHA256: nil, Evidence: "evidence:three"},
	}
	unexplained := "/home/igor/.config/local-change"
	states := map[string]string{}
	for _, observation := range ClassifyAssetDrift(spec, observed, []string{unexplained}) {
		states[observation.Subject] = observation.State
	}
	wanted := map[string]string{
		spec.Assets[0].AssetID: DriftMatch,
		spec.Assets[1].AssetID: DriftMismatch,
		spec.Assets[2].AssetID: DriftMissing,
		spec.Assets[3].AssetID: DriftUnavailable,
		unexplained:            DriftUnexplained,
	}
	for subject, want := range wanted {
		if states[subject] != want {
			t.Errorf("state of %s = %q, want %q", subject, states[subject], want)
		}
	}
}

// TestDriftRendersNullForAnUnknownDigest keeps the report shape the controller
// parses: a digest that is not known is null, never an empty string.
func TestDriftRendersNullForAnUnknownDigest(t *testing.T) {
	spec, err := SpecFromMap(loadFixture(t))
	if err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	rendered := DriftToMaps(ClassifyAssetDrift(spec, map[string]Observed{}, nil))
	if len(rendered) != len(spec.Assets) {
		t.Fatalf("rendered %d observations, want %d", len(rendered), len(spec.Assets))
	}
	if rendered[0]["observed_sha256"] != nil {
		t.Errorf("observed_sha256 = %v, want null", rendered[0]["observed_sha256"])
	}
	if rendered[0]["expected_sha256"] != spec.Assets[0].SHA256 {
		t.Errorf("expected_sha256 = %v, want the frozen digest", rendered[0]["expected_sha256"])
	}
	encoded, err := canonjson.Compact(rendered[0])
	if err != nil {
		t.Fatalf("the drift report does not encode: %v", err)
	}
	if !strings.Contains(string(encoded), `"observed_sha256":null`) {
		t.Errorf("drift report = %s, want an explicit null", encoded)
	}
}

func TestBehaviorRequiresCompleteNonRegressingProbeEvidence(t *testing.T) {
	spec, err := SpecFromMap(loadFixture(t))
	if err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	evidence := strings.Repeat("a", 64)
	var baseline, candidate []ProbeResult
	for _, probe := range spec.Probes {
		baseline = append(baseline, ProbeResult{
			ProbeID: probe.ProbeID, Passed: true,
			SafetyClaims: probe.SafetyClaims, EvidenceSHA256: evidence,
		})
		candidate = append(candidate, ProbeResult{
			ProbeID: probe.ProbeID, Passed: true,
			SafetyClaims:   append(append([]string{}, probe.SafetyClaims...), "stricter"),
			EvidenceSHA256: evidence,
		})
	}
	if !BehaviorIsEquivalentOrStricter(spec.Probes, baseline, candidate) {
		t.Error("a strictly stronger candidate was rejected")
	}
	if BehaviorIsEquivalentOrStricter(spec.Probes, baseline, candidate[:len(candidate)-1]) {
		t.Error("incomplete candidate evidence was accepted")
	}
	failed := append([]ProbeResult{}, candidate...)
	failed[0].Passed = false
	if BehaviorIsEquivalentOrStricter(spec.Probes, baseline, failed) {
		t.Error("a failing probe was accepted")
	}
}

func TestRecoveryInventoryRetainsOwnerAndUnavailableSources(t *testing.T) {
	spec, err := SpecFromMap(loadFixture(t))
	if err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	evidence := strings.Repeat("a", 64)
	observation := RecoveryObservation{
		Kind: "lease", Owner: "t3-steward", Status: "ready",
		Identities: []string{"lease-1"}, EvidenceSHA256: evidence,
	}
	projected, err := ProjectRecoveryInventory(
		spec, map[string]RecoveryObservation{"lease": observation})
	if err != nil {
		t.Fatalf("the inventory was refused: %v", err)
	}
	unavailable := map[string]bool{}
	for _, item := range projected {
		if item.Kind == "lease" && item.Status != "ready" {
			t.Errorf("the observed lease was replaced by %q", item.Status)
		}
		if item.Status == "unavailable" {
			unavailable[item.Kind] = true
		}
	}
	for _, kind := range []string{"huyang-journal", "t3-thread", "worktree"} {
		if !unavailable[kind] {
			t.Errorf("%s was not reported as explicitly unavailable", kind)
		}
	}
	_, err = ProjectRecoveryInventory(spec, map[string]RecoveryObservation{
		"lease": {Kind: "lease", Owner: "UpKeeper", Status: "ready", EvidenceSHA256: evidence},
	})
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Errorf("a misattributed observation was accepted: %v", err)
	}
}

func TestPhaseVerdictDistinguishesAmbiguityFromDisagreement(t *testing.T) {
	spec, err := SpecFromMap(loadFixture(t))
	if err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	if _, err := PhaseVerdict(spec, "teleport", map[string]any{}); err == nil {
		t.Error("an unsupported phase was accepted")
	}
	verdict, err := PhaseVerdict(spec, "install", "not a document")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != nil {
		t.Errorf("malformed evidence gave a verdict of %v, want ambiguity", *verdict)
	}
	verdict, err = PhaseVerdict(spec, "install", map[string]any{"schema_version": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != nil {
		t.Errorf("incomplete evidence gave a verdict of %v, want ambiguity", *verdict)
	}
	evidence := completeEvidence(t, spec)
	evidence["host_profile"] = "another-host"
	verdict, err = PhaseVerdict(spec, "install", evidence)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict == nil || *verdict {
		t.Errorf("evidence from another host gave %v, want an explained false", verdict)
	}
}

// completeEvidence builds the evidence document a healthy install phase would
// report, so that a test can spoil exactly one field of it.
func completeEvidence(t *testing.T, spec *Spec) map[string]any {
	t.Helper()
	installed := map[string]any{}
	for _, asset := range spec.Assets {
		installed[asset.AssetID] = asset.SHA256
	}
	active := map[string]any{}
	for _, asset := range spec.PriorAssets {
		active[asset.AssetID] = asset.SHA256
	}
	revision, err := spec.Revision()
	if err != nil {
		t.Fatalf("cannot compute the revision: %v", err)
	}
	instructions := map[string]any{}
	for _, reference := range spec.ProjectInstructions {
		instructions[reference.ID()] = reference.SHA256
	}
	return map[string]any{
		"schema_version":                 1,
		"host_profile":                   spec.HostProfile,
		"worker_id":                      spec.WorkerIDs[0],
		"installed_environment_revision": revision,
		"active_environment_revision":    spec.PreviousEnvironmentRevision,
		"worker_ready":                   true,
		"installed_assets":               installed,
		"active_assets":                  active,
		"project_instructions":           instructions,
		"baseline_probes":                []any{},
		"candidate_probes":               []any{},
		"unexplained_paths":              []any{},
		"preserved_unexplained_paths":    []any{},
	}
}
