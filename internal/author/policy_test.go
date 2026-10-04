package author_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/policytest"
)

// authorCatalog is a bounded read-only protocol fixture, not a mock parser.
// The real consumer validates configured sole-wildcard authorizations while the
// worker is offline/stale; no model, enrollment or live coordinator is invoked.
func authorCatalog(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "m14-author-catalog-")
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
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer connection.Close()
				connection.SetDeadline(time.Now().Add(5 * time.Second))
				var size [4]byte
				if _, err := io.ReadFull(connection, size[:]); err != nil {
					t.Error(err)
					return
				}
				n := binary.BigEndian.Uint32(size[:])
				if n > 1<<20 {
					t.Error("oversize catalog query")
					return
				}
				raw := make([]byte, n)
				if _, err := io.ReadFull(connection, raw); err != nil {
					t.Error(err)
					return
				}
				var query struct {
					Operation string
					Query     struct{ Kind string }
				}
				if err := json.Unmarshal(raw, &query); err != nil || query.Operation != "query" || query.Query.Kind != "workers" {
					t.Errorf("unexpected catalog operation: %s (%v)", raw, err)
					return
				}
				providers := []any{
					map[string]any{"instance": "codex", "models": []string{"*"}, "quotaPool": "exhausted"},
					map[string]any{"instance": "claudeAgent", "models": []string{"*"}, "quotaPool": "exhausted"},
					map[string]any{"instance": "opencode", "models": []string{"*"}, "quotaPool": "exhausted"},
				}
				response := map[string]any{"version": "backlog.admin.local/v1", "response": map[string]any{
					"version": "backlog.admin/v1", "kind": "workers", "workers": []any{map[string]any{
						"providers": providers, "health": "offline", "state": "offline", "stale": true, "enrolled": true,
					}},
				}}
				encoded, _ := json.Marshal(response)
				binary.BigEndian.PutUint32(size[:], uint32(len(encoded)))
				connection.Write(size[:])
				connection.Write(encoded)
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); wg.Wait() })
	write(t, filepath.Join(dir, "t3-steward/config.yaml"), "state_path: "+state+"\n", 0o600)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("HOME", dir)
}

const generatorPin = "88d9b534a6648e2615037dc3a655e6a8d0e9bebc"

var policyPaths = []string{
	".claude/omarchy-setup/CLAUDE.md",
	".config/agents/AGENTS.md", ".config/agents/huyang.md", ".config/agents/jocasta.md",
	".config/agents/ov-memory.md", ".config/agents/t3-steward.md", ".config/agents/t3-campaign-executor-briefs.md",
	".codex/AGENTS.md",
	".claude/agents/reader.md", ".claude/agents/executor.md", ".claude/agents/reviewer.md",
	".claude/agents/critical-reviewer.md", ".claude/agents/planner.md",
	".codex/reader.config.toml", ".codex/executor.config.toml", ".codex/reviewer.config.toml",
	".codex/critical-reviewer.config.toml", ".codex/planner.config.toml",
}

// This generator is an argument/ownership/failure fixture, not evidence of the
// published generator's semantics. TestAuthorPinnedPolicyGenerator supplies that.
const policyGenerator = `#!/usr/bin/env python3
import hashlib, json, os, pathlib, sys
args = sys.argv[1:]
assert args[0] == "--root" and args[2] == "--policy"
root, policy = pathlib.Path(args[1]), pathlib.Path(args[3])
home = pathlib.Path(os.environ["HOME"])
assert home != pathlib.Path(os.environ.get("M14_LIVE_HOME", "/impossible"))
assert policy.parent == home.parent and root.parent == home.parent
assert policy.stat().st_mode & 0o777 == 0o400
raw = policy.read_bytes()
document = json.loads(raw)
digest = hashlib.sha256(raw).hexdigest()
paths = json.loads((root / "inventory.json").read_text())
outputs = {}
for path in paths:
    content = "# Generated; policy SHA256: " + digest + "\n" + json.dumps(document, sort_keys=True) + "\n"
    outputs[path] = content.encode()
if "--check" in args:
    sys.exit(0 if all((home / path).read_bytes() == data for path, data in outputs.items()) else 3)
for path, data in outputs.items():
    target = home / path
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(data)
    target.chmod(0o644)
`

func initialAuthorPolicy(t *testing.T) map[string]any {
	t.Helper()
	policytest.RequireCLI(t)
	command := exec.Command("t3-steward", "policy", "show", "--file", "../../testdata/fleet/route-policy.yaml", "--json")
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real policy consumer: %v: %s", err, raw)
	}
	shown, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	// The initial fixture has only tier constraints, spelled identically in
	// the consumer envelope and policy schema. No schema is implemented here.
	return map[string]any{"schema": shown["schema"], "roles": shown["roles"]}
}

func addAuthorPolicy(t *testing.T, subject *fixture, policy any) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/fleet/fleet-intent.json")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	intent["route_policy"] = policy
	manifest, _ := subject.manifest(t)
	manifest["components"].(map[string]any)[fleet.Component] = intent
	encoded, err := canonjson.MarshalFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(subject.root, "desired/release.json"), string(encoded), 0o644)
	// Inventory.Load is read-only and needs both repository companions.
	for _, relative := range []string{"inventory/hosts.yml", "desired/components.yml"} {
		data, err := os.ReadFile(filepath.Join("../..", relative))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(subject.root, relative), string(data), 0o644)
	}
	subject.commit(t, subject.root, "reviewed policy fixture")
}

func installPolicyFixture(t *testing.T, subject *fixture, generator string) string {
	t.Helper()
	write(t, filepath.Join(subject.setup, author.Generator), generator, 0o755)
	inventory, _ := json.Marshal(policyPaths)
	write(t, filepath.Join(subject.setup, "inventory.json"), string(inventory), 0o644)
	return subject.commit(t, subject.setup, "policy generator fixture")
}

func assertScratchClean(t *testing.T, subject *fixture) {
	t.Helper()
	entries, err := os.ReadDir(subject.temporary)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch not cleaned: %v %v", entries, err)
	}
}

func assertRefusedUnchanged(t *testing.T, subject *fixture, commit string, expected ...string) {
	t.Helper()
	_, before := subject.manifest(t)
	tree := gitIn(t, subject.setup, "rev-parse", "HEAD^{tree}")
	if result, err := subject.author(t, commit, false); err == nil || result.Written {
		t.Fatalf("expected refusal without publication: %+v %v", result, err)
	} else if strings.Contains(err.Error(), "route_policy validate refused") {
		t.Fatalf("catalog refusal masked the causal generator/schema refusal: %v", err)
	} else {
		for _, fragment := range expected {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("wrong causal refusal, expected %q: %v", fragment, err)
			}
		}
	}
	_, after := subject.manifest(t)
	if !bytes.Equal(before, after) || tree != gitIn(t, subject.setup, "rev-parse", "HEAD^{tree}") ||
		gitIn(t, subject.setup, "status", "--porcelain") != "" ||
		gitIn(t, subject.root, "status", "--porcelain") != "" {
		t.Fatal("refusal changed manifest or unrelated source")
	}
	assertScratchClean(t, subject)
}

func TestAuthorPolicyDispatchAndPreservation(t *testing.T) {
	authorCatalog(t)
	subject := newFixture(t)
	addAuthorPolicy(t, subject, initialAuthorPolicy(t))
	commit := installPolicyFixture(t, subject, policyGenerator+"\n(home / '.codex/executor.config.toml').chmod(0o600)\n(home / '.claude/agents/executor.md').chmod(0o755)\n")
	live := t.TempDir()
	conflict := filepath.Join(live, fleet.RoutePolicyDestination)
	write(t, conflict, "invalid conflicting live policy\n", 0o600)
	write(t, filepath.Join(live, ".config/opencode/opencode.json"), "{invalid live config", 0o600)
	t.Setenv("HOME", live)
	// Carry unrelated native files through the existing exact Instructions map.
	if _, err := subject.author(t, commit, false); err != nil {
		t.Fatal(err)
	}
	manifest, _ := subject.manifest(t)
	component := manifest["components"].(map[string]any)["agent-environment"].(map[string]any)
	component, _, err := agentenv.RefreshDerived(component, agentenv.DerivedSources{
		SkillsRoot: filepath.Join(subject.setup, "config/claude/skills"),
		Instructions: map[string]agentenv.DerivedFile{
			".claude/agents/personal.md":  {Content: []byte("personal Claude\n"), Permissions: 0o600},
			".codex/personal.config.toml": {Content: []byte("personal Codex\n"), Permissions: 0o644},
		},
	}, gitIn(t, subject.root, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	manifest["components"].(map[string]any)["agent-environment"] = component
	raw, _ := canonjson.MarshalFile(manifest)
	write(t, filepath.Join(subject.root, "desired/release.json"), string(raw), 0o644)
	subject.commit(t, subject.root, "personal native files")

	policy := initialAuthorPolicy(t)
	role := policy["roles"].([]any)[1].(map[string]any)
	candidates := role["candidates"].([]any)
	candidates[0], candidates[1] = candidates[1], candidates[0]
	candidates[0].(map[string]any)["effort"] = "low"
	candidates[0].(map[string]any)["route"] = "codex/gpt-6-sol"
	addAuthorPolicy(t, subject, policy)
	_, before := subject.manifest(t)
	result, err := subject.author(t, commit, true)
	if err != nil || !result.Changed || result.Written {
		t.Fatalf("dry-run: %+v %v", result, err)
	}
	_, after := subject.manifest(t)
	if !bytes.Equal(before, after) || gitIn(t, subject.root, "status", "--porcelain") != "" {
		t.Fatal("dry-run wrote")
	}
	result, err = subject.author(t, commit, false)
	if err != nil || !result.Written {
		t.Fatalf("author: %+v %v", result, err)
	}
	manifest, _ = subject.manifest(t)
	files := environmentFiles(t, manifest, "files")
	rendered, _ := canonjson.MarshalFile(policy)
	for _, path := range policyPaths {
		if files[path] == nil || !strings.Contains(content(t, files[path]), agentenv.Digest(rendered)) {
			t.Errorf("missing exact rendered digest in %s", path)
		}
		if actions(result)[path] != agentenv.RefreshChanged {
			t.Errorf("policy edit did not change %s", path)
		}
	}
	if fmt.Sprint(files[".codex/executor.config.toml"]["permissions"]) != "384" || fmt.Sprint(files[".claude/agents/executor.md"]["permissions"]) != "493" {
		t.Fatal("generated output modes were normalized instead of preserved")
	}
	for path, want := range map[string]string{".claude/agents/personal.md": "personal Claude\n", ".codex/personal.config.toml": "personal Codex\n"} {
		if content(t, files[path]) != want || environmentFiles(t, manifest, "retired_files")[path] != nil {
			t.Errorf("personal file changed/retired: %s", path)
		}
	}
	if actions(result)[".config/opencode/opencode.json"] != "" || strings.Contains(content(t, files[".config/opencode/opencode.json"]), "invalid live config") {
		t.Fatal("changed inherited OpenCode or captured live data")
	}
	liveRaw, _ := os.ReadFile(conflict)
	if string(liveRaw) != "invalid conflicting live policy\n" {
		t.Fatal("live policy touched")
	}
	subject.commit(t, subject.root, "authored policy")
	_, before = subject.manifest(t)
	result, err = subject.author(t, commit, false)
	_, after = subject.manifest(t)
	if err != nil || result.Changed || result.Written || !bytes.Equal(before, after) {
		t.Fatalf("repeat: %+v %v", result, err)
	}
	assertScratchClean(t, subject)
}

func TestAuthorPolicyOutputRefusals(t *testing.T) {
	authorCatalog(t)
	for _, path := range policyPaths {
		for _, kind := range []string{"missing", "symlink"} {
			t.Run(kind+"/"+path, func(t *testing.T) {
				subject := newFixture(t)
				addAuthorPolicy(t, subject, initialAuthorPolicy(t))
				mutation := fmt.Sprintf("\n(home / %q).unlink()\n", path)
				if kind == "symlink" {
					mutation += fmt.Sprintf("(home / %q).symlink_to(policy)\n", path)
				}
				commit := installPolicyFixture(t, subject, policyGenerator+mutation)
				assertRefusedUnchanged(t, subject, commit, "omarchy-setup:")
			})
		}
	}
	for name, mutation := range map[string]string{
		"directory output":      "\n(home / '.codex/reader.config.toml').unlink()\n(home / '.codex/reader.config.toml').mkdir()\n",
		"empty output":          "\n(home / '.codex/reader.config.toml').write_bytes(b'')\n",
		"unsupported mode":      "\n(home / '.codex/reader.config.toml').chmod(0o666)\n",
		"invalid output":        "\n(home / '.codex/reader.config.toml').write_bytes(b'\\x00')\n",
		"drift":                 "\n(home / '.codex/reader.config.toml').write_text('invalid projection')\n",
		"policy mutation":       "\npolicy.chmod(0o600)\npolicy.write_text('{}')\n",
		"source mutation":       "\n(root / 'config/claude/CLAUDE.md').write_text('mutated')\n",
		"linked parent":         "\nimport shutil\nshutil.rmtree(home / '.claude/agents')\n(home / '.claude/agents').symlink_to(home / '.config/agents')\n",
		"hardlink":              "\n(home / '.codex/reader.config.toml').unlink()\nos.link(policy, home / '.codex/reader.config.toml')\n",
		"unexpected OpenCode":   "\n(home / '.config/opencode').mkdir()\n",
		"refusal":               "\nsys.exit(1)\n",
		"check mutation":        "",
		"unsupported generator": "",
	} {
		t.Run(name, func(t *testing.T) {
			subject := newFixture(t)
			addAuthorPolicy(t, subject, initialAuthorPolicy(t))
			generator := policyGenerator + mutation
			if name == "unsupported generator" {
				generator = fakeGenerator
			}
			if name == "check mutation" {
				generator = strings.Replace(policyGenerator, `if "--check" in args:`, `if "--check" in args:
    (home / ".codex/reader.config.toml").chmod(0o600)`, 1)
			}
			commit := installPolicyFixture(t, subject, generator)
			assertRefusedUnchanged(t, subject, commit, "omarchy-setup:")
		})
	}
}

func TestAuthorPolicySchemaRefusals(t *testing.T) {
	policytest.RequireCLI(t)
	for _, name := range []string{"null", "schema", "unknown", "missing effort", "max"} {
		t.Run(name, func(t *testing.T) {
			subject := newFixture(t)
			policy := initialAuthorPolicy(t)
			var value any = policy
			switch name {
			case "null":
				value = nil
			case "schema":
				policy["schema"] = "route-policy/v2"
			case "unknown":
				policy["secret"] = "invalid"
			case "missing effort":
				delete(policy["roles"].([]any)[0].(map[string]any)["candidates"].([]any)[0].(map[string]any), "effort")
			case "max":
				policy["roles"].([]any)[0].(map[string]any)["candidates"].([]any)[0].(map[string]any)["effort"] = "max"
			}
			addAuthorPolicy(t, subject, value)
			assertRefusedUnchanged(t, subject, gitIn(t, subject.setup, "rev-parse", "HEAD"))
		})
	}
}

// TestAuthorPinnedPolicyGenerator is included in the ordinary Go suite. Only
// absent opt-in inputs permit a skip; a supplied wrong/incomplete pin fails.
func TestAuthorPinnedPolicyGenerator(t *testing.T) {
	repository, pin := os.Getenv("M14_GENERATOR_REPOSITORY"), os.Getenv("M14_GENERATOR_COMMIT")
	if repository == "" && pin == "" {
		t.Skip("immutable generator fixture not supplied")
	}
	if repository == "" || pin != generatorPin {
		t.Fatal("both immutable generator inputs and exact published pin are required")
	}
	policytest.RequireCLI(t)
	authorCatalog(t)
	if gitIn(t, repository, "rev-parse", "HEAD") != generatorPin ||
		gitIn(t, repository, "rev-parse", "HEAD^{tree}") != "db9750657bac6f3fbb2f0ded4a139c0f585333ec" ||
		gitIn(t, repository, "status", "--porcelain", "--untracked-files=no") != "" {
		t.Fatal("generator fixture is not the clean immutable published source")
	}
	subject := newFixture(t)
	// Separate local Git clone; never modify the supplied immutable generator.
	setup := filepath.Join(t.TempDir(), "setup")
	gitIn(t, filepath.Dir(setup), "clone", "--quiet", "--no-hardlinks", repository, setup)
	subject.setup = setup
	gitIn(t, setup, "checkout", "--quiet", "--detach", generatorPin)
	gitIn(t, setup, "branch", "--force", "main", generatorPin)
	gitIn(t, setup, "remote", "set-url", "origin", fixtureOrigin)
	gitIn(t, setup, "config", "url."+setup+".insteadOf", fixtureOrigin)
	manifest, _ := subject.manifest(t)
	manifest["components"].(map[string]any)["omarchy-setup"].(map[string]any)["commit"] = generatorPin
	encoded, _ := canonjson.MarshalFile(manifest)
	write(t, filepath.Join(subject.root, "desired/release.json"), string(encoded), 0o644)
	policy := initialAuthorPolicy(t)
	addAuthorPolicy(t, subject, policy)
	live := t.TempDir()
	t.Setenv("HOME", live)
	write(t, filepath.Join(live, fleet.RoutePolicyDestination), "conflicting live policy", 0o600)
	write(t, filepath.Join(live, ".config/opencode/opencode.json"), "{invalid}", 0o600)

	rendered, _ := canonjson.MarshalFile(policy)
	originalPolicy, _ := os.ReadFile("../../testdata/fleet/route-policy.yaml")
	if bytes.Equal(rendered, originalPolicy) || agentenv.Digest(rendered) == agentenv.Digest(originalPolicy) {
		t.Fatal("fixture must distinguish raw YAML from canonical rendering")
	}
	document, err := fleet.Parse(manifestPolicyIntent(t, subject))
	if err != nil || !bytes.Equal(rendered, document.RoutePolicyBytes()) {
		t.Fatalf("fleet rendering: %v", err)
	}
	_, original := subject.manifest(t)
	dry, err := subject.author(t, generatorPin, true)
	_, current := subject.manifest(t)
	if err != nil || dry.Written || !dry.Changed || !bytes.Equal(original, current) {
		t.Fatalf("real dry run: %+v %v", dry, err)
	}
	result, err := subject.author(t, generatorPin, false)
	if actions(result)[".config/opencode/opencode.json"] != "" {
		t.Fatal("generator refreshed inherited OpenCode")
	}
	if err != nil || !result.Written {
		t.Fatalf("actual pinned generator author dispatch: %+v %v", result, err)
	}
	manifest, _ = subject.manifest(t)
	firstFiles := environmentFiles(t, manifest, "files")
	assertRealPolicyOutputs(t, firstFiles, rendered, "gpt-6.1-sol", "medium", "opus", "medium")
	subject.commit(t, subject.root, "real authored fixture")
	result, err = subject.author(t, generatorPin, false)
	if err != nil || result.Changed || result.Written {
		t.Fatalf("real repeat: %+v %v", result, err)
	}

	// Candidate order/model/effort remain policy inputs. Select the first
	// same-provider candidate from a changed ordered list on both frontends.
	execute := policy["roles"].([]any)[1].(map[string]any)
	execute["candidates"] = []any{
		map[string]any{"route": "codex/gpt-6-sol", "effort": "low", "tier": "standard"},
		map[string]any{"route": "claudeAgent/claude-sonnet-5-5", "effort": "high", "tier": "standard"},
		map[string]any{"route": "codex/gpt-6.1-sol", "effort": "medium", "tier": "standard"},
		map[string]any{"route": "claudeAgent/claude-opus-5-5", "effort": "medium", "tier": "standard"},
	}
	addAuthorPolicy(t, subject, policy)
	rendered, _ = canonjson.MarshalFile(policy)
	if _, err := subject.author(t, generatorPin, false); err != nil {
		t.Fatal(err)
	}
	manifest, _ = subject.manifest(t)
	files := environmentFiles(t, manifest, "files")
	assertRealPolicyOutputs(t, files, rendered, "gpt-6-sol", "low", "sonnet", "high")
	for _, path := range []string{".claude/omarchy-setup/CLAUDE.md", ".config/agents/AGENTS.md", ".codex/AGENTS.md", ".claude/agents/executor.md", ".codex/executor.config.toml"} {
		if firstFiles[path]["sha256"] == files[path]["sha256"] {
			t.Errorf("real changed policy did not flow through %s", path)
		}
	}
	if !reflect.DeepEqual(policy, manifestPolicyIntent(t, subject)["route_policy"]) {
		t.Fatal("author changed reviewed policy inputs")
	}
	assertScratchClean(t, subject)
	if gitIn(t, setup, "status", "--porcelain") != "" {
		t.Fatal("pinned setup source mutated")
	}
	liveBytes, _ := os.ReadFile(filepath.Join(live, fleet.RoutePolicyDestination))
	if string(liveBytes) != "conflicting live policy" {
		t.Fatal("live policy changed")
	}
	t.Logf("actual generator %s and real CLI: 18 exact files; canonical policy digest %s; provider/model/effort, changed order, dry-run, no-op and cleanup verified", generatorPin, agentenv.Digest(rendered))

	for _, kind := range []string{"missing native role", "unsupported provider", "unsupported alias"} {
		t.Run(kind, func(t *testing.T) {
			p := initialAuthorPolicy(t)
			if kind == "missing native role" {
				p["roles"] = p["roles"].([]any)[1:]
			}
			if kind == "unsupported provider" {
				p["roles"].([]any)[0].(map[string]any)["candidates"].([]any)[0].(map[string]any)["route"] = "opencode/claude-opus-5-5"
			}
			if kind == "unsupported alias" {
				p["roles"].([]any)[0].(map[string]any)["candidates"].([]any)[0].(map[string]any)["route"] = "claudeAgent/unknown-family"
			}
			addAuthorPolicy(t, subject, p)
			assertRefusedUnchanged(t, subject, generatorPin, "policy generator refused")
		})
	}
}

func manifestPolicyIntent(t *testing.T, subject *fixture) map[string]any {
	t.Helper()
	manifest, _ := subject.manifest(t)
	return manifest["components"].(map[string]any)[fleet.Component].(map[string]any)
}

func assertRealPolicyOutputs(t *testing.T, files map[string]map[string]any, policy []byte, model, effort, alias, claudeEffort string) {
	t.Helper()
	digest := agentenv.Digest(policy)
	for _, path := range policyPaths {
		file := files[path]
		if file == nil {
			t.Fatalf("missing mandatory generated file %s", path)
		}
		raw := content(t, file)
		if file["sha256"] != agentenv.Digest([]byte(raw)) || fmt.Sprint(file["permissions"]) != "420" {
			t.Errorf("byte digest/mode mismatch: %s", path)
		}
		if strings.HasPrefix(path, ".claude/agents/") || strings.HasSuffix(path, ".config.toml") ||
			path == ".claude/omarchy-setup/CLAUDE.md" || path == ".config/agents/AGENTS.md" || path == ".codex/AGENTS.md" {
			if !strings.Contains(raw, "policy SHA256: "+digest) {
				t.Errorf("canonical rendered policy digest missing: %s", path)
			}
		}
	}
	for _, role := range []struct{ native, codex, effort, claude, claudeEffort string }{
		{"reader", "gpt-6-luna", "medium", "sonnet", "medium"},
		{"executor", model, effort, alias, claudeEffort},
		{"reviewer", "gpt-6.1-sol", "medium", "opus", "medium"},
		{"critical-reviewer", "gpt-6-astra", "medium", "fable", "medium"},
		{"planner", "gpt-6.1-sol", "high", "opus", "high"},
	} {
		nativeCodex := content(t, files[".codex/"+role.native+".config.toml"])
		nativeClaude := content(t, files[".claude/agents/"+role.native+".md"])
		if !strings.Contains(nativeCodex, "model = "+fmt.Sprintf("%q", role.codex)) ||
			!strings.Contains(nativeCodex, "model_reasoning_effort = "+fmt.Sprintf("%q", role.effort)) ||
			!strings.Contains(nativeClaude, "\nmodel: "+role.claude+"\n") ||
			!strings.Contains(nativeClaude, "\neffort: "+role.claudeEffort+"\n") {
			t.Errorf("policy provider/model/effort mismatch for %s", role.native)
		}
		for _, text := range []string{nativeCodex, nativeClaude} {
			if strings.Contains(text, "tools:") || strings.Contains(text, "permissionMode:") || strings.Contains(text, "sandbox_mode") {
				t.Errorf("unexpected tool/permission grant for %s", role.native)
			}
		}
	}
	codex := content(t, files[".codex/executor.config.toml"])
	claude := content(t, files[".claude/agents/executor.md"])
	if !strings.Contains(codex, "model = "+fmt.Sprintf("%q", model)) ||
		!strings.Contains(codex, "model_reasoning_effort = "+fmt.Sprintf("%q", effort)) ||
		!strings.Contains(claude, "\nmodel: "+alias+"\n") ||
		!strings.Contains(claude, "\neffort: "+claudeEffort+"\n") {
		t.Fatalf("native provider/effort mismatch:\n%s\n%s", codex, claude)
	}
	if strings.Contains(content(t, files[".config/opencode/opencode.json"]), "{invalid}") {
		t.Fatal("captured live conditional OpenCode output")
	}
}
