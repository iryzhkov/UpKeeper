package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func TestFleetPolicyExplicitClientConflictRefusesBeforeEffects(t *testing.T) {
	for name, config := range map[string]string{
		"endpoint":           "backlog_v2:\n  coordinator_client:\n    coordinator_id: normandy-coordinator\n    address: another-host\n    credential: secretref:f03-admin/omarchy-pc\n",
		"escaped-key":        "backlog_v2:\n  \"coordinator\\u005fclient\":\n    coordinator_id: another-coordinator\n",
		"alias":              "other: &client\n  address: another-host\nbacklog_v2:\n  coordinator_client: *client\n",
		"merge":              "other: &client\n  address: another-host\nbacklog_v2:\n  <<: {coordinator_client: *client}\n",
		"duplicate":          "backlog_v2:\n  coordinator_client: {}\n  coordinator_client: {address: hidden}\n",
		"multiple-documents": "{}\n---\nbacklog_v2: {}\n",
		"truncation":         "backlog_v2: [",
		"wrong-shape":        "backlog_v2: {coordinator_client: []}\n",
		"coordinator-mode":   "backlog_v2: {mode: coordinator}\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, component := policyContext(t)
			path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "t3-steward", "config.yaml")
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			c.Runner = func(procx.Command) procx.Result { calls++; return procx.Result{Code: 0} }
			_, err := fleetProjection(c, component, fleetTestHost, "explicit-conflict", false)
			if err == nil || calls != 0 {
				t.Fatalf("conflict reached transport/effects: %d %v", calls, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, []byte(config)) {
				t.Fatalf("operator config changed: %v", err)
			}
			for _, destination := range []string{fleet.ClientDestination, fleet.WorkerDestination, fleet.RoutePolicyDestination, fleetPolicyReceiptPath, fleet.StateDirectory + "/explicit-conflict"} {
				if _, err := os.Stat(c.Path(destination)); !os.IsNotExist(err) {
					t.Fatalf("conflict wrote %s: %v", destination, err)
				}
			}
		})
	}
}

func TestFleetPolicyExplicitClientMatchesAllSecuritySettings(t *testing.T) {
	c, component := policyContext(t)
	d, err := fleet.Parse(component)
	if err != nil {
		t.Fatal(err)
	}
	client, err := d.ClientBytes(fleetTestHost)
	if err != nil {
		t.Fatal(err)
	}
	intended, err := canonjson.DecodeObject(client)
	if err != nil {
		t.Fatal(err)
	}
	delete(intended, "schema_version")
	intended["credential"] = intended["credential_ref"]
	delete(intended, "credential_ref")
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "t3-steward", "config.yaml")
	write := func(value map[string]any) {
		t.Helper()
		body, err := canonjson.MarshalFile(map[string]any{"unrelated": map[string]any{"retained": true}, "backlog_v2": map[string]any{"coordinator_client": value}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(intended)
	before, _ := os.ReadFile(path)
	if err := fleetCheckExplicitPolicyClient(c, client); err != nil {
		t.Fatalf("matching explicit client: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("matching config was rewritten")
	}
	for _, key := range []string{"credential", "coordinator_id", "address", "connection", "remote_command", "request_timeout", "message_limits"} {
		t.Run(key, func(t *testing.T) {
			saved := intended[key]
			intended[key] = "conflicting-security-setting"
			write(intended)
			err := fleetCheckExplicitPolicyClient(c, client)
			intended[key] = saved
			if err == nil || !strings.Contains(err.Error(), "differs") {
				t.Fatalf("security mismatch accepted: %v", err)
			}
		})
	}
	if err := os.WriteFile(path, []byte("backlog_v2:\n  coordinator_client:\n    defaults:\n      model: codex/gpt-6.1-sol\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fleetCheckExplicitPolicyClient(c, client); err != nil {
		t.Fatalf("defaults-only block: %v", err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if err := fleetCheckExplicitPolicyClient(c, client); err == nil {
		t.Fatal("writable authority file accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "operator-config")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := fleetCheckExplicitPolicyClient(c, client); err == nil {
		t.Fatal("symlink authority file accepted")
	}
}
