package release_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// The environment component has its own validator, which lives in the package
// that also projects it. These tests are about the manifest around it, so they
// accept whatever the environment says and assert the rest.
func acceptAnyEnvironment(t *testing.T) {
	t.Helper()
	previous := release.EnvironmentValidator
	release.EnvironmentValidator = func(map[string]any) error { return nil }
	t.Cleanup(func() { release.EnvironmentValidator = previous })
}

func publishedManifest(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", release.File))
	if err != nil {
		t.Skipf("no published manifest in this checkout: %v", err)
	}
	document, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return document
}

func TestThePublishedManifestValidatesAndKeepsItsIdentity(t *testing.T) {
	acceptAnyEnvironment(t)
	value, err := release.Validate(publishedManifest(t))
	if err != nil {
		t.Fatalf("the repository's own release did not validate: %v", err)
	}
	digest, err := value.SHA256()
	if err != nil {
		t.Fatalf("SHA256: %v", err)
	}
	// This digest was produced by the Python implementation from this file.
	const expected = "4af4e0281126f3523f256106ce85d5adf6d2e29ef76ae7141d584780fc81fc5a"
	if digest != expected {
		t.Errorf("release identity is %s, expected %s", digest, expected)
	}
	summary := release.Describe(value)
	if len(summary) != 7 {
		t.Errorf("a paired release describes seven lines, got %d: %v", len(summary), summary)
	}
	if !strings.HasPrefix(summary[0], "omarchy-setup ") {
		t.Errorf("summary does not start with the setup checkout: %q", summary[0])
	}
}

func TestValidateRefusals(t *testing.T) {
	acceptAnyEnvironment(t)
	cases := []struct {
		name     string
		mutate   func(map[string]any)
		expected string
	}{
		{
			"a future schema version",
			func(m map[string]any) { m["schema_version"] = number("2") },
			"unsupported release schema_version",
		},
		{
			"a controller that is not a host name",
			func(m map[string]any) { m["captured_from"] = "Some Machine" },
			"invalid host selector: 'Some Machine'",
		},
		{
			"a component set that is neither the legacy nor the current one",
			func(m map[string]any) { delete(components(m), "huyang") },
			"release components must be exactly " + strings.Join(release.ComponentOrder, ", "),
		},
		{
			"a checkout pinned to a branch instead of a commit",
			func(m map[string]any) { component(m, "huyang")["commit"] = "main" },
			"git refs must be full lowercase 40-character commits",
		},
		{
			"a build word that could become a shell fragment",
			func(m map[string]any) {
				component(m, "huyang")["build"] = []any{"make", "build; rm -rf /"}
			},
			`huyang.build contains an unsafe word: 'build; rm -rf /'`,
		},
		{
			"a steward build that was never tagged",
			func(m map[string]any) {
				component(m, "t3-steward")["version"] = "0.10.1-3-gabcdef1"
			},
			"t3-steward.version is not a release version: '0.10.1-3-gabcdef1'",
		},
		{
			"a steward build from a dirty tree",
			func(m map[string]any) {
				component(m, "t3-steward")["version"] = "0.10.1-dirty"
			},
			"t3-steward.version is a local build, not a release: '0.10.1-dirty'",
		},
		{
			"a T3 pin outside the range its steward was tested with",
			func(m map[string]any) {
				component(m, "t3")["compatibility"].(map[string]any)["max"] = "0.0.1"
			},
			"T3 is outside the captured steward compatibility range",
		},
		{
			"a T3 component carrying an extra field",
			func(m map[string]any) { component(m, "t3")["extra"] = "x" },
			"invalid T3 component fields",
		},
		{
			"a steward installed by the retired installer",
			func(m map[string]any) { component(m, "t3-steward")["installer"] = "t3-update" },
			"t3-steward.installer must be upkeeper",
		},
		{
			"the retired agents module coming back with the environment",
			func(m map[string]any) {
				component(m, "omarchy-setup")["modules"] = []any{"28-scripts", "45-agents"}
			},
			"release environment replaces the 45-agents module",
		},
		{
			"companion files while the environment owns them",
			func(m map[string]any) {
				component(m, "agent-config")["files"] = []any{map[string]any{
					"path": ".codex/AGENTS.md", "store": "codex/AGENTS.md",
					"sha256": strings.Repeat("a", 64),
				}}
			},
			"agent-config must be empty when agent-environment owns its files",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := publishedManifest(t)
			testCase.mutate(manifest)
			_, err := release.Validate(manifest)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if err.Error() != testCase.expected {
				t.Errorf("produced %q, expected %q", err.Error(), testCase.expected)
			}
		})
	}
}

func TestWorkerConfigurationRefusals(t *testing.T) {
	valid := map[string]any{
		"kind": "worker-configuration",
		"hosts": map[string]any{
			"omarchy-pc": map[string]any{
				"schema_version":  number("1"),
				"worker_id":       "omarchy-pc",
				"coordinator_id":  "normandy",
				"transport":       "ssh",
				"capabilities":    []any{"git", "huyang"},
				"provider_routes": []any{"anthropic/claude"},
				"credential_ref":  "secretref:f02-protocol/omarchy-pc",
			},
		},
	}
	if err := release.ValidateWorkers(valid); err != nil {
		t.Fatalf("the valid worker configuration was refused: %v", err)
	}
	entry := func(mutate func(map[string]any)) map[string]any {
		copied := map[string]any{
			"schema_version": number("1"), "worker_id": "omarchy-pc",
			"coordinator_id": "normandy", "transport": "ssh",
			"capabilities":    []any{"git", "huyang"},
			"provider_routes": []any{"anthropic/claude"},
			"credential_ref":  "secretref:f02-protocol/omarchy-pc",
		}
		mutate(copied)
		return map[string]any{
			"kind":  "worker-configuration",
			"hosts": map[string]any{"omarchy-pc": copied},
		}
	}
	cases := []struct {
		name     string
		value    map[string]any
		expected string
	}{
		{
			"a secret smuggled into the bootstrap",
			entry(func(e map[string]any) { e["credential"] = "hunter2" }),
			"unknown worker bootstrap fields; catalogs and secrets are forbidden",
		},
		{
			"an identity that does not match its host",
			entry(func(e map[string]any) { e["worker_id"] = "normandy" }),
			"worker identity must match its inventory host",
		},
		{
			"a shared secret reference",
			entry(func(e map[string]any) { e["credential_ref"] = "secretref:f02-protocol/shared" }),
			"worker needs its own opaque f02-protocol secret reference",
		},
		{
			"unsorted capabilities",
			entry(func(e map[string]any) { e["capabilities"] = []any{"huyang", "git"} }),
			"capabilities and provider routes must be sorted and unique",
		},
		{
			"a worker that cannot run the tools the fleet needs",
			entry(func(e map[string]any) { e["capabilities"] = []any{"git"} }),
			"worker must declare git and huyang",
		},
		{
			"a transport other than ssh",
			entry(func(e map[string]any) { e["transport"] = "http" }),
			"remote worker bootstrap transport must be ssh",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := release.ValidateWorkers(testCase.value)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if err.Error() != testCase.expected {
				t.Errorf("produced %q, expected %q", err.Error(), testCase.expected)
			}
		})
	}
}

func TestLoadVerifiesCompanionDigests(t *testing.T) {
	acceptAnyEnvironment(t)
	root := t.TempDir()
	manifest := publishedManifest(t)
	// A legacy-shaped release still carries companion files, which is the case
	// the digest check exists for.
	component(manifest, "agent-config")["files"] = []any{map[string]any{
		"path": ".codex/AGENTS.md", "store": "codex/AGENTS.md",
		"sha256": "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
	}}
	delete(components(manifest), "agent-environment")
	delete(components(manifest), "t3")
	delete(components(manifest), "worker-configuration")
	component(manifest, "t3-steward")["installer"] = "t3-update"
	delete(component(manifest, "omarchy-setup"), "modules")

	writeFile(t, filepath.Join(root, release.File), mustEncode(t, manifest))
	stored := filepath.Join(root, release.AgentConfigDir, "codex", "AGENTS.md")
	writeFile(t, stored, []byte("foo"))
	if _, err := release.Load(root); err != nil {
		t.Fatalf("a release whose companion matches its digest was refused: %v", err)
	}
	writeFile(t, stored, []byte("tampered"))
	_, err := release.Load(root)
	if err == nil ||
		err.Error() != "stored agent file does not match its manifest digest: codex/AGENTS.md" {
		t.Fatalf("produced %v", err)
	}
}

func components(manifest map[string]any) map[string]any {
	value, _ := manifest["components"].(map[string]any)
	return value
}

func component(manifest map[string]any, name string) map[string]any {
	value, _ := components(manifest)[name].(map[string]any)
	return value
}

func number(literal string) any {
	decoded, err := canonjson.Decode([]byte(literal))
	if err != nil {
		panic(err)
	}
	return decoded
}

func mustEncode(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := canonjson.MarshalFile(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}

func writeFile(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
