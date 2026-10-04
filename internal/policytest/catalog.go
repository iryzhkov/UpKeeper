// Package policytest supplies a bounded, read-only configured-catalog transport
// fixture. The actual installed Steward CLI remains the parser and validator.
// Nothing in this package is used by production.
package policytest

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

const Policy = `{"schema":"route-policy/v1","roles":[{"name":"execute","candidates":[{"route":"codex/gpt-6.1-sol","effort":"medium","tier":"standard"}]}]}`

func RequireCLI(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("t3-steward"); err != nil {
		t.Fatal("semantic consumer fixture requires t3-steward on PATH; configured wildcard catalogs require ced8e299561920ad1b46059f354316467f10f655 or a release containing that fix")
	}
}

// Catalog presents configured authorization even though the sole worker is
// offline, stale, and has no advertised models. Empty providers means an older
// coordinator with no authoritative catalog. Only workers queries are accepted.
func Catalog(t *testing.T, models []string) {
	t.Helper()
	providers := []any{}
	if models != nil {
		providers = append(providers, map[string]any{"instance": "codex", "models": models, "quotaPool": "exhausted"})
	}
	catalog(t, providers)
}

// ManifestCatalog isolates tests of the published manifest from operator state.
// Authorization comes from configured profiles and host exceptions, never from
// the policy being validated. The real CLI still parses and authorizes it.
func ManifestCatalog(t *testing.T, document map[string]any) {
	t.Helper()
	components, _ := document["components"].(map[string]any)
	fleet, _ := components["steward-fleet-configuration"].(map[string]any)
	if _, present := fleet["route_policy"]; !present {
		return
	}
	RequireCLI(t)
	models := map[string]map[string]bool{}
	collect := func(value any) {
		configured, _ := value.(map[string]any)
		for instance, raw := range configured {
			entries, ok := raw.([]any)
			if !ok {
				t.Fatalf("configured models for %s are not an array", instance)
			}
			if models[instance] == nil {
				models[instance] = map[string]bool{}
			}
			for _, entry := range entries {
				model, ok := entry.(string)
				if !ok {
					t.Fatalf("configured model for %s is not a string", instance)
				}
				models[instance][model] = true
			}
		}
	}
	profiles, _ := fleet["profiles"].(map[string]any)
	for _, raw := range profiles {
		profile, _ := raw.(map[string]any)
		collect(profile["models"])
	}
	hosts, _ := fleet["hosts"].(map[string]any)
	for _, raw := range hosts {
		host, _ := raw.(map[string]any)
		exceptions, _ := host["exceptions"].(map[string]any)
		collect(exceptions["models"])
	}
	instances := []string{}
	for instance := range models {
		instances = append(instances, instance)
	}
	sort.Strings(instances)
	providers := []any{}
	for _, instance := range instances {
		allowed := []string{}
		if models[instance]["*"] {
			allowed = append(allowed, "*")
		} else {
			for model := range models[instance] {
				allowed = append(allowed, model)
			}
			sort.Strings(allowed)
		}
		providers = append(providers, map[string]any{"instance": instance, "models": allowed, "quotaPool": "exhausted"})
	}
	catalog(t, providers)
}

func catalog(t *testing.T, providers []any) {
	t.Helper()
	// Unix socket paths are limited to 108 bytes; Go's per-test temp path can exceed it.
	dir, err := os.MkdirTemp("", "upkeeper-policy-catalog-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	state := filepath.Join(dir, "state.db")
	listener, err := net.Listen("unix", state+".admin.sock")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				var size [4]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					t.Error(err)
					return
				}
				n := binary.BigEndian.Uint32(size[:])
				if n > 1<<20 {
					t.Error("oversize fixture query")
					return
				}
				body := make([]byte, n)
				if _, err := io.ReadFull(conn, body); err != nil {
					t.Error(err)
					return
				}
				var query struct {
					Operation string
					Query     struct{ Kind string }
				}
				if err := json.Unmarshal(body, &query); err != nil || query.Operation != "query" || query.Query.Kind != "workers" {
					t.Errorf("unexpected fixture operation: %s (%v)", body, err)
					return
				}
				response := map[string]any{"version": "backlog.admin.local/v1", "response": map[string]any{
					"version": "backlog.admin/v1", "kind": "workers", "workers": []any{map[string]any{
						"providers": providers, "health": "offline", "state": "offline", "stale": true, "enrolled": true,
					}},
				}}
				raw, _ := json.Marshal(response)
				binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
				conn.Write(size[:])
				conn.Write(raw)
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); wg.Wait() })
	config := filepath.Join(dir, "t3-steward")
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "config.yaml"), []byte("state_path: "+state+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	// UpKeeper bootstrap lookup is home-relative, independently of XDG.
	t.Setenv("HOME", dir)
}
