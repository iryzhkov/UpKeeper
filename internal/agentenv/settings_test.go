package agentenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedSettingsPreserveAccountsAndUnownedBytes(t *testing.T) {
	original := "# local account settings\nmodel = \"old\" # replace this\nmodel_provider = \"private\"\n" +
		"[features]\nold = true\n[features.nested]\nx = true\n" +
		"[model_providers.private]\nbase_url = \"https://private.invalid\"\nenv_key = \"PRIVATE_KEY\"\n" +
		"[projects.\"/srv/work\"]\ntrust_level = \"trusted\"\n"
	owned := map[string]any{"model": "gpt-6-astra", "features": map[string]any{"multi_agent": true}}
	got, err := ProjectRootKeys(original, owned)
	if err != nil {
		t.Fatal(err)
	}
	suffix := original[strings.Index(original, "[model_providers."):]
	if !strings.HasSuffix(got, suffix) || !strings.Contains(got, "model_provider = \"private\"\n") {
		t.Fatalf("unowned settings changed: %s", got)
	}
	again, err := ProjectRootKeys(got, owned)
	if err != nil || again != got {
		t.Fatalf("projection not idempotent: %v", err)
	}
	root, err := ParseTOML(got)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := root.Get("model")
	if model != "gpt-6-astra" {
		t.Fatal(model)
	}
	if _, ok := root.Child("features").Get("old"); ok {
		t.Fatal("stale owned feature survived")
	}
	got, err = ProjectRootKeys(got, map[string]any{"model": nil})
	if err != nil {
		t.Fatal(err)
	}
	root, _ = ParseTOML(got)
	if _, ok := root.Get("model"); ok {
		t.Fatal("default did not remove old model")
	}
}

func TestCaptureIncludesAllUserSkillRoots(t *testing.T) {
	home := fakeHome(t)
	for _, root := range []string{".codex/skills", ".agents/skills"} {
		writeHomeFile(t, home, root+"/custom/SKILL.md", "custom", 0644)
		writeHomeFile(t, home, root+"/custom/scripts/run", "#!/bin/sh\n", 0755)
	}
	writeHomeFile(t, home, ".codex/skills/.system/builtin/SKILL.md", "client-owned", 0644)
	component := capture(t, home)
	decoded, err := DecodeComponent(component)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]File{}
	for _, f := range decoded.Files {
		found[f.Path] = f
	}
	for _, root := range []string{".codex/skills", ".agents/skills"} {
		if found[root+"/custom/SKILL.md"].SHA256 != Digest([]byte("custom")) {
			t.Fatal("missing skill", root)
		}
		if found[root+"/custom/scripts/run"].Permissions != 0755 {
			t.Fatal("script lost executable mode")
		}
	}
	if _, ok := found[".codex/skills/.system/builtin/SKILL.md"]; ok {
		t.Fatal("captured client-owned system skills")
	}
}

func TestRootSettingsDottedKeysAndMalformedInput(t *testing.T) {
	got, err := ProjectRootKeys("features.multi_agent = false\n# retain\nother = 2\n", map[string]any{"features": map[string]any{"multi_agent": true}})
	if err != nil || !strings.Contains(got, "# retain\nother = 2\n") {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := ProjectRootKeys("model = \"one\"\nmodel = \"two\"\n", map[string]any{"model": "new"}); err == nil {
		t.Fatal("accepted ambiguous TOML")
	}
}

func TestOutsideHomeSkillRemainsPackageOwned(t *testing.T) {
	home := fakeHome(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("OS-owned"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex/skills"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".codex/skills/os")); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeComponent(capture(t, home))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range decoded.Files {
		if strings.Contains(f.Path, "skills/os/") {
			t.Fatal("captured OS package")
		}
	}
}
