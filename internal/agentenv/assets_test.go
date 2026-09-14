package agentenv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testProvenance is the instruction reference the Python test suite captures
// with, so the assets below carry the same provenance fields.
var testProvenance = ProjectInstructionRef{
	Repository:         "github.com/iryzhkov/dev-fleet",
	RepositoryRevision: strings.Repeat("a", 40),
	Path:               "README.md",
	SHA256:             strings.Repeat("b", 64),
}

// fakeHome builds the controller home the Python suite captures from, so that
// the digests below can be compared with the ones Python produced.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	files := map[string]string{
		".codex/AGENTS.md":                "# Global agent instructions\n",
		".claude/omarchy-setup/CLAUDE.md": "# Shared rules\n",
		".claude/CLAUDE.md": ClaudeBegin +
			"\n@~/.claude/omarchy-setup/CLAUDE.md\n" + ClaudeEnd,
		".claude/settings.json": `{"model":"test","permissions":{"allow":[]}}`,
		".claude.json": `{"mcpServers":{"huyang":{"command":"huyang"}},` +
			`"oauthAccount":{"token":"not-captured"}}`,
		".codex/config.toml":             "[mcp_servers.huyang]\ncommand = \"huyang\"\n",
		".config/opencode/opencode.json": `{"mcp":{"huyang":{"command":["huyang"]}},"instructions":[]}`,
		".config/agents/huyang.md":       "# Huyang\n",
		".claude/skills/sample/SKILL.md": "# Sample\n",
	}
	for path, text := range files {
		writeHomeFile(t, home, path, text, 0o644)
	}
	return home
}

func writeHomeFile(t *testing.T, home, path, text string, mode os.FileMode) {
	t.Helper()
	target := filepath.Join(home, path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("cannot create %s: %v", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, []byte(text), mode); err != nil {
		t.Fatalf("cannot write %s: %v", target, err)
	}
	// The process umask must not decide what a fixture's mode is.
	if err := os.Chmod(target, mode); err != nil {
		t.Fatalf("cannot set the mode of %s: %v", target, err)
	}
}

func capture(t *testing.T, home string) map[string]any {
	t.Helper()
	component, err := CaptureEnvironment(home, testProvenance)
	if err != nil {
		t.Fatalf("capture failed: %v", err)
	}
	return component
}

func readHomeFile(t *testing.T, home, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, path))
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return string(data)
}

func apply(
	t *testing.T, component map[string]any, home, runID string,
	dryRun bool, fault func(string) error,
) *ApplyResult {
	t.Helper()
	result, err := ApplyEnvironment(component, home, runID, dryRun, fault)
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	return result
}

// TestCaptureFreezesTheOwnedFiles pins every descriptor the capture produces.
// The digests are the ones the Python implementation produced from the same
// home, so a change to any encoding in this package shows up here.
func TestCaptureFreezesTheOwnedFiles(t *testing.T) {
	component := capture(t, fakeHome(t))
	decoded, err := DecodeComponent(component)
	if err != nil {
		t.Fatalf("the capture does not validate: %v", err)
	}
	wanted := []File{
		{Path: ".claude.json", Mode: "claude-mcp", Kind: "mcp-client", Permissions: 0o600,
			SizeBytes: 46,
			SHA256:    "a0fd61b7835fa58cccd5b3ed57489a587a0a344686c9266897f1272fa3b5fa66"},
		{Path: ".claude/CLAUDE.md", Mode: "claude-block", Kind: "global-instruction",
			Permissions: 0o600, SizeBytes: 36,
			SHA256: "e311929865f4759a1735ec53e4f4490abada358e7b8dcfdd496f49a28e556bdd"},
		{Path: ".claude/omarchy-setup/CLAUDE.md", Mode: "file", Kind: "global-instruction",
			Permissions: 0o644, SizeBytes: 15,
			SHA256: "ac561e7f7674cadd2fd6c668ff560fd2b0f7027b8486b312c58a1d6a64201ed0"},
		{Path: ".claude/settings.json", Mode: "claude-settings", Kind: "harness-setting",
			Permissions: 0o600, SizeBytes: 62,
			SHA256: "97e55f57f8289f6c7118d11eead578192e9eb88345a1c2be40765b6db03a5a73"},
		{Path: ".claude/skills/sample/SKILL.md", Mode: "file", Kind: "skill",
			Permissions: 0o644, SizeBytes: 9,
			SHA256: "504c93c0602db99e83b1f46edb699375a33b3018c0a4f8b7c707a3a470539b31"},
		{Path: ".codex/AGENTS.md", Mode: "file", Kind: "global-instruction",
			Permissions: 0o644, SizeBytes: 28,
			SHA256: "da633ae16a567a8f4a33f531ce4ea3a324a9b545463c90f4262b08ea3a9dfbfb"},
		{Path: ".codex/config.toml", Mode: "codex-mcp", Kind: "mcp-client",
			Permissions: 0o600, SizeBytes: 46,
			SHA256: "a0fd61b7835fa58cccd5b3ed57489a587a0a344686c9266897f1272fa3b5fa66"},
		{Path: ".config/agents/huyang.md", Mode: "file", Kind: "global-instruction",
			Permissions: 0o644, SizeBytes: 9,
			SHA256: "4c2a6a01c3cd31ce3527b5fbfe3b006cd0fd2a4bdd760924c833a81dd683dadd"},
		{Path: ".config/opencode/opencode.json", Mode: "opencode", Kind: "mcp-client",
			Permissions: 0o600, SizeBytes: 107,
			SHA256: "0cfd976d7c25a667e06ca6233ba495c990c39f9fa3a0d9e0776adbbf18bd03f1"},
	}
	if len(decoded.Files) != len(wanted) {
		t.Fatalf("captured %d files, want %d", len(decoded.Files), len(wanted))
	}
	for index, want := range wanted {
		got := decoded.Files[index]
		if got.Path != want.Path || got.Mode != want.Mode || got.Kind != want.Kind ||
			got.Permissions != want.Permissions || got.SizeBytes != want.SizeBytes ||
			got.SHA256 != want.SHA256 {
			t.Errorf("file %d =\n %+v\nwant\n %+v", index, redactContent(got), want)
		}
	}
	rendered := string(mustCanonical(t, component))
	if strings.Contains(rendered, "not-captured") {
		t.Error("the capture published a host-local account value")
	}
	// Sizes and modes must reach the manifest as whole numbers, because the
	// release digest is computed from these exact bytes.
	for _, literal := range []string{`"permissions": 384`, `"size_bytes": 46`} {
		if !strings.Contains(rendered, literal) {
			t.Errorf("the rendered component does not contain %s", literal)
		}
	}
	if len(decoded.Spec.SecretRefs) != 0 {
		t.Errorf("secret_refs = %v, want none", decoded.Spec.SecretRefs)
	}
	if decoded.Spec.HostProfile != "release-host" {
		t.Errorf("host_profile = %q, want release-host", decoded.Spec.HostProfile)
	}
}

func redactContent(file File) File {
	file.Content = nil
	return file
}

func mustCanonical(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := Canonical(value)
	if err != nil {
		t.Fatalf("cannot render the canonical document: %v", err)
	}
	return encoded
}

// TestCanonicalUsesTheIndentedEncoding pins the encoding of a stored document
// at the exact site the baseline identity is computed from.
func TestCanonicalUsesTheIndentedEncoding(t *testing.T) {
	priorAssets := []any{
		map[string]any{
			"asset_id": "asset-000000", "sha256": strings.Repeat("a", 64),
			"size_bytes": 3, "active_path": "/home/x/.claude.json",
		},
		map[string]any{
			"asset_id": "asset-000001", "sha256": strings.Repeat("b", 64),
			"size_bytes": 0, "active_path": "/home/x/.codex/AGENTS.md",
		},
	}
	wanted := "[\n  {\n    \"active_path\": \"/home/x/.claude.json\",\n" +
		"    \"asset_id\": \"asset-000000\",\n" +
		"    \"sha256\": \"" + strings.Repeat("a", 64) + "\",\n" +
		"    \"size_bytes\": 3\n  },\n  {\n" +
		"    \"active_path\": \"/home/x/.codex/AGENTS.md\",\n" +
		"    \"asset_id\": \"asset-000001\",\n" +
		"    \"sha256\": \"" + strings.Repeat("b", 64) + "\",\n" +
		"    \"size_bytes\": 0\n  }\n]\n"
	got := string(mustCanonical(t, priorAssets))
	if got != wanted {
		t.Errorf("canonical document =\n%q\nwant\n%q", got, wanted)
	}
	const wantedDigest = "ad4b0caa32c18d3b3262624e62cd7c468df505b349942991affb41e2e6d54a06"
	if Digest([]byte(got)) != wantedDigest {
		t.Errorf("digest = %s, want %s", Digest([]byte(got)), wantedDigest)
	}
}

// TestProjectionPreservesHostSettingsAndRollsBackTheWholeFileSet is the
// central contract: a projection merges into the host's files, and a failed
// activation restores all of them, not only the ones it reached.
func TestProjectionPreservesHostSettingsAndRollsBackTheWholeFileSet(t *testing.T) {
	home := fakeHome(t)
	component := capture(t, home)
	writeHomeFile(t, home, ".claude.json",
		`{"mcpServers":{"local":{"command":"local"}},"oauthAccount":{"token":"private"}}`, 0o644)
	writeHomeFile(t, home, ".claude/settings.json",
		`{"hooks":{"local":"keep"},"permissions":{"allow":["local"],"deny":["danger"]}}`, 0o644)
	writeHomeFile(t, home, ".codex/config.toml",
		"[projects.\"/private\"]\ntrust_level=\"trusted\"\n"+
			"[mcp_servers.local]\ncommand=\"local\"\n", 0o644)
	writeHomeFile(t, home, ".claude/CLAUDE.md",
		"Local rules\n\n"+ClaudeBegin+"\nstale\n"+ClaudeEnd+"\nTail\n", 0o644)

	decoded, err := DecodeComponent(component)
	if err != nil {
		t.Fatalf("the capture does not validate: %v", err)
	}
	before := map[string]string{}
	for _, file := range decoded.Files {
		before[file.Path] = readHomeFile(t, home, file.Path)
	}

	calls := 0
	failSecond := func(string) error {
		calls++
		if calls == 2 {
			return errors.New("injected activation failure")
		}
		return nil
	}
	result := apply(t, component, home, "rollback-test", false, failSecond)
	if result.State != "rolled-back" {
		t.Fatalf("state = %q, want rolled-back", result.State)
	}
	for path, content := range before {
		if got := readHomeFile(t, home, path); got != content {
			t.Errorf("%s was not restored:\n got %q\nwant %q", path, got, content)
		}
	}

	result = apply(t, component, home, "success-test", false, nil)
	if result.State != "updated" {
		t.Fatalf("state = %q, want updated", result.State)
	}
	wantedChanged := []string{
		".claude.json", ".claude/CLAUDE.md", ".claude/settings.json",
		".codex/config.toml", ".config/opencode/opencode.json",
	}
	if !equalStrings(result.Changed, wantedChanged) {
		t.Errorf("changed = %v, want %v", result.Changed, wantedChanged)
	}

	// Every one of these is a byte-for-byte golden from the Python implementation.
	wantedContent := map[string]string{
		".claude.json": "{\n  \"mcpServers\": {\n    \"huyang\": {\n" +
			"      \"command\": \"huyang\"\n    },\n    \"local\": {\n" +
			"      \"command\": \"local\"\n    }\n  },\n  \"oauthAccount\": {\n" +
			"    \"token\": \"private\"\n  }\n}\n",
		".claude/settings.json": "{\n  \"hooks\": {\n    \"local\": \"keep\"\n  },\n" +
			"  \"model\": \"test\",\n  \"permissions\": {\n    \"allow\": [\n" +
			"      \"local\"\n    ],\n    \"deny\": [\n      \"danger\"\n    ]\n  }\n}\n",
		".codex/config.toml": "[projects.\"/private\"]\ntrust_level=\"trusted\"\n" +
			"[mcp_servers.local]\ncommand=\"local\"\n\n" +
			"[mcp_servers.\"huyang\"]\n\"command\" = \"huyang\"\n",
		".claude/CLAUDE.md": "Local rules\n\n" + ClaudeBegin +
			"\n@~/.claude/omarchy-setup/CLAUDE.md\n" + ClaudeEnd + "\nTail\n",
	}
	for path, want := range wantedContent {
		if got := readHomeFile(t, home, path); got != want {
			t.Errorf("%s =\n%q\nwant\n%q", path, got, want)
		}
	}

	if result := apply(t, component, home, "noop", false, nil); result.State != "converged" {
		t.Errorf("a second projection reported %q, want converged", result.State)
	}
	if _, err := os.Stat(
		filepath.Join(home, environmentsRoot, "noop")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a converged projection opened a transaction directory")
	}
}

// TestHarnessReformattingIsNotOwnedDrift keeps a convergence from fighting the
// harness over bytes nobody in the release owns.
func TestHarnessReformattingIsNotOwnedDrift(t *testing.T) {
	home := fakeHome(t)
	component := capture(t, home)
	apply(t, component, home, "initial", false, nil)
	reformatted := `{"localCache":{"new":"value"},"mcpServers":{"huyang":{"command":"huyang"}}}`
	writeHomeFile(t, home, ".claude.json", reformatted, 0o600)
	result := apply(t, component, home, "no-owned-drift", false, nil)
	if result.State != "converged" {
		t.Errorf("state = %q, want converged; changed %v", result.State, result.Changed)
	}
	if got := readHomeFile(t, home, ".claude.json"); got != reformatted {
		t.Errorf(".claude.json was rewritten:\n got %q\nwant %q", got, reformatted)
	}
}

// TestTargetLocalCredentialsSurviveOwnedRegistrationUpdates proves the one
// thing a worker cannot recover from on its own: losing its credentials.
func TestTargetLocalCredentialsSurviveOwnedRegistrationUpdates(t *testing.T) {
	home := fakeHome(t)
	component := capture(t, home)
	writeHomeFile(t, home, ".claude.json",
		`{"mcpServers":{"huyang":{"command":"old","env":{"API_TOKEN":"private"}}}}`, 0o644)
	writeHomeFile(t, home, ".codex/config.toml",
		"[mcp_servers.huyang]\ncommand=\"old\"\n"+
			"[mcp_servers.huyang.env]\nAPI_TOKEN=\"private\"\n", 0o644)
	writeHomeFile(t, home, ".config/opencode/opencode.json",
		`{"mcp":{"huyang":{"command":["old"],"environment":{"API_TOKEN":"private"}}}}`, 0o644)
	if result := apply(t, component, home, "preserve-mcp", false, nil); result.State != "updated" {
		t.Fatalf("state = %q, want updated", result.State)
	}
	wanted := map[string]string{
		".claude.json": "{\n  \"mcpServers\": {\n    \"huyang\": {\n" +
			"      \"command\": \"huyang\",\n      \"env\": {\n" +
			"        \"API_TOKEN\": \"private\"\n      }\n    }\n  }\n}\n",
		".codex/config.toml": "\n\n[mcp_servers.\"huyang\"]\n\"command\" = \"huyang\"\n" +
			"\"env\" = { \"API_TOKEN\" = \"private\" }\n",
		".config/opencode/opencode.json": "{\n  \"instructions\": [],\n  \"mcp\": {\n" +
			"    \"huyang\": {\n      \"command\": [\n        \"huyang\"\n      ],\n" +
			"      \"environment\": {\n        \"API_TOKEN\": \"private\"\n" +
			"      }\n    }\n  }\n}\n",
	}
	for path, want := range wanted {
		if got := readHomeFile(t, home, path); got != want {
			t.Errorf("%s =\n%q\nwant\n%q", path, got, want)
		}
	}
}

// TestUnrelatedTomlTablesAndCommentsSurvive states the span editor's promise
// directly: only the owned tables are rewritten.
func TestUnrelatedTomlTablesAndCommentsSurvive(t *testing.T) {
	home := fakeHome(t)
	component := capture(t, home)
	preserved := "# the operator's own comment\nmodel = \"gpt-5\"\n\n" +
		"[projects.\"/srv/work\"]\ntrust_level = \"trusted\"   # trailing comment\n\n" +
		"[mcp_servers.other]\ncommand = \"other\"\nargs = [\"--flag\", 3]\n"
	writeHomeFile(t, home, ".codex/config.toml", preserved, 0o644)
	apply(t, component, home, "preserve-toml", false, nil)
	got := readHomeFile(t, home, ".codex/config.toml")
	if !strings.HasPrefix(got, preserved) {
		t.Errorf("the unrelated bytes were rewritten:\n got %q\nwant the prefix %q", got, preserved)
	}
	if !strings.HasSuffix(got, "\n[mcp_servers.\"huyang\"]\n\"command\" = \"huyang\"\n") {
		t.Errorf("the owned registration was not appended: %q", got)
	}
	root, err := ParseTOML(got)
	if err != nil {
		t.Fatalf("the projected file does not parse: %v", err)
	}
	servers := root.Child("mcp_servers")
	if servers.Child("other") == nil || servers.Child("huyang") == nil {
		t.Errorf("registrations = %v, want both other and huyang", servers.Keys())
	}
	if root.Child("projects").Child("/srv/work") == nil {
		t.Error("the unrelated projects table did not survive")
	}
}

// TestTheTomlEditorFailsClosed records exactly what the narrow reader refuses,
// because refusing is what keeps a file it cannot model from being rewritten.
func TestTheTomlEditorFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"an array of tables", "[[products]]\nname = \"hammer\"\n"},
		{"a multi-line string", "key = \"\"\"\nline\n\"\"\"\n"},
		{"a date value", "when = 1979-05-27T07:32:00Z\n"},
		{"a duplicate table", "[a]\nx = 1\n[a]\ny = 2\n"},
		{"an unterminated string", "key = \"open\n"},
		{"an unterminated table", "[a\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ParseTOML(testCase.text); err == nil {
				t.Error("the editor accepted syntax it does not model")
			}
		})
	}
	root, err := ParseTOML(
		"# comment\n[a.b]\nx = 1\ny = [1, 2]\nz = { k = \"v\" }\nw = true\nv = 1.5\n")
	if err != nil {
		t.Fatalf("the supported subset was refused: %v", err)
	}
	if root.Child("a").Child("b").Len() != 5 {
		t.Errorf("parsed %v, want five keys", root.Child("a").Child("b").Keys())
	}
}

// TestDryRunChangesNothingAndRebasesOntoAnotherHome proves both that a dry run
// is inert and that a capture from one home projects onto another.
func TestDryRunChangesNothingAndRebasesOntoAnotherHome(t *testing.T) {
	component := capture(t, fakeHome(t))
	home := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("cannot create the target home: %v", err)
	}
	if result := apply(t, component, home, "dry", true, nil); result.State != "updated" {
		t.Fatalf("state = %q, want updated", result.State)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("cannot list the target home: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a dry run wrote %d entries into the target home", len(entries))
	}
	apply(t, component, home, "install", false, nil)
	observation, err := ObserveEnvironment(component, home, "pending")
	if err != nil {
		t.Fatalf("observation failed: %v", err)
	}
	for _, asset := range observation.Spec.Assets {
		if !strings.HasPrefix(asset.ActivePath, home+"/") {
			t.Errorf("active path %q was not rebased onto %q", asset.ActivePath, home)
		}
	}
	for _, drift := range observation.Drift {
		if drift.State != DriftMatch {
			t.Errorf("drift of %s = %q, want MATCH", drift.Subject, drift.State)
		}
	}
}

func TestCorruptPayloadAndEscapingSymlinkAreRefused(t *testing.T) {
	home := fakeHome(t)
	component := capture(t, home)
	corrupt := capture(t, home)
	files := corrupt["files"].([]any)
	files[0].(map[string]any)["content"] = "Y29ycnVwdA=="
	if err := ValidateEnvironment(corrupt); err == nil ||
		!strings.Contains(err.Error(), "digest") {
		t.Errorf("a corrupt payload was accepted: %v", err)
	}
	target := filepath.Join(home, ".codex/AGENTS.md")
	if err := os.Remove(target); err != nil {
		t.Fatalf("cannot remove the target: %v", err)
	}
	outside := filepath.Join(filepath.Dir(home), "outside")
	if err := os.Symlink(outside, target); err != nil {
		t.Fatalf("cannot create the symlink: %v", err)
	}
	_, err := ObserveEnvironment(component, home, "pending")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("a symlink out of home was followed: %v", err)
	}
}

func TestOperatingSystemOwnedSkillSymlinksArePreserved(t *testing.T) {
	home := fakeHome(t)
	systemSkill := filepath.Join(filepath.Dir(home), "system-skill")
	if err := os.MkdirAll(systemSkill, 0o755); err != nil {
		t.Fatalf("cannot create the system skill: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(systemSkill, "SKILL.md"), []byte("# OS-owned skill"), 0o644); err != nil {
		t.Fatalf("cannot write the system skill: %v", err)
	}
	link := filepath.Join(home, ".claude/skills/os-owned")
	if err := os.Symlink(systemSkill, link); err != nil {
		t.Fatalf("cannot link the system skill: %v", err)
	}
	component := capture(t, home)
	decoded, err := DecodeComponent(component)
	if err != nil {
		t.Fatalf("the capture does not validate: %v", err)
	}
	for _, file := range decoded.Files {
		if strings.Contains(file.Path, "os-owned") {
			t.Errorf("the capture claimed the package-owned skill %s", file.Path)
		}
	}
	apply(t, component, home, "preserve-os", false, nil)
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the package-owned skill link was replaced: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(systemSkill, "SKILL.md"))
	if err != nil || string(data) != "# OS-owned skill" {
		t.Errorf("the package-owned skill bytes changed: %q, %v", data, err)
	}
}

// TestCaptureFollowsSkillLinksThatStayInsideHome captures the bytes behind a
// published skill link under the name the harness sees, and skips the
// directories no release should ever carry.
func TestCaptureFollowsSkillLinksThatStayInsideHome(t *testing.T) {
	home := fakeHome(t)
	published := filepath.Join(home, ".claude/omarchy-setup/published")
	writeHomeFile(t, home, ".claude/omarchy-setup/published/SKILL.md", "# Linked\n", 0o644)
	writeHomeFile(
		t, home, ".claude/omarchy-setup/published/nested/extra.md", "# Extra\n", 0o644)
	writeHomeFile(t, home, ".claude/omarchy-setup/published/.git/config", "ignored", 0o644)
	writeHomeFile(
		t, home, ".claude/omarchy-setup/published/__pycache__/x.pyc", "ignored", 0o644)
	if err := os.Symlink(published, filepath.Join(home, ".claude/skills/linked")); err != nil {
		t.Fatalf("cannot link the published skill: %v", err)
	}
	decoded, err := DecodeComponent(capture(t, home))
	if err != nil {
		t.Fatalf("the capture does not validate: %v", err)
	}
	var skills []string
	for _, file := range decoded.Files {
		if file.Kind == "skill" {
			skills = append(skills, file.Path)
		}
	}
	wanted := []string{
		".claude/skills/linked/SKILL.md",
		".claude/skills/linked/nested/extra.md",
		".claude/skills/sample/SKILL.md",
	}
	if !equalStrings(skills, wanted) {
		t.Errorf("captured skills = %v, want %v", skills, wanted)
	}
}

func TestCaptureRejectsCredentialsInOwnedRegistrations(t *testing.T) {
	home := fakeHome(t)
	writeHomeFile(t, home, ".claude.json",
		`{"mcpServers":{"bad":{"command":"client","env":{"API_TOKEN":"private"}}}}`, 0o644)
	_, err := CaptureEnvironment(home, testProvenance)
	if err == nil || !strings.Contains(err.Error(), "environment key") {
		t.Errorf("a credential-bearing registration was captured: %v", err)
	}
}

// TestNoBlindOverwriteWhenAFileChangesDuringActivation refuses to reclaim a
// path somebody edited while the transaction was running.
func TestNoBlindOverwriteWhenAFileChangesDuringActivation(t *testing.T) {
	home := fakeHome(t)
	component := capture(t, home)
	writeHomeFile(t, home, ".claude/CLAUDE.md", "old", 0o644)
	var changedPaths []string
	concurrentEdit := func(changed string) error {
		changedPaths = append(changedPaths, changed)
		writeHomeFile(t, home, changed, "concurrent", 0o644)
		return errors.New("failure")
	}
	_, err := ApplyEnvironment(component, home, "conflict", false, concurrentEdit)
	if err == nil || !strings.Contains(err.Error(), "unexplained") {
		t.Fatalf("the rollback claimed success over a concurrent change: %v", err)
	}
	if len(changedPaths) == 0 {
		t.Fatal("no path was activated")
	}
	if got := readHomeFile(t, home, changedPaths[0]); got != "concurrent" {
		t.Errorf("%s = %q, want the concurrent change preserved", changedPaths[0], got)
	}
}

func TestAtomicWriteRoundTripAndMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "nested", "record.json")
	payload := []byte("{\"a\":1}\n")
	if err := AtomicWrite(path, payload, 0o600); err != nil {
		t.Fatalf("the first write failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("read back %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("cannot stat the record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	if err := AtomicWrite(path, []byte("replaced"), 0o644); err != nil {
		t.Fatalf("the replacing write failed: %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("cannot stat the record: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 644", info.Mode().Perm())
	}
	if data, err = os.ReadFile(path); err != nil || string(data) != "replaced" {
		t.Errorf("read back %q, %v", data, err)
	}
	if _, err := os.Stat(path + temporarySuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("the temporary file outlived the write")
	}
	const replacedDigest = "6c1aa50442a93e42c0eb2907cf4e017cd19547891fa190f3ea473582b0479290"
	if Digest([]byte("replaced")) != replacedDigest {
		t.Errorf("digest = %q, want %s", Digest([]byte("replaced")), replacedDigest)
	}
}

// TestSafePathRefusesWhatIsNotOurs keeps a projection inside the directories
// the fleet manages.
func TestSafePathRefusesWhatIsNotOurs(t *testing.T) {
	accepted := []string{".claude.json", ".config/agents/a.md", ".codex/config.toml"}
	for _, path := range accepted {
		if _, err := SafePath(path); err != nil {
			t.Errorf("SafePath(%q) = %v, want it accepted", path, err)
		}
	}
	refused := []string{"", "/etc/passwd", ".ssh/id_ed25519", ".config/../../escape", "a\x00b"}
	for _, path := range refused {
		if _, err := SafePath(path); err == nil {
			t.Errorf("SafePath(%q) was accepted", path)
		}
	}
}
