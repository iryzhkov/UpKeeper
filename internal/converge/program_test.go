package converge_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// pinnedRelease is the published release with a program pin added.
func pinnedRelease(t *testing.T) release.Release {
	t.Helper()
	encoded, err := canonjson.MarshalFile(fleetRelease(t).Manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := canonjson.DecodeObject(encoded)
	if err != nil {
		t.Fatal(err)
	}
	size := json.Number("9437184")
	manifest["components"].(map[string]any)[release.ProgramComponent] = map[string]any{
		"kind": "upkeeper-binary", "repository": release.ProgramRepository, "tag": "v0.1.31",
		"source_commit": strings.Repeat("b", 40), "toolchain": "go1.24.13",
		"assets": map[string]any{
			"linux-amd64": map[string]any{"asset": "upkeeper-linux-amd64", "sha256": strings.Repeat("1", 64), "size": size},
			"linux-arm64": map[string]any{"asset": "upkeeper-linux-arm64", "sha256": strings.Repeat("2", 64), "size": size},
		},
	}
	value, err := release.Validate(manifest)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return value
}

func TestTheProgramFenceIsThePinnedCommitExceptForARollback(t *testing.T) {
	pinned := pinnedRelease(t)
	if got := converge.ProgramFence(pinned, "pull"); got != strings.Repeat("b", 40) {
		t.Errorf("pull fence %q", got)
	}
	if got := converge.ProgramFence(pinned, "scheduled-pull"); got != strings.Repeat("b", 40) {
		t.Errorf("scheduled-pull fence %q", got)
	}
	if got := converge.ProgramFence(pinned, "rollback"); got != "" {
		t.Errorf("a rollback requires program %q", got)
	}
	if got := converge.ProgramFence(fleetRelease(t), "pull"); got != "" {
		t.Errorf("a release without a pin requires program %q", got)
	}
}

// The pin is applied by self-pull, never dispatched as a component, and the
// fence and the self-update account travel to the agent and into the receipt.
func TestTheFenceAndTheSelfUpdateTravelAndThePinIsNotDispatched(t *testing.T) {
	value := pinnedRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"normandy"})
	choices.ExpectedProgramCommit = converge.ProgramFence(value, "pull")
	choices.SelfUpdate = map[string]any{"after_update": strings.Repeat("c", 40)}
	var seen []agent.Payload
	choices.Runner = stateRunner("converged", &seen)
	report, _, err := converge.Fleet(config, value, choices)
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(seen) != 1 || seen[0].ExpectedProgramCommit != strings.Repeat("b", 40) {
		t.Fatalf("payloads %#v", seen)
	}
	if slices.Contains(seen[0].Components, release.ProgramComponent) {
		t.Errorf("the program pin was dispatched as a component: %v", seen[0].Components)
	}
	if report["self_update"] == nil {
		t.Error("the report does not say what the self-update did")
	}
	record, err := choices.RecordStore.Read(choices.RunID)
	if err != nil {
		t.Fatal(err)
	}
	details, _ := record["details"].(map[string]any)
	if details["self_update"] == nil {
		t.Errorf("the run record does not say what the self-update did: %v", details)
	}
}

// A payload without a fence is byte for byte what an older controller sent.
func TestAnUnfencedPayloadOmitsTheField(t *testing.T) {
	encoded, err := json.Marshal(agent.Payload{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "expected_program_commit") {
		t.Errorf("an unfenced payload carries the field: %s", encoded)
	}
}
