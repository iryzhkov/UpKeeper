package publish_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/publish"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const inventoryFile = "inventory/hosts.yml"

// authoredInventoryRepository turns the authored fixture into a real Git
// repository whose committed inventory lacks agent-a, while the working tree
// holds the full inventory. It returns the fixture, a Git helper, the baseline
// commit and the full inventory bytes.
func authoredInventoryRepository(t *testing.T) (
	*fixture, func(...string) string, string, []byte,
) {
	t.Helper()
	f, _, _ := prepareAuthoredFixture(t)
	if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
		t.Fatal(err)
	}
	runGit := func(arguments ...string) string {
		t.Helper()
		result := procx.Run(procx.Command{Argv: append([]string{"git", "-C", f.root}, arguments...)})
		if !result.OK() {
			t.Fatalf("git %v: %s", arguments, result.Output())
		}
		return strings.TrimSpace(result.Stdout)
	}
	path := filepath.Join(f.root, inventoryFile)
	full := mustRead(t, path)
	var document map[string]any
	if err := json.Unmarshal(full, &document); err != nil {
		t.Fatal(err)
	}
	hosts := document["all"].(map[string]any)["hosts"].(map[string]any)
	if _, present := hosts["agent-a"]; !present {
		t.Fatal("the fixture inventory has no agent-a to withhold")
	}
	delete(hosts, "agent-a")
	without, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(without, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("init", "--quiet")
	runGit("config", "user.name", "UpKeeper Test")
	runGit("config", "user.email", "upkeeper@example.invalid")
	runGit("add", ".")
	runGit("commit", "--quiet", "-m", "baseline without agent-a")
	return f, runGit, runGit("rev-parse", "HEAD"), full
}

// referenceAgentA makes the reviewed release depend on the inventory host
// agent-a, the way the release that enrolled the fresh VMs did, and returns
// the reviewed digest.
func referenceAgentA(t *testing.T, f *fixture) string {
	t.Helper()
	published, err := release.Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	workers := published.Components()["worker-configuration"].(map[string]any)
	entry := workers["hosts"].(map[string]any)["agent-a"]
	if entry == nil {
		t.Fatal("the published release has no agent-a worker to copy")
	}
	value, err := release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	value.Components()["worker-configuration"] = map[string]any{
		"kind": "worker-configuration", "hosts": map[string]any{"agent-a": entry},
	}
	if _, err := release.Write(f.root, value); err != nil {
		t.Fatal(err)
	}
	reloaded, err := release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	return reloaded.Digest()
}

func authoredOptions(f *fixture, head, digest string) publish.Options {
	options := f.options()
	options.Runner = func(command procx.Command) procx.Result {
		if len(command.Argv) > 0 && command.Argv[0] == "git" {
			return procx.Run(command)
		}
		return f.run(command)
	}
	options.Remotes = nil
	options.Push = false
	options.Authored = true
	options.ExpectedHead = head
	options.ExpectedReleaseSHA256 = digest
	return options
}

// The release that enrolled agent-a was published without the inventory entry
// it depends on, and CI failed on the commit. The inventory change the release
// needs is part of what an authored publication commits.
func TestAuthoredPublicationCommitsTheInventoryTheReleaseNeeds(t *testing.T) {
	f, runGit, head, full := authoredInventoryRepository(t)
	if err := os.WriteFile(filepath.Join(f.root, inventoryFile), full, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := referenceAgentA(t, f)
	if _, err := publish.Release(f.root, authoredOptions(f, head, digest)); err != nil {
		t.Fatalf("authored publication: %v", err)
	}
	if runGit("rev-parse", "HEAD") == head {
		t.Fatal("nothing was committed")
	}
	if committed := runGit("show", "HEAD:"+inventoryFile); committed != strings.TrimSpace(string(full)) {
		t.Fatal("the committed inventory is not the inventory the release was validated against")
	}
	if changed := runGit("diff", "--name-only", "HEAD"); changed != "" {
		t.Fatalf("paths still differ from the published commit: %q", changed)
	}
}

// When the inventory the operator has does not know the host either, nothing
// is published and the refusal names the host and the file to fix.
func TestAuthoredPublicationRefusesAReleaseTheInventoryDoesNotKnow(t *testing.T) {
	f, runGit, head, _ := authoredInventoryRepository(t)
	digest := referenceAgentA(t, f)
	_, err := publish.Release(f.root, authoredOptions(f, head, digest))
	if err == nil || !strings.Contains(err.Error(), "agent-a") ||
		!strings.Contains(err.Error(), inventoryFile) {
		t.Fatalf("want a refusal naming agent-a and %s, got %v", inventoryFile, err)
	}
	if runGit("rev-parse", "HEAD") != head {
		t.Fatal("a refused publication moved HEAD")
	}
}

// An inventory edit the release does not depend on stays the operator's own
// work: an authored publication neither commits nor discards it.
func TestAuthoredPublicationLeavesAnUnrelatedInventoryEditAlone(t *testing.T) {
	f, runGit, head, full := authoredInventoryRepository(t)
	committed := runGit("show", "HEAD:"+inventoryFile)
	if err := os.WriteFile(filepath.Join(f.root, inventoryFile), full, 0o644); err != nil {
		t.Fatal(err)
	}
	value, err := release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	value.Manifest["captured_from"] = "reviewed-controller"
	if _, err = release.Write(f.root, value); err != nil {
		t.Fatal(err)
	}
	reloaded, err := release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publish.Release(f.root, authoredOptions(f, head, reloaded.Digest())); err != nil {
		t.Fatalf("authored publication: %v", err)
	}
	if got := runGit("show", "HEAD:"+inventoryFile); got != committed {
		t.Fatal("an unrelated inventory edit was committed")
	}
	if changed := runGit("diff", "--name-only"); changed != inventoryFile {
		t.Fatalf("working changes after publication = %q, want only %s", changed, inventoryFile)
	}
}
