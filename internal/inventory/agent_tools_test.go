package inventory_test

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/inventory"
)

// An absent profile is every tool, which is what every host had before
// profiles existed; a declared one is kept as written.
func TestAgentToolsProfileIsOptional(t *testing.T) {
	hosts, err := loadInventoryText(t, installPackagesInventory(""))
	if err != nil || hosts["agent-a"].AgentTools != nil {
		t.Fatalf("absent profile = %#v (%v)", hosts["agent-a"].AgentTools, err)
	}
	for _, profile := range [][]string{
		{"homelab-cli", "ov-memory"},
		{"local-file-llm"},
		{"ov-memory", "homelab-cli", "local-file-llm"},
	} {
		value := `,"agent_tools":["` + strings.Join(profile, `","`) + `"]`
		hosts, err := loadInventoryText(t, installPackagesInventory(value))
		if err != nil || !reflect.DeepEqual(hosts["agent-a"].AgentTools, profile) {
			t.Fatalf("%s = %#v (%v)", value, hosts["agent-a"].AgentTools, err)
		}
	}
}

func TestAgentToolsProfileRefusesWhatIsNotAProfile(t *testing.T) {
	for value, want := range map[string]string{
		`[]`:                        "agent-a.agent_tools must name at least one tool",
		`null`:                      "agent-a.agent_tools must be a list of tool names",
		`"ov-memory"`:               "agent-a.agent_tools must be a list of tool names",
		`{"ov-memory":true}`:        "agent-a.agent_tools must be a list of tool names",
		`["ov-memory","ollama"]`:    `agent-a.agent_tools names unknown tool "ollama"; known tools: homelab-cli, ov-memory, local-file-llm`,
		`["OV-Memory"]`:             `agent-a.agent_tools names unknown tool "OV-Memory"`,
		`["ov-memory","ov-memory"]`: `agent-a.agent_tools names "ov-memory" twice`,
		`["ov-memory",1]`:           "agent-a.agent_tools entries must be strings",
		`["ov-memory",null]`:        "agent-a.agent_tools entries must be strings",
		`[["ov-memory"]]`:           "agent-a.agent_tools entries must be strings",
		`[""]`:                      `agent-a.agent_tools names unknown tool ""`,
		`[" ov-memory"]`:            `agent-a.agent_tools names unknown tool " ov-memory"`,
		`["homelab-cli","local-file-llm","ov-memory","homelab-cli"]`: `agent-a.agent_tools names "homelab-cli" twice`,
	} {
		_, err := loadInventoryText(t, installPackagesInventory(`,"agent_tools":`+value))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("agent_tools %s: error %v, want %q", value, err, want)
		}
	}
}

// The repository's own inventory gives the agent VMs on VLAN 130 the profile
// without local-file-llm, whose Ollama server they cannot reach by design,
// and leaves every other host on the default.
func TestRepositoryInventoryProfiles(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "inventory", "hosts.yml")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	hosts, err := inventory.LoadInventory(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range hosts {
		var want []string
		if name == "agent-a" || name == "agent-b" {
			want = []string{"homelab-cli", "ov-memory"}
		}
		if !reflect.DeepEqual(spec.AgentTools, want) {
			t.Errorf("%s agent_tools = %#v, want %#v", name, spec.AgentTools, want)
		}
	}
	for _, name := range []string{"agent-a", "agent-b"} {
		if _, ok := hosts[name]; !ok {
			t.Errorf("inventory has no %s", name)
		}
	}
}
