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
				providers := []any{}
				if models != nil {
					providers = append(providers, map[string]any{"instance": "codex", "models": models, "quotaPool": "exhausted"})
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
