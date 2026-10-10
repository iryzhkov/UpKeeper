package rollout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var reviewedComponentNames = []string{"omarchy-setup", "agent-config", "agent-tooling", "jocasta", "codex-controlled-runtime", "agent-environment", "huyang", "t3-steward", "t3", "worker-configuration", "steward-fleet-configuration"}

func seedPublication(t *testing.T, c Config, s *State) {
	t.Helper()
	s.ConfigDigest = c.Digest()
	s.ExpectHead = strings.Repeat("b", 40)
	s.ReleaseSHA256 = strings.Repeat("e", 64)
	if s.TimerPrior == nil {
		s.TimerPrior = map[string]string{}
	}
	for _, host := range c.TimerHosts {
		if s.TimerPrior[host] == "" {
			s.TimerPrior[host] = "reviewed stopped timer baseline"
		}
	}
	if s.Extra == nil {
		s.Extra = map[string]json.RawMessage{}
	}
	manifest := map[string]any{}
	for _, name := range reviewedComponentNames {
		manifest[name] = map[string]any{}
	}
	manifest["t3-steward"] = map[string]any{"version": c.Version}
	for _, key := range []string{"author-started", "release.diff", "release-before", "release-after", "fleet-plan", "preview-scoped", "preview-rollback", "rc118-findings"} {
		if s.Extra[key] == nil {
			s.Extra[key] = jsonBytes(true)
		}
	}
	s.Extra["release-after"] = jsonBytes(map[string]any{"components": manifest})
	hosts := []any{}
	for host := range c.Targets {
		components := []any{}
		for _, name := range reviewedComponentNames {
			components = append(components, map[string]any{"name": name, "state": "converged"})
		}
		hosts = append(hosts, map[string]any{"host": host, "components": components})
	}
	s.Extra["preview-full"] = jsonBytes(map[string]any{"schema_version": 1, "command": "pull", "dry_run": true, "exit_code": 0, "desired_manifest_commit": s.ExpectHead, "release_sha256": s.ReleaseSHA256, "hosts": hosts})
	if s.Extra["assets"] == nil {
		seedAssets(t, s)
	}
	raw := jsonBytes(reviewedPackage{s.ExpectHead, s.ReleaseSHA256, s.ConfigDigest, s.TimerPrior, s.Extra})
	s.PackageDigest = digest(raw)
	if err := os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.StatePath+".review-package.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.ReviewFile, []byte("VERDICT: ACCEPT\n"+s.ExpectHead+"\n"+s.ReleaseSHA256+"\nPACKAGE "+s.PackageDigest+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}
func seedStatus(f *backendFake) []byte {
	hosts := []any{}
	for host := range f.c.Targets {
		components := []any{}
		for _, name := range reviewedComponentNames {
			components = append(components, map[string]any{"name": name, "state": "converged"})
		}
		hosts = append(hosts, map[string]any{"host": host, "reachable": true, "observe_only": true, "result": "converged", "components": components})
	}
	return jsonBytes(map[string]any{"schema_version": 1, "command": "status", "desired_manifest_commit": f.head, "release_sha256": strings.Repeat("e", 64), "exit_code": 0, "result": "converged", "hosts": hosts})
}
