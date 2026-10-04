package author_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/sharedgeneration"
)

// The pinned helper remains the renderer. Only this disposable source fixture
// adds inert text and selects a supported private creation mode for staging.
func TestSharedLargePrivateInstructionStage(t *testing.T) {
	authorCatalog(t)
	f, _, _ := currentSharedFixture(t)
	helper := filepath.Join(f.root, author.SharedDirectory, "config/bin/agents-role-project")
	raw, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	write(t, helper, strings.Replace(string(raw), "else 0o644", "else 0o600", 1), 0755)
	claude := filepath.Join(f.root, author.SharedDirectory, "config/claude/CLAUDE.md")
	raw, err = os.ReadFile(claude)
	if err != nil {
		t.Fatal(err)
	}
	write(t, claude, string(raw)+"\n"+strings.Repeat("inert boundary fixture text\n", 14000), 0644)
	addAuthorPolicy(t, f, initialAuthorPolicy(t))
	options := sharedOptions(t, f, gitIn(t, f.root, "rev-parse", "HEAD"))
	root := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := sharedgeneration.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := store.StageWithPolicy(options)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, file := range stage.Receipt.Files {
		if file.Path != "instructions/.claude/omarchy-setup/CLAUDE.md" {
			continue
		}
		info, err := os.Stat(filepath.Join(root, "generations", stage.GenerationID, file.Path))
		if err != nil || info.Size() <= 256<<10 || file.Mode != 0600 {
			t.Fatal("missing large private instruction", info, file.Mode, err)
		}
		found = true
	}
	if !found {
		t.Fatal("missing instruction")
	}
	if _, err := store.Inspect(); err != nil {
		t.Fatal(err)
	}
}
