package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The observe-only result a controller's status reads carries the host's own
// convergence evidence, so status can explain a remote host's drift without
// a second round trip.
func TestObserveReleaseCarriesTheHostsUpkeeperEvidence(t *testing.T) {
	home := t.TempDir()
	holds := filepath.Join(home, ".local", "state", "dev-fleet", "scheduled-holds")
	if err := os.MkdirAll(holds, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(holds, "laptop.hold"), []byte("run 20260917T140905Z-6b2c83ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Context{Payload: Payload{ObserveOnly: true}, Home: home, Host: HostRef{Name: "laptop"}}
	result := Execute(c)
	evidence, ok := result["upkeeper"].(map[string]any)
	if !ok {
		t.Fatalf("the observe-only result carries no upkeeper evidence: %v", result)
	}
	markers, _ := evidence["holds"].([]any)
	if len(markers) != 1 || markers[0].(map[string]any)["run_id"] != "20260917T140905Z-6b2c83ab" {
		t.Errorf("holds = %v", markers)
	}
	if _, present := evidence["timers"]; !present {
		t.Errorf("timers are absent: %v", evidence)
	}
}
