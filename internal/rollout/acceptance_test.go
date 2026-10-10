package rollout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedPublication(t *testing.T, c Config, s *State) {
	t.Helper()
	s.ConfigDigest = c.Digest()
	s.ExpectHead = strings.Repeat("b", 40)
	s.ReleaseSHA256 = strings.Repeat("e", 64)
	raw := jsonBytes(struct{ Head, Release, ConfigDigest string }{s.ExpectHead, s.ReleaseSHA256, c.Digest()})
	s.PackageDigest = digest(raw)
	if err := os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.StatePath+".review-package.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func seedStatus(f *backendFake) []byte {
	hosts := []any{}
	for _, h := range append(append([]string{}, append([]string{f.c.Canary}, f.c.FleetOrder...)...), f.c.LaptopHost) {
		if h == "" {
			continue
		}
		hosts = append(hosts, map[string]any{"host": h, "reachable": true, "observe_only": true, "result": "converged", "components": []any{map[string]any{"name": "t3-steward", "state": "converged"}}})
	}
	return jsonBytes(map[string]any{"schema_version": 1, "command": "status", "desired_manifest_commit": f.head, "release_sha256": strings.Repeat("e", 64), "exit_code": 0, "result": "converged", "hosts": hosts})
}
