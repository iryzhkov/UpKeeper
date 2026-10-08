// Owned file projections and environment transactions for a release pull.
//
// Only this file knows how a shared harness file is projected onto a target.
// Secret values are never captured: a worker credential stays target-local and
// the release carries an opaque reference to it.

package agentenv

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// The markers around the block of ~/.claude/CLAUDE.md the fleet owns. Every
// instruction outside them belongs to the person at the keyboard.
const (
	ClaudeBegin = "<!-- >>> omarchy-setup:claude >>> -->"
	ClaudeEnd   = "<!-- <<< omarchy-setup:claude <<< -->"
)

// environmentsRoot is where an environment transaction journals the identity
// and the bytes it is about to replace.
const environmentsRoot = ".local/state/dev-fleet/environments"

// temporarySuffix names the file an atomic write publishes from.
const temporarySuffix = ".upkeeper-tmp"

var secretPattern = regexp.MustCompile(
	`(?i)(password|token|credential|api.?key|authorization|secret)`)

var credentialValuePattern = regexp.MustCompile(
	`(?:https?://[^/\s]+@|[?&](?:key|token)=|(?:Bearer|sk-)\S+)`)

// safeEnvironmentKeys are the only environment variables a captured MCP
// registration may set. Anything else has to become a secret reference.
var safeEnvironmentKeys = map[string]bool{
	"OV_AGENT": true, "OV_BASE_URL": true,
	"LOCAL_LLM_MODEL": true, "LOCAL_LLM_NUM_CTX": true, "LOCAL_LLM_URL": true,
}

// managedRoots are the home directory entries a managed path may start with.
var managedRoots = map[string]bool{
	".config": true, ".local": true, ".claude": true, ".codex": true, ".agents": true,
}

// settingsKeys are the harness settings the fleet owns. Every other key in the
// file belongs to the host.
var settingsKeys = []string{"model", "theme", "tui", "syntaxHighlightingDisabled", "permissions"}

// projectionModes are the ways a captured payload can be projected onto a
// target: whole-file, or merged into a structured document the harness shares.
var projectionModes = map[string]bool{
	"file": true, "claude-block": true, "claude-settings": true,
	"claude-mcp": true, "codex-mcp": true, "codex-settings": true, "opencode": true,
}

// structuredModes are the projection modes whose payload is a JSON document.
var structuredModes = map[string]bool{
	"claude-settings": true, "claude-mcp": true, "codex-mcp": true, "codex-settings": true, "opencode": true,
}

// modeByPath pins each shared document to its one allowed projection, so that a
// captured payload can never be projected through a different merge.
var modeByPath = map[string]string{
	".claude.json":                   "claude-mcp",
	".codex/config.toml":             "codex-mcp",
	".claude/settings.json":          "claude-settings",
	".config/opencode/opencode.json": "opencode",
	".claude/CLAUDE.md":              "claude-block",
}

// allowedPermissions are the only file modes a managed projection may carry.
var allowedPermissions = map[int64]bool{0o600: true, 0o644: true, 0o700: true, 0o755: true}

// componentFields is the exact field set of a current agent-environment component.
// legacyComponentFields keeps already-published releases readable.
var componentFields = []string{"kind", "files", "retired_files", "spec", "source_home"}
var legacyComponentFields = []string{"kind", "files", "spec", "source_home"}

// fileFields is the exact field set of one owned file descriptor.
var fileFields = []string{
	"path", "mode", "kind", "content", "sha256", "size_bytes", "permissions",
}

// Digest is the SHA-256 of a payload, in the lowercase hexadecimal form every
// identity in this package is written in.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Canonical renders a value as the indented, key-sorted JSON document with a
// trailing newline that UpKeeper stores on disk.
//
// This is not the encoding an environment revision is computed from. Spec.
// CanonicalBytes is compact on purpose; these two encodings must not be mixed.
func Canonical(value any) ([]byte, error) {
	return canonjson.MarshalFile(value)
}

// AtomicWrite replaces a file with new content, creating its directory when
// needed and giving the file its mode before it holds anything.
//
// The temporary file is created exclusively so that a convergence can never be
// tricked into following a temporary symlink an attacker left behind.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + temporarySuffix
	handle, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary) }()
	if err := handle.Chmod(mode); err != nil {
		_ = handle.Close()
		return err
	}
	if _, err := handle.Write(data); err != nil {
		_ = handle.Close()
		return err
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return err
	}
	if err := handle.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// SafePath accepts only a home-relative path inside a directory the fleet
// manages. The Claude harness keeps one document directly in the home
// directory, which is the single explicit exception.
func SafePath(value string) (string, error) {
	if value == ".claude.json" {
		return value, nil
	}
	parts := pathParts(value)
	if strings.HasPrefix(value, "/") || len(parts) == 0 || !managedRoots[parts[0]] ||
		contains(parts, "..") || hasControlCharacter(value) {
		return "", errors.New("unsafe managed home path")
	}
	return value, nil
}

// TargetPath resolves a managed path and refuses one whose symlinks leave the
// home directory. A convergence writes only inside the home it was given.
func TargetPath(home, relative string) (string, error) {
	safe, err := SafePath(relative)
	if err != nil {
		return "", err
	}
	resolved, err := resolvePath(filepath.Join(home, safe))
	if err != nil {
		return "", err
	}
	resolvedHome, err := resolvePath(home)
	if err != nil {
		return "", err
	}
	if !isWithin(resolved, resolvedHome) {
		return "", errors.New("managed target symlink escapes home")
	}
	return resolved, nil
}

// RetirementPath returns the lexical managed path only when no existing path
// component is a symlink. Retirement must unlink the owned file itself, never a
// same-byte target reached through a link.
func RetirementPath(home, relative string) (string, error) {
	safe, err := SafePath(relative)
	if err != nil {
		return "", err
	}
	target := filepath.Join(home, safe)
	current := home
	for _, part := range pathParts(safe) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return target, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("retired managed path traverses a symlink")
		}
	}
	return target, nil
}

// CheckPublic refuses a credential-bearing registration field instead of
// redacting it.
//
// A release is a durable public record: a secret that reaches it has to be
// rotated, so the capture fails rather than publishing one.
func CheckPublic(value any, key string) error {
	if secretPattern.MatchString(key) {
		return errors.New("credential-bearing configuration must use a secret reference")
	}
	switch typed := value.(type) {
	case *Table:
		return CheckPublic(typed.Generic(), key)
	case map[string]any:
		for _, name := range sortedKeysOf(typed) {
			if name == "env" || name == "environment" {
				if !onlySafeEnvironment(typed[name]) {
					return errors.New(
						"unapproved MCP environment key; use a target-local secret reference")
				}
			}
			if err := CheckPublic(typed[name], name); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			text, isText := item.(string)
			if isText && strings.HasPrefix(text, "-") && secretPattern.MatchString(text) {
				return errors.New("credential-bearing command arguments are forbidden")
			}
			if err := CheckPublic(item, ""); err != nil {
				return err
			}
		}
	case string:
		if credentialValuePattern.MatchString(typed) {
			return errors.New("credential-bearing registration value")
		}
	}
	return nil
}

func onlySafeEnvironment(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		if table, isTable := value.(*Table); isTable {
			object = table.Generic()
		} else {
			return false
		}
	}
	for name := range object {
		if !safeEnvironmentKeys[name] {
			return false
		}
	}
	return true
}

// File is one owned projection: the bytes the release carries, where they go,
// and which merge puts them there.
type File struct {
	Path        string
	Mode        string
	Kind        string
	Content     []byte
	SHA256      string
	SizeBytes   int64
	Permissions int64
}

// ToMap renders the descriptor as the generic document a release carries.
func (f File) ToMap() map[string]any {
	return map[string]any{
		"path":        f.Path,
		"mode":        f.Mode,
		"kind":        f.Kind,
		"content":     base64.StdEncoding.EncodeToString(f.Content),
		"sha256":      f.SHA256,
		"size_bytes":  f.SizeBytes,
		"permissions": f.Permissions,
	}
}

// Component is a validated agent-environment component.
type Component struct {
	Files        []File
	RetiredFiles []File
	Spec         *Spec
	SourceHome   string
}

// ToMap renders the component as the generic document a release carries.
func (c *Component) ToMap() (map[string]any, error) {
	spec, err := c.Spec.ToMap()
	if err != nil {
		return nil, err
	}
	files := make([]any, 0, len(c.Files))
	for _, file := range c.Files {
		files = append(files, file.ToMap())
	}
	retired := make([]any, 0, len(c.RetiredFiles))
	for _, file := range c.RetiredFiles {
		retired = append(retired, file.ToMap())
	}
	return map[string]any{
		"kind":          "agent-environment",
		"files":         files,
		"retired_files": retired,
		"spec":          spec,
		"source_home":   c.SourceHome,
	}, nil
}

// ValidateEnvironment refuses an agent-environment component that does not
// describe exactly what it carries.
func ValidateEnvironment(component map[string]any) error {
	_, err := DecodeComponent(component)
	return err
}

// DecodeComponent validates a component and returns it in structured form.
//
// Every payload is checked against its own digest and against the frozen spec,
// so a component that decodes here cannot misdescribe what it will write.
func DecodeComponent(component map[string]any) (*Component, error) {
	if !sameKeys(component, componentFields) && !sameKeys(component, legacyComponentFields) {
		return nil, errors.New("unknown agent-environment fields")
	}
	rawSpec, ok := component["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("the agent-environment spec must be an object")
	}
	spec, err := SpecFromMap(rawSpec)
	if err != nil {
		return nil, err
	}
	kind, _ := component["kind"].(string)
	sourceHome, _ := component["source_home"].(string)
	if kind != "agent-environment" || !filepath.IsAbs(sourceHome) {
		return nil, errors.New("invalid agent-environment identity")
	}
	rawFiles, err := anyList(component["files"])
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, len(rawFiles))
	paths := make([]string, 0, len(rawFiles))
	for _, raw := range rawFiles {
		object, isObject := raw.(map[string]any)
		if !isObject {
			return nil, errors.New("invalid owned file descriptor")
		}
		path, pathIsText := object["path"].(string)
		if !pathIsText {
			return nil, errors.New("invalid owned file descriptor")
		}
		safe, err := SafePath(path)
		if err != nil {
			return nil, err
		}
		paths = append(paths, safe)
	}
	if len(paths) == 0 || !isSortedUnique(paths) || len(rawFiles) != len(spec.Assets) {
		return nil, errors.New(
			"environment files must be unique, sorted and cover the F05 spec")
	}
	for index, raw := range rawFiles {
		file, err := fileFromMap(raw.(map[string]any))
		if err != nil {
			return nil, err
		}
		if err := file.matchesAsset(spec.Assets[index], sourceHome); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	retired := []File{}
	retiredPaths := []string{}
	if rawRetired, present := component["retired_files"]; present {
		items, err := anyList(rawRetired)
		if err != nil {
			return nil, errors.New("invalid retired file descriptor")
		}
		for _, raw := range items {
			object, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("invalid retired file descriptor")
			}
			file, err := fileFromMap(object)
			if err != nil || file.Mode != "file" {
				return nil, errors.New("invalid retired file descriptor")
			}
			safe, err := SafePath(file.Path)
			if err != nil {
				return nil, err
			}
			retiredPaths = append(retiredPaths, safe)
			retired = append(retired, file)
		}
	}
	if !isSortedUnique(retiredPaths) {
		return nil, errors.New("retired environment files must be unique, sorted and inactive")
	}
	active := map[string]bool{}
	for _, path := range paths {
		active[path] = true
	}
	for _, path := range retiredPaths {
		if active[path] {
			return nil, errors.New("retired environment files must be unique, sorted and inactive")
		}
	}
	return &Component{
		Files: files, RetiredFiles: retired, Spec: spec, SourceHome: sourceHome,
	}, nil
}

func fileFromMap(raw map[string]any) (File, error) {
	var file File
	descriptorError := errors.New("invalid owned file descriptor")
	if !sameKeys(raw, fileFields) {
		return file, descriptorError
	}
	var err error
	if file.Path, err = fieldString(raw, "path"); err != nil {
		return file, descriptorError
	}
	if file.Mode, err = fieldString(raw, "mode"); err != nil {
		return file, descriptorError
	}
	if file.Kind, err = fieldString(raw, "kind"); err != nil {
		return file, descriptorError
	}
	if !projectionModes[file.Mode] {
		return file, descriptorError
	}
	if file.Permissions, err = fieldInt64(raw, "permissions"); err != nil {
		return file, descriptorError
	}
	if !allowedPermissions[file.Permissions] {
		return file, errors.New("unsupported managed file permissions")
	}
	encoded, err := fieldString(raw, "content")
	if err != nil {
		return file, descriptorError
	}
	if file.Content, err = base64.StdEncoding.Strict().DecodeString(encoded); err != nil {
		return file, fmt.Errorf("environment payload is not valid base64: %w", err)
	}
	if file.SHA256, err = fieldString(raw, "sha256"); err != nil {
		return file, descriptorError
	}
	if file.SizeBytes, err = fieldInt64(raw, "size_bytes"); err != nil {
		return file, descriptorError
	}
	if Digest(file.Content) != file.SHA256 || int64(len(file.Content)) != file.SizeBytes {
		return file, errors.New("environment payload digest mismatch")
	}
	return file, nil
}

// matchesAsset refuses a descriptor that disagrees with the frozen asset it is
// supposed to be, or that would be projected through the wrong merge.
func (f File) matchesAsset(asset ManagedAsset, sourceHome string) error {
	if asset.SHA256 != f.SHA256 || asset.SizeBytes != f.SizeBytes || asset.Kind != f.Kind ||
		asset.ActivePath != filepath.Join(sourceHome, f.Path) {
		return errors.New("environment file does not match F05 asset")
	}
	if structuredModes[f.Mode] {
		decoded, err := canonjson.Decode(f.Content)
		if err != nil {
			return err
		}
		if err := CheckPublic(decoded, ""); err != nil {
			return err
		}
		if f.Mode == "codex-settings" {
			if err := validateCodexSettings(objectOf(decoded)); err != nil {
				return err
			}
		}
	}
	expected, pinned := modeByPath[f.Path]
	if !pinned {
		expected = "file"
	}
	if f.Path == ".codex/config.toml" && f.Mode == "codex-settings" {
		expected = f.Mode
	}
	if f.Mode != expected {
		return errors.New("owned projection does not match its allowed destination")
	}
	return nil
}

// CaptureEnvironment reads the controller's own agent environment and freezes
// it as a publishable component. It is retained for tests and callers without a
// repository-owned skill source.
func CaptureEnvironment(home string, provenance ProjectInstructionRef) (map[string]any, error) {
	return CaptureEnvironmentWithSkills(home, "", provenance)
}

// CaptureEnvironmentWithSkills makes UpKeeper the single publisher of
// repository-owned skills. Installed home copies are outputs, not capture
// inputs: the pinned source tree replaces same-named installed skills and is
// projected to Claude/OpenCode and Codex discovery roots.
func CaptureEnvironmentWithSkills(
	home, skillsRoot string, provenance ProjectInstructionRef,
) (map[string]any, error) {
	return CaptureEnvironmentWithSkillsAndPrevious(home, skillsRoot, provenance, nil)
}

// CaptureEnvironmentWithSkillsAndPrevious carries exact-byte retirement
// tombstones forward so a host that skips one release still removes files the
// reviewed source stopped publishing.
func CaptureEnvironmentWithSkillsAndPrevious(
	home, skillsRoot string, provenance ProjectInstructionRef, previous map[string]any,
) (map[string]any, error) {
	files, err := captureFiles(home, skillsRoot)
	if err != nil {
		return nil, err
	}
	var decodedPrevious *Component
	if previous != nil {
		decodedPrevious, err = DecodeComponent(previous)
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(files, func(left, right int) bool {
		return files[left].Path < files[right].Path
	})
	desired := map[string][]byte{}
	active := map[string]bool{}
	for _, file := range files {
		desired[file.Path] = file.Content
		active[file.Path] = true
	}
	retiredByPath := map[string]File{}
	if decodedPrevious != nil {
		for _, file := range decodedPrevious.RetiredFiles {
			retiredByPath[file.Path] = file
		}
		for _, file := range decodedPrevious.Files {
			if file.Mode == "file" && !active[file.Path] {
				retiredByPath[file.Path] = file
			}
		}
	}
	for path := range active {
		delete(retiredByPath, path)
	}
	retiredPaths := make([]string, 0, len(retiredByPath))
	for path := range retiredByPath {
		retiredPaths = append(retiredPaths, path)
	}
	sort.Strings(retiredPaths)
	retired := make([]File, 0, len(retiredPaths))
	for _, path := range retiredPaths {
		retired = append(retired, retiredByPath[path])
	}
	spec, err := makeSpec(home, desired, desired, files, provenance, "capture")
	if err != nil {
		return nil, err
	}
	component := &Component{
		Files: files, RetiredFiles: retired, Spec: spec, SourceHome: home,
	}
	rendered, err := component.ToMap()
	if err != nil {
		return nil, err
	}
	if err := ValidateEnvironment(rendered); err != nil {
		return nil, err
	}
	return rendered, nil
}

func captureFiles(home, skillsRoot string) ([]File, error) {
	var files []File
	add := func(relative, mode, kind string, value []byte) error {
		safe, err := SafePath(relative)
		if err != nil {
			return err
		}
		source := filepath.Join(home, safe)
		data := value
		if data == nil {
			if data, err = os.ReadFile(source); err != nil {
				return err
			}
		}
		permissions := int64(0o600)
		if mode == "file" {
			info, err := os.Stat(source)
			if err != nil {
				return err
			}
			permissions = int64(info.Mode().Perm())
		}
		files = append(files, File{
			Path: safe, Mode: mode, Kind: kind, Content: data, SHA256: Digest(data),
			SizeBytes: int64(len(data)), Permissions: permissions,
		})
		return nil
	}
	if err := add(".codex/AGENTS.md", "file", "global-instruction", nil); err != nil {
		return nil, err
	}
	if err := add(".claude/omarchy-setup/CLAUDE.md", "file", "global-instruction", nil); err != nil {
		return nil, err
	}
	block, err := captureClaudeBlock(home)
	if err != nil {
		return nil, err
	}
	if err := add(".claude/CLAUDE.md", "claude-block", "global-instruction", block); err != nil {
		return nil, err
	}
	for _, relative := range agentInstructionFiles(home) {
		if err := add(relative, "file", "global-instruction", nil); err != nil {
			return nil, err
		}
	}
	skills, err := captureSkillFiles(home)
	if err != nil {
		return nil, err
	}
	for _, relative := range skills {
		if err := add(relative, "file", "skill", nil); err != nil {
			return nil, err
		}
	}
	if skillsRoot != "" {
		files, err = projectSourceSkills(files, skillsRoot)
		if err != nil {
			return nil, err
		}
	}
	owned, err := captureSettings(home)
	if err != nil {
		return nil, err
	}
	if err := add(".claude/settings.json", "claude-settings", "harness-setting", owned); err != nil {
		return nil, err
	}
	registrations, err := captureRegistrations(home)
	if err != nil {
		return nil, err
	}
	for _, registration := range registrations {
		if err := add(
			registration.path, registration.mode, "mcp-client", registration.data); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// captureClaudeBlock extracts the managed import and never the surrounding
// instructions the user wrote.
func captureClaudeBlock(home string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(home, ".claude/CLAUDE.md"))
	if err != nil {
		return nil, err
	}
	text := string(data)
	if strings.Count(text, ClaudeBegin) != 1 || strings.Count(text, ClaudeEnd) != 1 {
		return nil, errors.New("Claude managed block is missing or ambiguous")
	}
	after := text[strings.Index(text, ClaudeBegin)+len(ClaudeBegin):]
	return []byte(after[:strings.Index(after, ClaudeEnd)]), nil
}

func agentInstructionFiles(home string) []string {
	entries, err := os.ReadDir(filepath.Join(home, ".config/agents"))
	if err != nil {
		// A controller without shared agent instructions simply has none to
		// publish, which is not a capture failure.
		return nil
	}
	var relative []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".md") {
			relative = append(relative, filepath.Join(".config/agents", entry.Name()))
		}
	}
	sort.Strings(relative)
	return relative
}

// captureSkillFiles resolves published skill directory symlinks but freezes
// bytes, never links.
func captureSkillFiles(home string) ([]string, error) {
	var files []string
	for _, relative := range []string{".claude/skills", ".codex/skills", ".agents/skills"} {
		found, err := captureSkillRoot(home, relative)
		if err != nil {
			return nil, err
		}
		files = append(files, found...)
	}
	return files, nil
}

func captureSkillRoot(home, relativeRoot string) ([]string, error) {
	root := filepath.Join(home, relativeRoot)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	resolvedHome, err := resolvePath(home)
	if err != nil {
		return nil, err
	}
	var relative []string
	for _, entry := range entries {
		// Codex installs its own system skills with the client.
		if entry.Name() == ".system" {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			continue
		}
		resolved, err := resolvePath(directory)
		if err != nil || !isWithin(resolved, resolvedHome) {
			// An operating-system supplied skill link, for example one into
			// /usr/share, stays package-owned. Its installed bytes never become
			// a writable target.
			continue
		}
		found, err := skillFiles(directory, resolved)
		if err != nil {
			return nil, err
		}
		for _, path := range found {
			path, err := filepath.Rel(home, path)
			if err != nil {
				return nil, err
			}
			relative = append(relative, path)
		}
	}
	return relative, nil
}

// skillFiles lists the regular files under a skill directory, reported under
// the directory's own name even when that name is a symlink.
func skillFiles(directory, resolved string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		suffix, err := filepath.Rel(resolved, path)
		if err != nil {
			return err
		}
		logical := filepath.Join(directory, suffix)
		if containsAny(pathParts(logical), ".git", "__pycache__") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		found = append(found, logical)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

type sourceSkillFile struct {
	skill       string
	relative    string
	content     []byte
	permissions int64
}

// projectSourceSkills replaces repository-owned skill names with exact bytes
// from the pinned setup checkout and emits each skill for Claude/OpenCode and
// Codex. Personal and package-owned skill names remain untouched.
func projectSourceSkills(files []File, skillsRoot string) ([]File, error) {
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot read repository skill source: %w", err)
	}
	var sourceFiles []sourceSkillFile
	owned := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".system" {
			continue
		}
		name := entry.Name()
		if _, err := SafePath(filepath.Join(".agents/skills", name, "SKILL.md")); err != nil {
			return nil, fmt.Errorf("unsafe repository skill name %q", name)
		}
		directory := filepath.Join(skillsRoot, name)
		if info, err := os.Stat(filepath.Join(directory, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("repository skill %s has no regular SKILL.md", name)
		}
		owned[name] = true
		err := filepath.WalkDir(directory, func(path string, item fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if item.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("repository skill %s contains a symlink", name)
			}
			if item.IsDir() {
				return nil
			}
			info, err := item.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || !allowedPermissions[int64(info.Mode().Perm())] {
				return fmt.Errorf("repository skill %s contains an unsupported file", name)
			}
			relative, err := filepath.Rel(directory, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sourceFiles = append(sourceFiles, sourceSkillFile{
				skill: name, relative: relative, content: data,
				permissions: int64(info.Mode().Perm()),
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	projected := make([]File, 0, len(files)+2*len(sourceFiles))
	for _, file := range files {
		parts := pathParts(file.Path)
		if len(parts) >= 3 && parts[1] == "skills" &&
			(parts[0] == ".claude" || parts[0] == ".agents" || parts[0] == ".codex") &&
			owned[parts[2]] {
			continue
		}
		projected = append(projected, file)
	}
	for _, source := range sourceFiles {
		for _, root := range []string{".claude/skills", ".agents/skills"} {
			path := filepath.Join(root, source.skill, source.relative)
			safe, err := SafePath(path)
			if err != nil {
				return nil, err
			}
			projected = append(projected, File{
				Path: safe, Mode: "file", Kind: "skill", Content: source.content,
				SHA256: Digest(source.content), SizeBytes: int64(len(source.content)),
				Permissions: source.permissions,
			})
		}
	}
	return projected, nil
}

// captureSettings freezes only the harness settings the fleet owns, and only
// the allow list out of the permissions the host has.
func captureSettings(home string) ([]byte, error) {
	settings, err := readJSONObject(filepath.Join(home, ".claude/settings.json"))
	if err != nil {
		return nil, err
	}
	owned := map[string]any{}
	for _, key := range settingsKeys {
		if key == "permissions" {
			continue
		}
		if value, present := settings[key]; present {
			owned[key] = value
		}
	}
	allow := []any{}
	if permissions, present := settings["permissions"]; present {
		object, isObject := permissions.(map[string]any)
		if !isObject {
			return nil, errors.New("harness permissions must be an object")
		}
		if value, hasAllow := object["allow"]; hasAllow {
			if allow, err = anyList(value); err != nil {
				return nil, err
			}
		}
	}
	owned["permissions"] = map[string]any{"allow": allow}
	if err := CheckPublic(owned, ""); err != nil {
		return nil, err
	}
	return Canonical(owned)
}

type registration struct {
	path string
	mode string
	data []byte
}

// captureRegistrations freezes the MCP registrations of every harness the
// controller runs, in the one canonical encoding all three are stored in.
func captureRegistrations(home string) ([]registration, error) {
	claude, err := readJSONObject(filepath.Join(home, ".claude.json"))
	if err != nil {
		return nil, err
	}
	claudeServers, present := claude["mcpServers"]
	if !present {
		return nil, errors.New(".claude.json has no mcpServers registrations")
	}
	codexText, err := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if err != nil {
		return nil, err
	}
	codexRoot, err := ParseTOML(string(codexText))
	if err != nil {
		return nil, err
	}
	codexServers, present := codexRoot.Get("mcp_servers")
	if !present {
		return nil, errors.New(".codex/config.toml has no mcp_servers registrations")
	}
	opencode, err := readJSONObject(filepath.Join(home, ".config/opencode/opencode.json"))
	if err != nil {
		return nil, err
	}
	opencodeServers, hasMCP := opencode["mcp"]
	instructions, hasInstructions := opencode["instructions"]
	if !hasMCP || !hasInstructions {
		return nil, errors.New("opencode.json has no mcp or instructions")
	}
	sources := []struct {
		path  string
		mode  string
		value any
	}{
		{".claude.json", "claude-mcp", claudeServers},
		{".codex/config.toml", "codex-settings", map[string]any{"mcp_servers": genericValue(codexServers), "settings": captureCodexSettings(codexRoot)}},
		{".config/opencode/opencode.json", "opencode", map[string]any{
			"mcp": opencodeServers, "instructions": instructions,
		}},
	}
	captured := make([]registration, 0, len(sources))
	for _, source := range sources {
		if err := CheckPublic(source.value, ""); err != nil {
			return nil, err
		}
		data, err := Canonical(source.value)
		if err != nil {
			return nil, err
		}
		captured = append(captured, registration{path: source.path, mode: source.mode, data: data})
	}
	return captured, nil
}

// makeSpec derives the frozen contract from the files a projection will write
// and the bytes it will replace.
func makeSpec(
	home string, desired, prior map[string][]byte, files []File,
	provenance ProjectInstructionRef, identity string,
) (*Spec, error) {
	assets := make([]ManagedAsset, 0, len(files))
	priorAssets := make([]PriorAsset, 0, len(files))
	priorDocuments := make([]any, 0, len(files))
	for index, file := range files {
		active := filepath.Join(home, file.Path)
		data := desired[file.Path]
		old := prior[file.Path]
		if old == nil {
			old = []byte{}
		}
		assetID := fmt.Sprintf("asset-%06d", index)
		assets = append(assets, ManagedAsset{
			AssetID:         assetID,
			Kind:            file.Kind,
			Provenance:      provenance.Repository,
			SourceRevision:  provenance.RepositoryRevision,
			SHA256:          Digest(data),
			SizeBytes:       int64(len(data)),
			StagedPath:      filepath.Join(home, environmentsRoot, identity, assetID),
			ActivePath:      active,
			ActivationOrder: index,
		})
		priorAsset := PriorAsset{
			AssetID: assetID, SHA256: Digest(old), SizeBytes: int64(len(old)), ActivePath: active,
		}
		priorAssets = append(priorAssets, priorAsset)
		priorDocuments = append(priorDocuments, map[string]any{
			"asset_id":    priorAsset.AssetID,
			"sha256":      priorAsset.SHA256,
			"size_bytes":  priorAsset.SizeBytes,
			"active_path": priorAsset.ActivePath,
		})
	}
	// The baseline identity is the digest of the complete previous environment,
	// so that a rollback can prove it restored all of it rather than some of it.
	encoded, err := Canonical(priorDocuments)
	if err != nil {
		return nil, err
	}
	spec := &Spec{
		SchemaVersion:       1,
		Adapter:             Adapter,
		HostProfile:         "release-host",
		WorkerIDs:           []string{"release-host"},
		Phases:              Phases,
		Assets:              assets,
		ProjectInstructions: []ProjectInstructionRef{provenance},
		Probes: []BehavioralProbe{{
			ProbeID:        "owned-projection",
			Argv:           []string{"/usr/bin/python3", "-"},
			TimeoutSeconds: 30,
			SafetyClaims: []string{
				"explicit-effect-authority",
				"instruction-safety",
				"preserve-unexplained-changes",
				"skill-safety",
			},
		}},
		RecoverySources:             recoverySourcesFor(home),
		SecretRefs:                  []string{},
		PreviousEnvironmentRevision: Digest(encoded),
		PriorAssets:                 priorAssets,
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

func recoverySourcesFor(home string) []RecoverySource {
	sources := []struct{ kind, owner, path string }{
		{"huyang-journal", "Huyang", ".local/state/huyang"},
		{"lease", "t3-steward", ".local/state/t3-steward"},
		{"t3-thread", "T3", ".local/share/t3"},
		{"worktree", "Git", "Work"},
	}
	projected := make([]RecoverySource, 0, len(sources))
	for _, source := range sources {
		projected = append(projected, RecoverySource{
			Kind: source.kind, Owner: source.owner, Root: filepath.Join(home, source.path),
		})
	}
	return projected
}

// Observation is everything a read-only pass over the target learned.
type Observation struct {
	Component *Component
	Spec      *Spec
	// Desired and Prior are keyed by the component-relative path. A nil Prior
	// entry means the target does not exist, which is not the same as an empty
	// file and is never treated as one.
	Desired map[string][]byte
	Prior   map[string][]byte
	Drift   []DriftObservation
}

// ObserveEnvironment renders what every managed path would become and reports
// the drift, without changing anything.
func ObserveEnvironment(component map[string]any, home, identity string) (*Observation, error) {
	return ObserveEnvironmentWithout(component, home, identity, nil)
}

// ObserveEnvironmentWithout is ObserveEnvironment for a host whose agent tool
// profile leaves out the named tooling MCP servers: they are not registered,
// and the registrations UpKeeper made for them are removed.
func ObserveEnvironmentWithout(component map[string]any, home, identity string, without []string) (*Observation, error) {
	if err := checkWithout(without); err != nil {
		return nil, err
	}
	decoded, err := DecodeComponent(component)
	if err != nil {
		return nil, err
	}
	desired := map[string][]byte{}
	prior := map[string][]byte{}
	for _, file := range decoded.Files {
		path, err := TargetPath(home, file.Path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(path); err == nil {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			prior[file.Path] = data
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		} else {
			prior[file.Path] = nil
		}
		rendered, err := render(file, prior[file.Path], decoded.SourceHome, home, dropping{names: without, home: home})
		if err != nil {
			return nil, err
		}
		desired[file.Path] = rendered
	}
	for _, file := range decoded.RetiredFiles {
		path, err := RetirementPath(home, file.Path)
		if err != nil {
			return nil, err
		}
		var data []byte
		info, statErr := os.Lstat(path)
		if statErr == nil {
			if !info.Mode().IsRegular() || int64(info.Mode().Perm()) != file.Permissions {
				return nil, fmt.Errorf(
					"retired managed file has unexplained changes: %s", file.Path)
			}
			data, err = os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			if Digest(data) != file.SHA256 {
				return nil, fmt.Errorf(
					"retired managed file has unexplained changes: %s", file.Path)
			}
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return nil, statErr
		}
		prior[file.Path] = data
		desired[file.Path] = nil
	}
	spec, err := makeSpec(
		home, desired, prior, decoded.Files, decoded.Spec.ProjectInstructions[0], identity)
	if err != nil {
		return nil, err
	}
	observed := map[string]Observed{}
	for index, asset := range spec.Assets {
		content := prior[decoded.Files[index].Path]
		item := Observed{Evidence: "owned projection"}
		if content != nil {
			digest := Digest(content)
			item.SHA256 = &digest
		}
		observed[asset.AssetID] = item
	}
	drift := ClassifyAssetDrift(spec, observed, nil)
	for _, file := range decoded.RetiredFiles {
		if prior[file.Path] == nil {
			continue
		}
		observedDigest := Digest(prior[file.Path])
		drift = append(drift, DriftObservation{
			Subject: "retired:" + file.Path, State: DriftMismatch,
			ObservedSHA256: &observedDigest, Evidence: "retired owned projection",
		})
	}
	return &Observation{
		Component: decoded, Spec: spec, Desired: desired, Prior: prior, Drift: drift,
	}, nil
}

// ApplyResult is the outcome of one environment transaction.
type ApplyResult struct {
	State   string
	Changed []string
	Removed []string
	Drift   []DriftObservation
}

// ApplyEnvironment projects the owned files, verifies the result, and restores
// the complete previous environment if any step fails.
//
// A rollback that cannot prove it restored every managed path is an error, not
// a quiet success: a half-restored environment is worse than a failed one.
func ApplyEnvironment(
	component map[string]any, home, runID string, dryRun bool, fault func(string) error,
) (*ApplyResult, error) {
	return ApplyEnvironmentWithout(component, home, runID, dryRun, fault, nil)
}

// ApplyEnvironmentWithout is ApplyEnvironment for a host whose agent tool
// profile leaves out the named tooling MCP servers.
func ApplyEnvironmentWithout(
	component map[string]any, home, runID string, dryRun bool, fault func(string) error, without []string,
) (*ApplyResult, error) {
	observation, err := ObserveEnvironmentWithout(component, home, runID, without)
	if err != nil {
		return nil, err
	}
	files := observation.Component.Files
	changed, err := changedPaths(home, files, observation)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, file := range observation.Component.RetiredFiles {
		if observation.Prior[file.Path] != nil {
			removed = append(removed, file.Path)
		}
	}
	mutations := append(append([]string{}, changed...), removed...)
	if len(mutations) == 0 || dryRun {
		state := "converged"
		if len(mutations) > 0 {
			state = "updated"
		}
		return &ApplyResult{
			State: state, Changed: changed, Removed: removed, Drift: observation.Drift,
		}, nil
	}
	allFiles := append(append([]File{}, files...), observation.Component.RetiredFiles...)
	paths, modes, err := resolveTargets(home, allFiles)
	if err != nil {
		return nil, err
	}
	for _, file := range observation.Component.RetiredFiles {
		paths[file.Path], err = RetirementPath(home, file.Path)
		if err != nil {
			return nil, err
		}
	}
	if err := journal(home, runID, observation, files, modes); err != nil {
		return nil, err
	}
	activated, activateErr := activate(observation, files, home, paths, mutations, fault)
	if activateErr != nil {
		if err := restore(observation, paths, modes, activated); err != nil {
			return nil, err
		}
		return &ApplyResult{
			State: "rolled-back", Changed: changed, Removed: removed, Drift: observation.Drift,
		}, nil
	}
	return &ApplyResult{
		State: "updated", Changed: changed, Removed: removed, Drift: observation.Drift,
	}, nil
}

// changedPaths lists the managed paths whose content or mode differs from what
// the release wants. A file the harness reformatted without changing any owned
// key is deliberately not on this list.
func changedPaths(home string, files []File, observation *Observation) ([]string, error) {
	changed := []string{}
	for _, file := range files {
		if !sameContent(observation.Desired[file.Path], observation.Prior[file.Path]) {
			changed = append(changed, file.Path)
			continue
		}
		path, err := TargetPath(home, file.Path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if int64(info.Mode().Perm()) != file.Permissions {
			changed = append(changed, file.Path)
		}
	}
	return changed, nil
}

func resolveTargets(
	home string, files []File,
) (paths map[string]string, modes map[string]os.FileMode, err error) {
	paths = map[string]string{}
	modes = map[string]os.FileMode{}
	distinct := map[string]bool{}
	for _, file := range files {
		path, err := TargetPath(home, file.Path)
		if err != nil {
			return nil, nil, err
		}
		paths[file.Path] = path
		distinct[path] = true
		modes[file.Path] = 0o600
		if info, err := os.Stat(path); err == nil {
			modes[file.Path] = info.Mode().Perm()
		}
	}
	if len(distinct) != len(paths) {
		return nil, nil, errors.New("managed paths alias one another")
	}
	return paths, modes, nil
}

// journal freezes the complete prior identity and bytes before the first
// activation, so that a crash mid-transaction still leaves a way back.
func journal(
	home, runID string, observation *Observation, files []File, modes map[string]os.FileMode,
) error {
	root := filepath.Join(home, environmentsRoot, runID)
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	spec, err := observation.Spec.ToMap()
	if err != nil {
		return err
	}
	encoded, err := Canonical(spec)
	if err != nil {
		return err
	}
	if err := AtomicWrite(filepath.Join(root, "spec.json"), encoded, 0o600); err != nil {
		return err
	}
	record := map[string]any{}
	for path, content := range observation.Prior {
		var encodedContent any
		if content != nil {
			encodedContent = base64.StdEncoding.EncodeToString(content)
		}
		record[path] = map[string]any{
			"content": encodedContent, "mode": int64(modes[path]),
		}
	}
	encoded, err = Canonical(record)
	if err != nil {
		return err
	}
	if err := AtomicWrite(filepath.Join(root, "prior.json"), encoded, 0o600); err != nil {
		return err
	}
	for index, file := range files {
		staged := filepath.Join(root, fmt.Sprintf("asset-%06d", index))
		if err := AtomicWrite(staged, observation.Desired[file.Path], 0o600); err != nil {
			return err
		}
	}
	return nil
}

// activate writes every changed path and then proves that all of them, changed
// or not, hold exactly what the release asked for.
//
// A target that moved between the observation and the write is refused rather
// than overwritten: the concurrent change is somebody's work.
func activate(
	observation *Observation, files []File, home string, paths map[string]string,
	changed []string, fault func(string) error,
) ([]string, error) {
	activated := []string{}
	for _, path := range changed {
		target := paths[path]
		mode := os.FileMode(0o600)
		for _, file := range files {
			if file.Path == path {
				mode = os.FileMode(file.Permissions)
			}
		}
		if observation.Desired[path] == nil {
			fresh, err := RetirementPath(home, path)
			if err != nil || fresh != target {
				if err != nil {
					return activated, err
				}
				return activated, errors.New("retired managed path changed since observation")
			}
			var expectedMode int64
			for _, file := range observation.Component.RetiredFiles {
				if file.Path == path {
					expectedMode = file.Permissions
				}
			}
			info, err := os.Lstat(fresh)
			if err != nil || !info.Mode().IsRegular() || int64(info.Mode().Perm()) != expectedMode {
				return activated, errors.New("retired managed file changed since observation")
			}
			actual, err := os.ReadFile(fresh)
			if err != nil || !sameContent(actual, observation.Prior[path]) {
				return activated, errors.New("retired managed file changed since observation")
			}
			if err := os.Remove(fresh); err != nil {
				return activated, err
			}
		} else {
			actual, err := readIfPresent(target)
			if err != nil {
				return activated, err
			}
			if !sameContent(actual, observation.Prior[path]) {
				return activated, errors.New("managed file changed since observation")
			}
			if err := AtomicWrite(target, observation.Desired[path], mode); err != nil {
				return activated, err
			}
		}
		activated = append(activated, path)
		if fault != nil {
			if err := fault(path); err != nil {
				return activated, err
			}
		}
	}
	for index, file := range files {
		target := paths[file.Path]
		data, err := os.ReadFile(target)
		if err != nil {
			return activated, err
		}
		info, err := os.Stat(target)
		if err != nil {
			return activated, err
		}
		if Digest(data) != observation.Spec.Assets[index].SHA256 ||
			int64(info.Mode().Perm()) != file.Permissions {
			return activated, errors.New("F05 active asset failed verification")
		}
	}
	for _, file := range observation.Component.RetiredFiles {
		if _, err := os.Stat(paths[file.Path]); !errors.Is(err, fs.ErrNotExist) {
			if err == nil {
				return activated, errors.New("retired managed file still exists")
			}
			return activated, err
		}
	}
	return activated, nil
}

// restore puts every activated path back and then verifies the whole previous
// environment, not only the paths it touched.
//
// Every error it returns begins "rollback failed", an I/O error included: any
// of them leaves part of the previous environment unrestored, and the agent
// stops the whole host only for a failure that says its rollback failed.
func restore(
	observation *Observation, paths map[string]string,
	modes map[string]os.FileMode, activated []string,
) error {
	if err := restorePaths(observation, paths, modes, activated); err != nil {
		if strings.HasPrefix(err.Error(), "rollback failed") {
			return err
		}
		return fmt.Errorf("rollback failed: %w", err)
	}
	return nil
}

func restorePaths(
	observation *Observation, paths map[string]string,
	modes map[string]os.FileMode, activated []string,
) error {
	for index := len(activated) - 1; index >= 0; index-- {
		path := activated[index]
		current, err := readIfPresent(paths[path])
		if err != nil {
			return err
		}
		// A path that no longer holds what this run wrote holds somebody else's
		// change. Reverting it would destroy that change, so the rollback stops
		// and says so rather than claiming a clean restore.
		if !bytes.Equal(current, observation.Desired[path]) {
			return errors.New("rollback failed: unexplained managed-path change")
		}
		if observation.Prior[path] == nil {
			if err := os.Remove(paths[path]); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if err := AtomicWrite(paths[path], observation.Prior[path], modes[path]); err != nil {
			return err
		}
	}
	for path := range paths {
		restored, err := readIfPresent(paths[path])
		if err != nil {
			return err
		}
		if !sameContent(restored, observation.Prior[path]) {
			return errors.New("rollback failed: prior environment did not verify")
		}
	}
	return nil
}

// render turns a captured payload into the exact bytes the target should hold.
//
// Every mode but "file" merges into a document the harness also writes, so the
// host's own keys, accounts and formatting survive the projection.
func render(file File, old []byte, sourceHome, home string, drop dropping) ([]byte, error) {
	if file.Mode == "file" {
		return file.Content, nil
	}
	text := string(old)
	if file.Mode == "claude-block" {
		return renderClaudeBlock(file.Content, text)
	}
	decoded, err := canonjson.Decode(file.Content)
	if err != nil {
		return nil, err
	}
	// Only the controller home prefix is rebased; every other string in a
	// registration is the release's to keep.
	owned, ok := rebase(decoded, sourceHome, home).(map[string]any)
	if !ok {
		return nil, errors.New("an owned registration payload must be an object")
	}
	if file.Mode == "codex-mcp" {
		return renderCodex(text, old, drop.owned(owned), drop)
	}
	if file.Mode == "codex-settings" {
		if err := validateCodexSettings(owned); err != nil {
			return nil, err
		}
		settings := objectOf(owned["settings"])
		projected, err := ProjectRootKeys(text, settings)
		if err != nil {
			return nil, err
		}
		return renderCodex(projected, []byte(projected), drop.owned(objectOf(owned["mcp_servers"])), drop)
	}
	if file.Mode == "claude-mcp" {
		owned = drop.owned(owned)
	}
	if file.Mode == "opencode" {
		if servers, present := owned["mcp"]; present {
			owned["mcp"] = drop.owned(objectOf(servers))
		}
	}
	return renderJSON(file.Mode, text, old, owned, drop)
}

func renderClaudeBlock(payload []byte, text string) ([]byte, error) {
	if strings.Count(text, ClaudeBegin) != strings.Count(text, ClaudeEnd) ||
		strings.Count(text, ClaudeBegin) > 1 {
		return nil, errors.New("ambiguous Claude managed block")
	}
	block := ClaudeBegin + string(payload) + ClaudeEnd
	if strings.Contains(text, ClaudeBegin) {
		start := strings.Index(text, ClaudeBegin)
		end := strings.Index(text, ClaudeEnd)
		if end < start {
			return nil, errors.New("reversed Claude managed block")
		}
		return []byte(text[:start] + block + text[end+len(ClaudeEnd):]), nil
	}
	return []byte(strings.TrimRightFunc(text, isPythonSpace) + "\n\n" + block + "\n"), nil
}

// renderCodex projects the owned MCP registrations into the Codex TOML file
// while leaving every unrelated table exactly as the host wrote it.
func renderCodex(text string, old []byte, owned map[string]any, drop dropping) ([]byte, error) {
	root, err := ParseTOML(text)
	if err != nil {
		return nil, err
	}
	var servers *Table
	if value, present := root.Get("mcp_servers"); present {
		table, isTable := value.(*Table)
		if !isTable {
			return nil, errors.New("the Codex mcp_servers entry is not a table")
		}
		servers = table
	}
	current := servers.Generic()
	merged, err := MergeRegistrations(current, owned)
	if err != nil {
		return nil, err
	}
	removed := drop.registrations(merged, false)
	same, err := sameJSON(current, merged)
	if err != nil {
		return nil, err
	}
	if same {
		if old == nil {
			return []byte{}, nil
		}
		return old, nil
	}
	entries := make([]ServerEntry, 0, len(owned))
	for _, name := range sortedKeysOf(owned) {
		values, isObject := owned[name].(map[string]any)
		if !isObject {
			return nil, errors.New("an owned MCP registration must be an object")
		}
		table, err := mergeServerEntry(servers.Child(name), values)
		if err != nil {
			return nil, err
		}
		entries = append(entries, ServerEntry{Name: name, Table: table})
	}
	rendered, err := projectServerTables(text, entries, removed)
	if err != nil {
		return nil, err
	}
	// The span edit is only allowed to stand if reading the result back proves
	// it registers exactly what the merge intended.
	parsed, err := ParseTOML(rendered)
	if err != nil {
		return nil, errors.New("Codex TOML layout cannot be safely projected")
	}
	same, err = sameJSON(parsed.Child("mcp_servers").Generic(), merged)
	if err != nil {
		return nil, err
	}
	if !same {
		return nil, errors.New("Codex TOML layout cannot be safely projected")
	}
	return []byte(rendered), nil
}

// mergeServerEntry merges one owned registration over the table the file
// already had, keeping the key order of the existing file.
func mergeServerEntry(previous *Table, owned map[string]any) (*Table, error) {
	merged := NewTable()
	for _, key := range previous.Keys() {
		value, _ := previous.Get(key)
		merged.Set(key, value)
	}
	for _, key := range sortedKeysOf(owned) {
		merged.Set(key, owned[key])
	}
	for _, key := range []string{"env", "environment"} {
		previousValue, inPrevious := previous.Get(key)
		ownedValue, inOwned := owned[key]
		if !inPrevious && !inOwned {
			continue
		}
		// The target's own environment entries are merged rather than replaced,
		// because that is where a host keeps the credentials for its clients.
		nested := NewTable()
		if inPrevious {
			table, isTable := previousValue.(*Table)
			if !isTable {
				return nil, fmt.Errorf("the %s of an MCP registration must be a table", key)
			}
			for _, name := range table.Keys() {
				value, _ := table.Get(name)
				nested.Set(name, value)
			}
		}
		if inOwned {
			object, isObject := ownedValue.(map[string]any)
			if !isObject {
				return nil, fmt.Errorf("the %s of an MCP registration must be an object", key)
			}
			for _, name := range sortedKeysOf(object) {
				nested.Set(name, object[name])
			}
		}
		merged.Set(key, nested)
	}
	return merged, nil
}

// renderJSON merges the owned keys into a JSON document the harness shares.
func renderJSON(mode, text string, old []byte, owned map[string]any, drop dropping) ([]byte, error) {
	source := text
	if source == "" {
		source = "{}"
	}
	current, err := canonjson.DecodeObject([]byte(source))
	if err != nil {
		return nil, errors.New("harness settings must be an object")
	}
	switch mode {
	case "claude-settings":
		err = mergeClaudeSettings(current, owned)
	case "claude-mcp":
		var servers map[string]any
		servers, err = MergeRegistrations(objectOf(current["mcpServers"]), owned)
		if err == nil {
			drop.registrations(servers, false)
		}
		current["mcpServers"] = servers
	case "opencode":
		err = mergeOpenCode(current, owned)
		if err == nil {
			drop.registrations(objectOf(current["mcp"]), true)
		}
	default:
		return nil, fmt.Errorf("unsupported owned projection mode: %s", mode)
	}
	if err != nil {
		return nil, err
	}
	// A harness rewrites its own account and cache state, and its own
	// formatting. Those bytes are not owned drift when every projected key
	// already agrees, so the file is left exactly as the harness wrote it.
	if old != nil {
		original, err := canonjson.Decode([]byte(text))
		if err != nil {
			return nil, err
		}
		same, err := sameJSON(original, current)
		if err != nil {
			return nil, err
		}
		if same {
			return old, nil
		}
	}
	return Canonical(current)
}

func mergeClaudeSettings(current, owned map[string]any) error {
	allow, err := stringList(objectOf(current["permissions"])["allow"])
	if err != nil {
		return err
	}
	ownedAllow, err := stringList(objectOf(owned["permissions"])["allow"])
	if err != nil {
		return err
	}
	for key, value := range owned {
		if key != "permissions" {
			current[key] = value
		}
	}
	permissions, present := current["permissions"]
	if !present {
		permissions = map[string]any{}
		current["permissions"] = permissions
	}
	object, isObject := permissions.(map[string]any)
	if !isObject {
		return errors.New("harness permissions must be an object")
	}
	object["allow"] = toAnyList(sortedUnique(append(allow, ownedAllow...)))
	return nil
}

func mergeOpenCode(current, owned map[string]any) error {
	ownedServers, present := owned["mcp"]
	if !present {
		return errors.New("the owned opencode payload has no mcp registrations")
	}
	servers, err := MergeRegistrations(objectOf(current["mcp"]), objectOf(ownedServers))
	if err != nil {
		return err
	}
	current["mcp"] = servers
	ownedInstructions, present := owned["instructions"]
	if !present {
		return errors.New("the owned opencode payload has no instructions")
	}
	existing, err := anyList(current["instructions"])
	if err != nil {
		return err
	}
	ownedList, err := anyList(ownedInstructions)
	if err != nil {
		return err
	}
	// The shared instruction directory is fleet-owned, so the release decides
	// which of its files OpenCode loads. An entry of the target's own is kept:
	// only a generated one the release no longer names is dropped, which is how a
	// file that moved out of the loaded set stops being loaded everywhere.
	kept := make([]any, 0, len(existing))
	for _, entry := range existing {
		if text, isText := entry.(string); isText &&
			strings.Contains(text, managedInstructionDirectory) {
			continue
		}
		kept = append(kept, entry)
	}
	current["instructions"] = dedupeInOrder(append(ownedList, kept...))
	return nil
}

// managedInstructionDirectory names the directory whose instruction files the
// release owns. It is matched as a path fragment rather than against one home
// directory, because the targets do not share a user name.
const managedInstructionDirectory = "/.config/agents/"

// MergeRegistrations overlays the owned registrations on the ones a target
// already has, keeping every field the target added.
func MergeRegistrations(current, owned map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(current)+len(owned))
	for name, value := range current {
		result[name] = value
	}
	for _, name := range sortedKeysOf(owned) {
		values, isObject := owned[name].(map[string]any)
		if !isObject {
			return nil, errors.New("an owned MCP registration must be an object")
		}
		previous := map[string]any{}
		if existing, present := current[name]; present {
			object, wasObject := existing.(map[string]any)
			if !wasObject {
				return nil, fmt.Errorf("the existing %s registration is not an object", name)
			}
			previous = object
		}
		merged := map[string]any{}
		for key, value := range previous {
			merged[key] = value
		}
		for key, value := range values {
			merged[key] = value
		}
		for _, key := range []string{"env", "environment"} {
			_, inPrevious := previous[key]
			_, inOwned := values[key]
			if !inPrevious && !inOwned {
				continue
			}
			nested := map[string]any{}
			for key, value := range objectOf(previous[key]) {
				nested[key] = value
			}
			for key, value := range objectOf(values[key]) {
				nested[key] = value
			}
			merged[key] = nested
		}
		result[name] = merged
	}
	return result, nil
}

func rebase(value any, sourceHome, home string) any {
	switch typed := value.(type) {
	case string:
		return strings.ReplaceAll(typed, sourceHome+"/", home+"/")
	case []any:
		rebased := make([]any, len(typed))
		for index, item := range typed {
			rebased[index] = rebase(item, sourceHome, home)
		}
		return rebased
	case map[string]any:
		rebased := make(map[string]any, len(typed))
		for key, item := range typed {
			rebased[key] = rebase(item, sourceHome, home)
		}
		return rebased
	default:
		return value
	}
}

func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return canonjson.DecodeObject(data)
}

func readIfPresent(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// sameContent compares two payloads where nil means absent. An absent file and
// an empty file are different states and are never treated as equal.
func sameContent(left, right []byte) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	return bytes.Equal(left, right)
}

func sameJSON(left, right any) (bool, error) {
	encodedLeft, err := canonjson.Compact(left)
	if err != nil {
		return false, err
	}
	encodedRight, err := canonjson.Compact(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(encodedLeft, encodedRight), nil
}

func objectOf(value any) map[string]any {
	object, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return object
}

func anyList(value any) ([]any, error) {
	if value == nil {
		return []any{}, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("expected a list")
	}
	return items, nil
}

func stringList(value any) ([]string, error) {
	items, err := anyList(value)
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(items))
	for _, item := range items {
		text, isText := item.(string)
		if !isText {
			return nil, errors.New("a permission entry must be a string")
		}
		texts = append(texts, text)
	}
	return texts, nil
}

func toAnyList(values []string) []any {
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	return items
}

func dedupeInOrder(values []any) []any {
	seen := map[string]bool{}
	unique := make([]any, 0, len(values))
	for _, value := range values {
		key, err := canonjson.Compact(value)
		if err != nil {
			unique = append(unique, value)
			continue
		}
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		unique = append(unique, value)
	}
	return unique
}

func sameKeys(values map[string]any, fields []string) bool {
	if len(values) != len(fields) {
		return false
	}
	for _, field := range fields {
		if _, present := values[field]; !present {
			return false
		}
	}
	return true
}

// pathParts splits a path the way Python's PurePath does, dropping the empty
// and single-dot components a path may carry.
func pathParts(value string) []string {
	var parts []string
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." {
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsAny(values []string, wanted ...string) bool {
	for _, value := range wanted {
		if contains(values, value) {
			return true
		}
	}
	return false
}

func hasControlCharacter(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 {
			return true
		}
	}
	return false
}

func isWithin(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// resolvePath resolves symbolic links as far as they lead, keeping components
// that do not exist yet.
//
// A symbolic link is followed even when its target is missing, because a link
// out of the home directory has to be refused whether or not it points at
// anything.
func resolvePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = absolute
	}
	resolved := string(filepath.Separator)
	pending := pathParts(filepath.Clean(path))
	for links := 0; len(pending) > 0; {
		name := pending[0]
		pending = pending[1:]
		if name == ".." {
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, name)
		info, err := os.Lstat(candidate)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			resolved = candidate
			continue
		}
		links++
		if links > 64 {
			return "", fmt.Errorf("too many symbolic links while resolving %s", path)
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			resolved = string(filepath.Separator)
		}
		pending = append(pathParts(target), pending...)
	}
	return resolved, nil
}
