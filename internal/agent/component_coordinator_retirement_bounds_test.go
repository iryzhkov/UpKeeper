package agent

import (
	"bytes"
	"os"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

func TestCoordinatorRetirementRefusesOversizeAndUnknownNestedFields(t *testing.T) {
	for _, variant := range []string{"oversize", "worker-field", "project-field", "model-object"} {
		t.Run(variant, func(t *testing.T) {
			c := fleetTestPrepare(t)
			path := c.Path(fleet.CoordinatorDestination)
			prior := fleetTestFixture(t, "projection-coordinator.json")
			if variant == "oversize" {
				// Valid JSON with whitespace padding: an unbounded parser would
				// accept and remove it, so this is a genuine size negative control.
				prior = append(prior, bytes.Repeat([]byte(" "), (4<<20)+1-len(prior))...)
			} else {
				value, err := canonjson.DecodeObject(prior)
				if err != nil {
					t.Fatal(err)
				}
				workers := value["workers"].(map[string]any)
				worker := workers["homelab"].(map[string]any)
				switch variant {
				case "worker-field":
					worker["future_authority"] = true
				case "project-field":
					value["projects"].(map[string]any)["upkeeper"].(map[string]any)["future_authority"] = true
				case "model-object":
					worker["desired_models"].(map[string]any)["claudeAgent"] = map[string]any{"future_authority": true}
				}
				prior, err = canonjson.MarshalFile(value)
				if err != nil {
					t.Fatal(err)
				}
			}
			adapterTestWrite(t, path, string(prior))
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := fleetRetireCoordinator(c, "bounded-refusal", false); err == nil {
				t.Fatal("unsupported document retirement accepted")
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, prior) {
				t.Fatalf("refusal changed prior bytes: %v", err)
			}
			if _, err := os.Stat(c.Path(fleet.StateDirectory, "bounded-refusal")); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote recovery state: %v", err)
			}
		})
	}
}
