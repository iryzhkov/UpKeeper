package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const agentConfigContent = "# Global agent instructions\n"

func agentConfigDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func agentConfigSpec(digest string) map[string]any {
	return map[string]any{
		"kind": "files",
		"files": []any{map[string]any{
			"path":   ".codex/AGENTS.md",
			"store":  "codex/AGENTS.md",
			"sha256": digest,
		}},
	}
}

func TestAgentConfigApply(t *testing.T) {
	cases := []struct {
		name string
		// present is the content the home directory already has, or "" for a
		// home directory without the file.
		present   string
		stored    string
		dryRun    bool
		state     string
		errorText string
		want      string
		backup    bool
	}{
		{
			name:    "a file that already matches its digest is converged",
			present: agentConfigContent,
			stored:  agentConfigContent,
			state:   "converged",
			want:    agentConfigContent,
		},
		{
			name:      "stored content that does not match its digest is refused",
			present:   "old\n",
			stored:    "tampered\n",
			errorText: "stored content for .codex/AGENTS.md does not match its digest",
			want:      "old\n",
		},
		{
			name:    "an existing file is backed up, replaced and verified",
			present: "old\n",
			stored:  agentConfigContent,
			state:   "updated",
			want:    agentConfigContent,
			backup:  true,
		},
		{
			name:   "an absent file is written without a backup",
			stored: agentConfigContent,
			state:  "updated",
			want:   agentConfigContent,
		},
		{
			name:    "a dry run reports the write without performing it",
			present: "old\n",
			stored:  agentConfigContent,
			dryRun:  true,
			state:   "updated",
			want:    "old\n",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &adapterTestRunner{}
			context := adapterTestContext(t, runner)
			context.DryRun = test.dryRun
			context.Stored = map[string]string{"codex/AGENTS.md": test.stored}
			target := context.Path(".codex/AGENTS.md")
			if test.present != "" {
				adapterTestWrite(t, target, test.present)
			}
			spec := agentConfigSpec(agentConfigDigest(agentConfigContent))
			before, err := agentConfigObserve(context, spec)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			actions := []string{}
			state, err := agentConfigApply(context, spec, before, &actions)
			if test.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorText) {
					t.Fatalf("want error containing %q, got %q and %v",
						test.errorText, state, err)
				}
			} else if err != nil || state != test.state {
				t.Fatalf("state = %q, error = %v, want %q", state, err, test.state)
			}
			content, readError := os.ReadFile(target)
			if test.want == "" {
				if readError == nil {
					t.Fatalf("file exists with %q, want none", content)
				}
			} else if readError != nil || string(content) != test.want {
				t.Fatalf("content = %q (%v), want %q", content, readError, test.want)
			}
			backup := target + ".upkeeper-" + context.RunID + ".bak"
			if _, err := os.Stat(backup); (err == nil) != test.backup {
				t.Fatalf("backup present = %v, want %v", err == nil, test.backup)
			}
			if test.backup && !adapterTestMatches(actions, "backed up .codex/AGENTS.md to") {
				t.Fatalf("actions %v do not report the backup", actions)
			}
			if leftover, _ := filepath.Glob(
				filepath.Join(filepath.Dir(target), "*.upkeeper-tmp")); len(leftover) != 0 {
				t.Fatalf("staging files were left behind: %v", leftover)
			}
		})
	}
}

func TestAgentConfigObserveReportsAnAbsentFileAsUnknown(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	spec := agentConfigSpec(agentConfigDigest(agentConfigContent))
	before, err := agentConfigObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if value, present := before[".codex/AGENTS.md"]; !present || value != nil {
		t.Fatalf("observation = %v, want a nil digest", before)
	}
}

func TestAgentConfigRefusesAReleaseThatCarriesNoStoredContent(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	spec := agentConfigSpec(agentConfigDigest(agentConfigContent))
	actions := []string{}
	_, err := agentConfigApply(context, spec, map[string]any{}, &actions)
	if err == nil || !strings.Contains(err.Error(), "no stored content for codex/AGENTS.md") {
		t.Fatalf("want a missing-content refusal, got %v", err)
	}
}
