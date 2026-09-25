package agentenv

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// DerivedFile is one file an authored release takes from a pinned source tree
// instead of from any host: its bytes and the permissions it is installed with.
type DerivedFile struct {
	Content     []byte
	Permissions int64
}

// DerivedSources is everything an agent-environment component derives from one
// omarchy-setup commit. Every other file the component carries is left exactly
// as it was published.
type DerivedSources struct {
	// SkillsRoot is config/claude/skills of the pinned checkout. Every skill in
	// it is projected to .claude/skills and .agents/skills exactly as capture
	// projects it, and replaces whatever the release carried under that name.
	SkillsRoot string
	// FormerSkills are the skill names the previously pinned commit owned. A
	// skill the new commit no longer has is retired rather than left behind as
	// if it were a personal one.
	FormerSkills []string
	// Instructions are home-relative instruction files generated from the
	// checkout, keyed by the path they are installed at.
	Instructions map[string]DerivedFile
	// InstructionDirectories are home-relative directories whose files are all
	// generated: a released file in one that the generator no longer writes is
	// retired.
	InstructionDirectories []string
}

// Refresh is one owned path an authored refresh changed.
type Refresh struct {
	Path   string
	Action string
	From   string
	To     string
}

// The actions a Refresh reports.
const (
	RefreshChanged = "changed"
	RefreshAdded   = "added"
	RefreshRetired = "retired"
)

// RefreshDerived replaces the files of an agent-environment component that
// derive from an omarchy-setup commit, without reading anything from a host.
//
// When no derived file differs from what the component already publishes the
// component is returned unchanged, the same map and so the same bytes: an
// authored release that pins the commit it already pins is a no-op. Otherwise
// the spec is rebuilt with the same makeSpec capture uses, the one project
// instruction reference is moved to revision, and files the sources no longer
// produce are carried as retirement tombstones the way capture carries them.
func RefreshDerived(
	component map[string]any, sources DerivedSources, revision string,
) (map[string]any, []Refresh, error) {
	decoded, err := DecodeComponent(component)
	if err != nil {
		return nil, nil, err
	}
	if decoded.Spec == nil || len(decoded.Spec.ProjectInstructions) != 1 {
		return nil, nil, errors.New(
			"the agent-environment spec must carry exactly one project instruction reference")
	}
	former := map[string]bool{}
	for _, name := range sources.FormerSkills {
		former[name] = true
	}
	generated := func(path string) bool {
		if _, present := sources.Instructions[path]; present {
			return true
		}
		for _, directory := range sources.InstructionDirectories {
			if strings.HasPrefix(path, strings.TrimSuffix(directory, "/")+"/") {
				return true
			}
		}
		return false
	}
	kept := make([]File, 0, len(decoded.Files))
	for _, file := range decoded.Files {
		parts := pathParts(file.Path)
		formerSkill := len(parts) >= 3 && parts[1] == "skills" &&
			(parts[0] == ".claude" || parts[0] == ".agents" || parts[0] == ".codex") &&
			former[parts[2]]
		if !generated(file.Path) && !formerSkill {
			kept = append(kept, file)
			continue
		}
		if file.Mode != "file" {
			return nil, nil, fmt.Errorf("derived path %s is not a whole-file projection", file.Path)
		}
	}
	files, err := projectSourceSkills(kept, sources.SkillsRoot)
	if err != nil {
		return nil, nil, err
	}
	instructionPaths := make([]string, 0, len(sources.Instructions))
	for path := range sources.Instructions {
		instructionPaths = append(instructionPaths, path)
	}
	sort.Strings(instructionPaths)
	for _, path := range instructionPaths {
		safe, err := SafePath(path)
		if err != nil {
			return nil, nil, err
		}
		instruction := sources.Instructions[path]
		if !allowedPermissions[instruction.Permissions] {
			return nil, nil, fmt.Errorf("generated instruction %s has unsupported permissions", path)
		}
		files = append(files, File{
			Path: safe, Mode: "file", Kind: "global-instruction", Content: instruction.Content,
			SHA256: Digest(instruction.Content), SizeBytes: int64(len(instruction.Content)),
			Permissions: instruction.Permissions,
		})
	}
	sort.SliceStable(files, func(left, right int) bool { return files[left].Path < files[right].Path })

	previous := map[string]File{}
	for _, file := range decoded.Files {
		previous[file.Path] = file
	}
	active := map[string]File{}
	for _, file := range files {
		if _, duplicate := active[file.Path]; duplicate {
			return nil, nil, fmt.Errorf("derived path %s is produced twice", file.Path)
		}
		active[file.Path] = file
	}
	refreshes := []Refresh{}
	for _, file := range files {
		before, present := previous[file.Path]
		switch {
		case !present:
			refreshes = append(refreshes, Refresh{Path: file.Path, Action: RefreshAdded, To: file.SHA256})
		case before.SHA256 != file.SHA256 || before.Permissions != file.Permissions ||
			before.Mode != file.Mode || before.Kind != file.Kind:
			refreshes = append(refreshes, Refresh{
				Path: file.Path, Action: RefreshChanged, From: before.SHA256, To: file.SHA256,
			})
		}
	}
	for _, file := range decoded.Files {
		if _, present := active[file.Path]; !present {
			refreshes = append(refreshes, Refresh{Path: file.Path, Action: RefreshRetired, From: file.SHA256})
		}
	}
	if len(refreshes) == 0 {
		return component, refreshes, nil
	}
	sort.SliceStable(refreshes, func(left, right int) bool {
		return refreshes[left].Path < refreshes[right].Path
	})

	retiredByPath := map[string]File{}
	for _, file := range decoded.RetiredFiles {
		retiredByPath[file.Path] = file
	}
	for _, file := range decoded.Files {
		if _, present := active[file.Path]; !present && file.Mode == "file" {
			retiredByPath[file.Path] = file
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

	desired := map[string][]byte{}
	for _, file := range files {
		desired[file.Path] = file.Content
	}
	provenance := decoded.Spec.ProjectInstructions[0]
	provenance.RepositoryRevision = revision
	spec, err := makeSpec(decoded.SourceHome, desired, desired, files, provenance, "capture")
	if err != nil {
		return nil, nil, err
	}
	refreshed := &Component{
		Files: files, RetiredFiles: retired, Spec: spec, SourceHome: decoded.SourceHome,
	}
	rendered, err := refreshed.ToMap()
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateEnvironment(rendered); err != nil {
		return nil, nil, err
	}
	return rendered, refreshes, nil
}
