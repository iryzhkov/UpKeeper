package author_test

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/sharedgeneration"
)

func currentSharedFixture(t *testing.T) (*fixture, string, string) {
	t.Helper()
	f, old := sharedFixture(t)
	source := "../../" + author.SharedDirectory
	err := filepath.WalkDir(source, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		write(t, filepath.Join(f.root, author.SharedDirectory, relative), string(raw), info.Mode().Perm())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, old, f.commit(t, f.root, "current shared fixture")
}
func sharedOptions(t *testing.T, f *fixture, commit string) author.SharedPolicyOptions {
	t.Helper()
	_, raw := f.manifest(t)
	return author.SharedPolicyOptions{Repository: f.root, Commit: commit, ReleaseManifestPath: filepath.Join(f.root, "desired/release.json"), ExpectedReleaseManifestSHA256: agentenv.Digest(raw)}
}
func assertSharedRealOutputs(t *testing.T, p *author.SharedPreparation, model, effort, alias, claudeEffort string) {
	t.Helper()
	files := map[string]map[string]any{}
	for path, file := range p.Sources.Instructions {
		files[path] = map[string]any{"content": base64.StdEncoding.EncodeToString(file.Content), "sha256": agentenv.Digest(file.Content), "permissions": file.Permissions}
	}
	if len(files) != 18 {
		t.Fatal("not exact18")
	}
	files[".config/opencode/opencode.json"] = map[string]any{"content": ""}
	assertRealPolicyOutputs(t, files, p.PolicyBytes, model, effort, alias, claudeEffort)
	if p.Policy == nil || p.Policy.CurrentAuthorization != "unknown" || p.Policy.Readiness != "unassessed" {
		t.Fatal("incorrect provenance")
	}
	if p.Policy.CanonicalPolicySHA256 != agentenv.Digest(p.PolicyBytes) {
		t.Fatal("policy binding")
	}
}

func TestSharedPinnedPolicyPreparationAndMixedStore(t *testing.T) {
	repository, pin := os.Getenv("M14_SHARED_GENERATOR_REPOSITORY"), os.Getenv("M14_SHARED_GENERATOR_COMMIT")
	if repository == "" && pin == "" {
		t.Skip("current immutable shared generator fixture not supplied")
	}
	if repository == "" || pin != "b32c0b6f48e12b7d74a60f8a010f84bab73d299f" {
		t.Fatal("current shared generator inputs required")
	}
	if gitIn(t, repository, "rev-parse", "HEAD") != pin || gitIn(t, repository, "rev-parse", "HEAD^{tree}") != "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52" {
		t.Fatal("current fixture identity")
	}
	authorCatalog(t)
	f, old, _ := currentSharedFixture(t)
	policy := initialAuthorPolicy(t)
	addAuthorPolicy(t, f, policy)
	commit := gitIn(t, f.root, "rev-parse", "HEAD")
	options := sharedOptions(t, f, commit)
	p, err := author.PrepareSharedWithPolicy(options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	assertSharedRealOutputs(t, p, "gpt-6.1-sol", "medium", "opus", "medium")
	if p.Policy.UpstreamCommit != pin || p.Policy.UpstreamTree != "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52" {
		t.Fatal("new preparation must bind current provenance")
	}
	// Apply current upstream section-scoped assertions to actual policy-derived
	// residents/reference in disposable HOME, including obsolete recipe refusals.
	guidanceHome := t.TempDir()
	for path, file := range p.Sources.Instructions {
		write(t, filepath.Join(guidanceHome, path), string(file.Content), os.FileMode(file.Permissions))
		t.Logf("derived %s mode=%04o sha256=%s", path, file.Permissions, agentenv.Digest(file.Content))
	}
	guidance := exec.Command("python3", "-c", `
import pathlib,sys
sys.path.insert(0, str(pathlib.Path(sys.argv[1]) / "test"))
from m15_guidance import assert_m15_guidance
home = pathlib.Path(sys.argv[2])
residents = [(home / p).read_text() for p in (
    ".config/agents/AGENTS.md", ".codex/AGENTS.md", ".claude/omarchy-setup/CLAUDE.md")]
assert_m15_guidance(residents, (home / ".config/agents/t3-steward.md").read_text())
print("current b32 generated residents/reference: section-scoped M15 assertions passed")
`, repository, guidanceHome)
	guidance.Env = []string{"HOME=" + guidanceHome, "TMPDIR=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1"}
	if raw, err := guidance.CombinedOutput(); err != nil {
		t.Fatalf("current generated M15 guidance: %v %s", err, raw)
	} else {
		t.Log(string(raw))
	}
	// Compare exact current upstream output bytes/modes through its real parser.
	upstreamHome, upstreamTmp := t.TempDir(), t.TempDir()
	policyInput := filepath.Join(t.TempDir(), "policy.json")
	write(t, policyInput, string(p.PolicyBytes), 0400)
	command := exec.Command(filepath.Join(repository, author.Generator), "--root", repository, "--policy", policyInput)
	command.Env = []string{"HOME=" + upstreamHome, "TMPDIR=" + upstreamTmp, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("upstream generator: %v %s", err, raw)
	}
	for path, file := range p.Sources.Instructions {
		upstream, err := os.ReadFile(filepath.Join(upstreamHome, path))
		if err != nil {
			t.Fatal(err)
		}
		expected := strings.ReplaceAll(string(upstream), "omarchy-setup's config/", "UpKeeper's shared/agent-instructions config/")
		expected = strings.ReplaceAll(expected, "of omarchy-setup's", "of UpKeeper's shared/agent-instructions")
		expected = strings.Replace(expected, "UpKeeper, agent99, shared", "UpKeeper, shared", 1)
		info, err := os.Stat(filepath.Join(upstreamHome, path))
		if err != nil || int64(info.Mode().Perm()) != file.Permissions || !bytes.Equal([]byte(expected), file.Content) {
			t.Fatalf("upstream byte/mode parity %s: %v", path, err)
		}
	}
	check := exec.Command(command.Args[0], append(command.Args[1:], "--check")...)
	check.Env = command.Env
	if raw, err := check.CombinedOutput(); err != nil {
		t.Fatalf("upstream --check: %v %s", err, raw)
	}
	if p.Policy.InputManifestSHA256 != options.ExpectedReleaseManifestSHA256 {
		t.Fatal("input binding")
	}
	if _, err := author.PrepareShared(f.root, commit); err == nil {
		t.Fatal("v2 accepted implicit policy")
	}
	root := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := sharedgeneration.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := store.Stage(f.root, old)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := store.StageWithPolicy(options)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Receipt.SchemaVersion != 1 || v2.Receipt.SchemaVersion != 2 || v1.GenerationID == v2.GenerationID {
		t.Fatal("mixed generation identity")
	}
	policyPath := filepath.Join(root, "generations", v2.GenerationID, "inputs/route-policy.json")
	raw, err := os.ReadFile(policyPath)
	if err != nil || !bytes.Equal(raw, p.PolicyBytes) {
		t.Fatal("closed policy bytes", err)
	}
	info, err := os.Stat(policyPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("policy mode", err)
	}
	// Inspection must remain read-only and independent of coordinator availability.
	t.Setenv("PATH", t.TempDir())
	inspected, err := store.Inspect()
	if err != nil || inspected.GenerationID != v2.GenerationID {
		t.Fatal("offline inspection", err)
	}
	selected, err := store.SelectPrior(v1.GenerationID)
	if err != nil || selected.GenerationID != v1.GenerationID {
		t.Fatal("v1 rollback", err)
	}
	selected, err = store.SelectPrior(v2.GenerationID)
	if err != nil || selected.GenerationID != v2.GenerationID {
		t.Fatal("v2 reselection", err)
	}
}
func TestSharedPinnedPolicyChangedOrderAndRefusals(t *testing.T) {
	authorCatalog(t)
	f, _, _ := currentSharedFixture(t)
	policy := initialAuthorPolicy(t)
	execute := policy["roles"].([]any)[1].(map[string]any)
	execute["candidates"] = []any{
		map[string]any{"route": "codex/gpt-6-sol", "effort": "low", "tier": "standard"},
		map[string]any{"route": "claudeAgent/claude-sonnet-5-5", "effort": "high", "tier": "standard"},
		map[string]any{"route": "codex/gpt-6.1-sol", "effort": "medium", "tier": "standard"},
		map[string]any{"route": "claudeAgent/claude-opus-5-5", "effort": "medium", "tier": "standard"},
	}
	addAuthorPolicy(t, f, policy)
	commit := gitIn(t, f.root, "rev-parse", "HEAD")
	p, err := author.PrepareSharedWithPolicy(sharedOptions(t, f, commit))
	if err != nil {
		t.Fatal(err)
	}
	assertSharedRealOutputs(t, p, "gpt-6-sol", "low", "sonnet", "high")
	p.Close()
	for _, kind := range []string{"missing-Claude", "missing-Codex", "excluded-Claude", "excluded-Codex", "missing-role", "bad-pin", "missing-catalog", "missing-validator", "prior33-source"} {
		t.Run(kind, func(t *testing.T) {
			f, _, _ := currentSharedFixture(t)
			policy := initialAuthorPolicy(t)
			role := policy["roles"].([]any)[1].(map[string]any)
			if strings.HasPrefix(kind, "missing-") && (kind == "missing-Claude" || kind == "missing-Codex") {
				exclude := "claudeAgent/"
				if kind == "missing-Codex" {
					exclude = "codex/"
				}
				kept := []any{}
				for _, candidate := range role["candidates"].([]any) {
					if !strings.HasPrefix(candidate.(map[string]any)["route"].(string), exclude) {
						kept = append(kept, candidate)
					}
				}
				role["candidates"] = kept
			}
			if kind == "excluded-Claude" || kind == "excluded-Codex" {
				role["constraints"] = map[string]any{"tiers": []any{"economy"}}
				claudeTier, codexTier := "standard", "economy"
				if kind == "excluded-Codex" {
					claudeTier, codexTier = "economy", "standard"
				}
				role["candidates"] = []any{
					map[string]any{"route": "claudeAgent/claude-sonnet-5-5", "effort": "medium", "tier": claudeTier},
					map[string]any{"route": "codex/gpt-6-luna", "effort": "medium", "tier": codexTier},
				}
			}
			if kind == "missing-role" {
				policy["roles"] = policy["roles"].([]any)[1:]
			}
			if kind == "prior33-source" {
				path := filepath.Join(f.root, author.SharedDirectory, "owner.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				prior := strings.ReplaceAll(string(raw), "b32c0b6f48e12b7d74a60f8a010f84bab73d299f", "33cb805b5482d511071be7b344140e2ccb8c87da")
				prior = strings.ReplaceAll(prior, "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52", "3439bd874573cb88bbad852c5b66771f2ab80eeb")
				write(t, path, prior, 0644)
			}
			addAuthorPolicy(t, f, policy)
			commit := gitIn(t, f.root, "rev-parse", "HEAD")
			o := sharedOptions(t, f, commit)
			if kind == "bad-pin" {
				o.ExpectedReleaseManifestSHA256 = strings.Repeat("0", 64)
			}
			if kind == "missing-catalog" {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				t.Setenv("XDG_STATE_HOME", t.TempDir())
			}
			if kind == "missing-validator" {
				prior := release.EnvironmentValidator
				release.EnvironmentValidator = nil
				defer func() { release.EnvironmentValidator = prior }()
			}
			_, before := f.manifest(t)
			if p, err := author.PrepareSharedWithPolicy(o); err == nil {
				p.Close()
				t.Fatal("refusal accepted")
			} else if kind == "prior33-source" && !strings.Contains(err.Error(), "unsupported shared owner identity") {
				t.Fatal("prior source must refuse before derivation", err)
			}
			_, after := f.manifest(t)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed manifest")
			}
		})
	}

}
