package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func TestT3SameVersionUpstreamControlledAControlledBUpstream(t *testing.T) {
	c, spec, runner, prefix := t3ArtifactHost(t, false, false, 0, 0)
	actions := []string{}
	before, e := t3Observe(c, spec)
	if e != nil {
		t.Fatal(e)
	}
	if got, e := t3Apply(c, spec, before, &actions); e != nil || got != "updated" {
		t.Fatalf("upstream to A: %s %v", got, e)
	}
	a, e := release.ReadT3Retained(c.Home)
	if e != nil {
		t.Fatal(e)
	}
	s, archive, receipt := t3AgentFixture(t, release.T3Target(), strings.Repeat("e", 40))
	rawBody, _ := json.Marshal(s)
	var raw map[string]any
	_ = json.Unmarshal(rawBody, &raw)
	spec["artifact"] = raw
	original := runner.handle
	runner.handle = func(cmd procx.Command) procx.Result {
		if cmd.Argv[0] == "python3" {
			body := archive
			if strings.HasSuffix(cmd.Argv[7], ".json") {
				body = receipt
			}
			if e := os.WriteFile(cmd.Argv[8], body, 0600); e != nil {
				t.Fatal(e)
			}
			return procx.Result{}
		}
		if len(cmd.Argv) > 5 && cmd.Argv[0] == "npm" && cmd.Argv[1] == "install" {
			source := filepath.Join(cmd.Argv[4], "lib/node_modules/t3")
			if e := os.MkdirAll(source, 0755); e != nil {
				t.Fatal(e)
			}
			if _, e := release.VerifyT3Bundle(archive, receipt, s, release.T3Target(), "0.0.45", source); e != nil {
				t.Fatal(e)
			}
			packagePath := filepath.Join(source, "package.json")
			body, _ := os.ReadFile(packagePath)
			var pkg map[string]any
			_ = json.Unmarshal(body, &pkg)
			delete(pkg, "upkeeperControlledBuild")
			body, _ = json.Marshal(pkg)
			if e := os.WriteFile(packagePath, body, 0644); e != nil {
				t.Fatal(e)
			}
			return procx.Result{}
		}
		return original(cmd)
	}
	before, e = t3Observe(c, spec)
	if e != nil {
		t.Fatal(e)
	}
	if before["artifact_installed"] == true {
		t.Fatal("same version A satisfies B")
	}
	if got, e := t3Apply(c, spec, before, &actions); e != nil || got != "updated" {
		t.Fatalf("A to B: %s %v", got, e)
	}
	b, e := release.ReadT3Retained(c.Home)
	if e != nil {
		t.Fatal(e)
	}
	if a.Artifact.SourceCommit == b.Artifact.SourceCommit || a.Artifact.Assets[release.T3Target()].SHA256 == b.Artifact.Assets[release.T3Target()].SHA256 {
		t.Fatal("fixture B does not change controlled bytes")
	}
	delete(spec, "artifact")
	before, e = t3Observe(c, spec)
	if e != nil {
		t.Fatal(e)
	}
	if got, e := t3Apply(c, spec, before, &actions); e != nil || got != "updated" {
		t.Fatalf("B to upstream: %s %v", got, e)
	}
	if e = release.VerifyT3Upstream(c.Home, prefix, "0.0.45"); e != nil {
		t.Fatal(e)
	}
	before, e = t3Observe(c, spec)
	if e != nil {
		t.Fatal(e)
	}
	if got, e := t3Apply(c, spec, before, &actions); e != nil || got != "converged" {
		t.Fatalf("upstream convergence: %s %v", got, e)
	}
}
