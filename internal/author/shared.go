package author

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// SharedDirectory is the candidate source root in UpKeeper.
const SharedDirectory = "shared/agent-instructions"

// SharedOwner describes candidate source ownership, never activation authorization.
type SharedOwner struct {
	SchemaVersion       int      `json:"schema_version"`
	Owner               string   `json:"owner"`
	State               string   `json:"state"`
	SourceRepository    string   `json:"source_repository"`
	LegacyRepository    string   `json:"legacy_source_repository"`
	LegacyCommit        string   `json:"legacy_source_commit"`
	Generator           string   `json:"generator"`
	SourceFiles         []string `json:"source_files"`
	InstructionFiles    []string `json:"instruction_files"`
	SkillRoots          []string `json:"skill_roots"`
	FutureMerges        []string `json:"future_merges"`
	RetiredReference    string   `json:"retired_reference"`
	PolicyInputContract string   `json:"policy_input_contract,omitempty"`
	LegacyTree          string   `json:"legacy_source_tree,omitempty"`
}

// SharedPreparation has a private disposable source tree. Close removes it.
// Sources can feed RefreshDerived in tests/scratch, but no release selects it.
type SharedPreparation struct {
	Repository   string                  `json:"repository"`
	Commit       string                  `json:"input_source_commit"`
	Owner        SharedOwner             `json:"owner"`
	SourceSHA256 map[string]string       `json:"source_sha256"`
	OutputSHA256 map[string]string       `json:"output_sha256"`
	Sources      agentenv.DerivedSources `json:"-"`
	Policy       *SharedPolicy           `json:"policy,omitempty"`
	PolicyBytes  []byte                  `json:"-"`
	scratch      string
}

func (p *SharedPreparation) Close() error { return os.RemoveAll(p.scratch) }

// PrepareShared reads only a full local Git object identity, never a branch,
// installed home, network fetch or host capture. The default author path is unchanged.
func PrepareShared(repository, commit string) (*SharedPreparation, error) {
	return prepareShared(repository, commit, nil)
}

func prepareShared(repository, commit string, input *sharedPolicyInput) (_ *SharedPreparation, err error) {
	if _, err = safety.ValidateGitCommit(commit); err != nil {
		return nil, err
	}
	// A pinned object must not be reinterpreted by local replacement refs.
	// Scope this runner to scratch preparation; legacy author behavior is unchanged.
	runner := func(command procx.Command) procx.Result {
		if len(command.Argv) > 0 && command.Argv[0] == "git" {
			command.Argv = append([]string{"git", "--no-replace-objects"}, command.Argv[1:]...)
		}
		return procx.Run(command)
	}
	origin, err := git(runner, repository, 30*time.Second, "remote", "get-url", "--push", "origin")
	if err != nil {
		return nil, err
	}
	if origin != "https://github.com/iryzhkov/UpKeeper.git" && origin != "git@github.com:iryzhkov/UpKeeper.git" {
		return nil, errors.New("shared source requires the UpKeeper repository identity")
	}
	resolved, err := git(runner, repository, 30*time.Second, "rev-parse", "--verify", "--end-of-options", commit+"^{commit}")
	if err != nil || resolved != commit {
		return nil, errors.New("shared source commit is missing or not exact")
	}
	scratch, err := os.MkdirTemp("", "upkeeper-shared-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(scratch)
		}
	}()
	checkout := filepath.Join(scratch, "checkout")
	if err = extract(runner, repository, commit, checkout); err != nil {
		return nil, err
	}
	source := filepath.Join(checkout, SharedDirectory)
	ownerPath := filepath.Join(source, "owner.json")
	ownerInfo, err := os.Lstat(ownerPath)
	if err != nil || !ownerInfo.Mode().IsRegular() || ownerInfo.Size() <= 0 || ownerInfo.Size() > 256<<10 {
		return nil, errors.New("shared owner type or bound")
	}
	var ownerBytes []byte
	if input != nil {
		ownerBytes, _, err = sharedRead(ownerPath, 256<<10)
	} else {
		ownerBytes, err = os.ReadFile(ownerPath)
	}
	if err != nil {
		return nil, err
	}
	if err = ValidateSharedOwnerEncoding(ownerBytes); err != nil {
		return nil, err
	}
	var owner SharedOwner
	decoder := json.NewDecoder(strings.NewReader(string(ownerBytes)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&owner); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing owner descriptor data")
	}
	err = nil
	validVersion := owner.SchemaVersion == 1 && owner.LegacyCommit == "c6f58c421053690cff9ef1de04bc544cbb1fe720" && owner.PolicyInputContract == "" && owner.LegacyTree == "" ||
		owner.SchemaVersion == 2 && owner.LegacyCommit == sharedUpstreamCommit && owner.PolicyInputContract == sharedInputContract && owner.LegacyTree == sharedUpstreamTree
	if owner.SchemaVersion == 2 && input == nil {
		return nil, errors.New("shared v2 source requires explicit policy options")
	}
	if input != nil && owner.SchemaVersion != 2 {
		return nil, errors.New("shared policy preparation requires v2 source")
	}
	if !validVersion || owner.Owner != "upkeeper" || owner.State != "candidate-inert" ||
		owner.SourceRepository != "github.com/iryzhkov/UpKeeper" || owner.Generator != Generator ||
		owner.LegacyRepository != "github.com/iryzhkov/omarchy-setup" ||
		!reflect.DeepEqual(owner.SkillRoots, []string{".agents/skills", ".claude/skills"}) ||
		!reflect.DeepEqual(owner.FutureMerges, []string{".claude/CLAUDE.md:omarchy-setup import fence", ".config/opencode/opencode.json:instructions"}) ||
		owner.RetiredReference != ".config/agents/agent99.md" {
		return nil, errors.New("unsupported shared owner identity or activation state")
	}
	if owner.SchemaVersion == 2 && !reflect.DeepEqual(owner.SourceFiles, SharedSourcePaths()) {
		return nil, errors.New("shared v2 source inventory")
	}
	p := &SharedPreparation{Repository: owner.SourceRepository, Commit: commit, Owner: owner,
		SourceSHA256: map[string]string{"owner.json": digestOf(ownerBytes)}, OutputSHA256: map[string]string{}, scratch: scratch}
	actual := []string{}
	var sourceTotal int64
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("shared sources must be regular files")
		}
		relative, e := filepath.Rel(source, path)
		if e != nil {
			return e
		}
		relative = filepath.ToSlash(relative)
		if relative == "owner.json" {
			return nil
		}
		actual = append(actual, relative)
		if owner.SchemaVersion == 2 {
			i := sort.SearchStrings(owner.SourceFiles, relative)
			if i == len(owner.SourceFiles) || owner.SourceFiles[i] != relative {
				return errors.New("shared source unexpected path")
			}
			info, e := regularOutput(source, relative)
			if e != nil {
				return e
			}
			mode := os.FileMode(0644)
			if relative == Generator || relative == "config/bin/agents-role-project" {
				mode = 0755
			}
			if info.Size() <= 0 || info.Size() > 16<<20 || info.Mode().Perm() != mode {
				return errors.New("shared source mode or bound")
			}
			sourceTotal += info.Size()
			if sourceTotal > 64<<20 {
				return errors.New("shared source aggregate")
			}
		}
		var content []byte
		if owner.SchemaVersion == 2 {
			content, _, e = sharedRead(path, sharedFileLimit)
		} else {
			content, e = os.ReadFile(path)
		}
		if e != nil {
			return e
		}
		p.SourceSHA256[relative] = digestOf(content)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, owner.SourceFiles) {
		return nil, errors.New("shared source inventory differs from descriptor")
	}
	home := filepath.Join(scratch, "home")
	if err = os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}
	if input != nil {
		return prepareSharedPolicy(p, runner, source, home, input)
	}
	generated := runner(procx.Command{
		Argv: []string{"bash", filepath.Join(source, Generator), "--scratch-only", "--root", source},
		Dir:  source, Env: []string{"HOME=" + home, "TMPDIR=" + scratch, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"},
		Timeout: 120 * time.Second,
	})
	if !generated.OK() {
		return nil, fmt.Errorf("candidate generation failed: %s", generated.Stderr)
	}
	instructions := map[string]agentenv.DerivedFile{}
	reader := tar.NewReader(strings.NewReader(generated.Stdout))
	for {
		header, e := reader.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return nil, e
		}
		if header.Typeflag == tar.TypeDir && header.Name == "./" {
			continue
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag != tar.TypeReg || filepath.Base(name) != name || !strings.HasSuffix(name, ".md") {
			return nil, errors.New("invalid generator export")
		}
		path := instructionDirectory + "/" + name
		if _, exists := instructions[path]; exists {
			return nil, errors.New("duplicate generated path")
		}
		content, e := io.ReadAll(reader)
		if e != nil {
			return nil, e
		}
		instructions[path] = agentenv.DerivedFile{Content: content, Permissions: 0644}
	}
	shared, ok := instructions[instructionDirectory+"/AGENTS.md"]
	if !ok {
		return nil, errors.New("missing shared AGENTS.md")
	}
	instructions[".codex/AGENTS.md"] = agentenv.DerivedFile{
		Content: []byte("<!-- fleet:start -->\n" + strings.TrimSpace(string(shared.Content)) + "\n<!-- fleet:end -->\n"), Permissions: 0644}
	claude, err := os.ReadFile(filepath.Join(source, "config/claude/CLAUDE.md"))
	if err != nil {
		return nil, err
	}
	instructions[".claude/omarchy-setup/CLAUDE.md"] = agentenv.DerivedFile{Content: claude, Permissions: 0644}
	outputs := []string{}
	for path, file := range instructions {
		outputs = append(outputs, path)
		p.OutputSHA256[path] = digestOf(file.Content)
	}
	sort.Strings(outputs)
	if !reflect.DeepEqual(outputs, owner.InstructionFiles) {
		return nil, errors.New("generated inventory differs from descriptor")
	}
	p.Sources = agentenv.DerivedSources{SkillsRoot: filepath.Join(source, skillsDirectory),
		Instructions: instructions, InstructionDirectories: []string{instructionDirectory}}
	return p, nil
}

// RequireLegacyWriter is the future fence interface. It cannot validate active
// generations yet: any proposed transition refuses. It is deliberately not wired
// into existing host writers until SAME-ACTIVATION is independently accepted.
func RequireLegacyWriter(state string) error {
	if state == "legacy" {
		return nil
	}
	return errors.New("ownership transition unsupported: legacy writer must refuse")
}
