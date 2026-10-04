package author_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedCurrentSourceBlobClosure(t *testing.T) {
	repository, commit := os.Getenv("M14_SHARED_GENERATOR_REPOSITORY"), os.Getenv("M14_SHARED_GENERATOR_COMMIT")
	if repository == "" && commit == "" {
		t.Skip("current immutable shared generator inputs not supplied")
	}
	if repository == "" || commit != "33cb805b5482d511071be7b344140e2ccb8c87da" {
		t.Fatal("both exact current shared source pins required")
	}
	if gitIn(t, repository, "rev-parse", "HEAD") != commit || gitIn(t, repository, "rev-parse", "HEAD^{tree}") != "3439bd874573cb88bbad852c5b66771f2ab80eeb" {
		t.Fatal("current source identity")
	}
	files := []string{"config/bin/agents-instructions-gen", "config/bin/agents-role-project", "config/claude/CLAUDE.md", "config/claude/skills/huyang/SKILL.md", "config/claude/skills/implementation-session/SKILL.md", "config/claude/skills/implementation-session/references/prompt-template.md", "config/claude/skills/ov-memory-curation/SKILL.md", "config/claude/skills/ov-memory/SKILL.md", "config/claude/skills/publish-agent-tooling/SKILL.md", "config/claude/skills/t3-campaign/SKILL.md", "config/claude/skills/t3-campaign/references/executor-briefs.md", "config/claude/skills/t3-task/SKILL.md", "config/claude/skills/t3-wait/SKILL.md"}
	for _, path := range files {
		t.Run(path, func(t *testing.T) {
			command := exec.Command("git", "--no-replace-objects", "-C", repository, "show", commit+":"+path)
			upstream, e := command.Output()
			if e != nil {
				t.Fatal(e)
			}
			expected := string(upstream)
			if path == "config/claude/CLAUDE.md" || path == "config/claude/skills/publish-agent-tooling/SKILL.md" {
				expected = strings.Replace(expected, "UpKeeper, agent99, shared", "UpKeeper, shared", 1)
			}
			if path == "config/bin/agents-instructions-gen" {
				expected = strings.Replace(expected, "mode=install\npolicy=''", "mode=install\npolicy=''\nshared_root=0", 1)
				expected = strings.Replace(expected, "  --check)\n", "  --shared-root)\n    shared_root=1\n    shift\n    ;;\n  --check)\n", 1)
				expected = strings.Replace(expected, "# ------------------------------------------------------------------- root --", "# Shared transport is private derivation only; no installed or inferred root.\n((shared_root)) || die \"candidate generator requires --shared-root; host installation is not implemented\"\n[[ -n $root && -f $root/owner.json && -n $policy ]] || die \"candidate generation requires explicit shared source --root and --policy\"\n\n# ------------------------------------------------------------------- root --", 1)
				expected = strings.ReplaceAll(expected, "omarchy-setup's config/", "UpKeeper's shared/agent-instructions config/")
				expected = strings.ReplaceAll(expected, "of omarchy-setup's", "of UpKeeper's shared/agent-instructions")
				expected = strings.Replace(expected, "UpKeeper, agent99, shared", "UpKeeper, shared", 1)
				expected = strings.Replace(expected, "Usage: agents-instructions-gen [--root DIR] [--policy FILE] [--check]", "Usage: agents-instructions-gen --shared-root --root DIR --policy FILE [--check]", 1)
				expected = strings.Replace(expected, "Generate ~/.config/agents/ from an omarchy-setup checkout. The checkout is the only", "Derive candidate instructions in private scratch HOME from explicit shared source. The checkout is the only", 1)
				expected = strings.Replace(expected, "  --root DIR  The checkout to read. Default: $OMARCHY_SETUP_ROOT, else the checkout this\n              script is running from. A copy installed in ~/.local/bin is not in a\n              checkout, so it needs one of the two.", "  --shared-root  Required private shared-source transport.\n  --root DIR  Explicit shared source directory containing owner.json. Required.", 1)
			}
			target := filepath.Join("../../shared/agent-instructions", path)
			actual, e := os.ReadFile(target)
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(actual, []byte(expected)) {
				t.Fatal("undocumented upstream byte deviation")
			}
			mode := "100644"
			if strings.HasPrefix(path, "config/bin/") {
				mode = "100755"
			}
			tracked := gitIn(t, "../..", "ls-files", "--stage", "shared/agent-instructions/"+path)
			if !strings.HasPrefix(tracked, mode+" ") {
				t.Fatalf("Git mode %s expected %s", tracked, mode)
			}

		})
	}
}
