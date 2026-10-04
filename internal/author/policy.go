package author

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// policyInstructions is the generator's mandatory policy inventory. Native
// ownership is by exact file, never by .claude/agents or .codex directory.
var policyInstructions = []string{
	".claude/omarchy-setup/CLAUDE.md",
	".config/agents/AGENTS.md",
	".config/agents/huyang.md",
	".config/agents/jocasta.md",
	".config/agents/ov-memory.md",
	".config/agents/t3-steward.md",
	".config/agents/t3-campaign-executor-briefs.md",
	".codex/AGENTS.md",
	".claude/agents/reader.md",
	".claude/agents/executor.md",
	".claude/agents/reviewer.md",
	".claude/agents/critical-reviewer.md",
	".claude/agents/planner.md",
	".codex/reader.config.toml",
	".codex/executor.config.toml",
	".codex/reviewer.config.toml",
	".codex/critical-reviewer.config.toml",
	".codex/planner.config.toml",
}

// treeState records source/output bytes, modes and file identities. Directory
// mtimes are deliberately excluded: creating then removing staging files is safe.
type treeEntry struct {
	Mode          os.FileMode
	Digest        string
	Device, Inode uint64
	Modified      int64
}

func treeState(root string) (map[string]treeEntry, error) {
	state := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		item := treeEntry{Mode: info.Mode()}
		if !info.IsDir() {
			item.Modified = info.ModTime().UnixNano()
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				item.Device, item.Inode = uint64(stat.Dev), stat.Ino
			}
			var content []byte
			switch {
			case info.Mode().IsRegular():
				content, err = os.ReadFile(path)
			case info.Mode()&os.ModeSymlink != 0:
				var target string
				target, err = os.Readlink(path)
				content = []byte(target)
			default:
				return failure.Validation("omarchy-setup: unsupported scratch file %s", path)
			}
			if err != nil {
				return err
			}
			item.Digest = digestOf(content)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		state[relative] = item
		return nil
	})
	return state, err
}

// regularOutput rejects links in every path component, including links which
// resolve inside HOME, and hardlinks to other private inputs or outputs.
func regularOutput(home, relative string) (os.FileInfo, error) {
	path := filepath.Join(home, relative)
	for current := path; isWithin(current, home); current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, failure.Validation("omarchy-setup: mandatory output %s: %v", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, failure.Validation("omarchy-setup: linked output %s", relative)
		}
		if current == home {
			break
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, failure.Validation("omarchy-setup: output %s is not regular", relative)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return nil, failure.Validation("omarchy-setup: hardlinked output %s", relative)
	}
	return info, nil
}

func derivePolicy(runner procx.Runner, scratch, checkout, home, temporary string, policy []byte) (agentenv.DerivedSources, error) {
	// The policy is outside both protected source and output trees and is never
	// discovered from HOME. Chmod explicitly defeats the caller's umask.
	path := filepath.Join(scratch, "route-policy.yaml")
	if err := os.WriteFile(path, policy, 0o400); err != nil {
		return agentenv.DerivedSources{}, err
	}
	if err := os.Chmod(path, 0o400); err != nil {
		return agentenv.DerivedSources{}, err
	}
	identity, err := os.Lstat(path)
	if err != nil {
		return agentenv.DerivedSources{}, err
	}
	sourceState, err := treeState(checkout)
	if err != nil {
		return agentenv.DerivedSources{}, err
	}
	unchanged := func() error {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 ||
			!os.SameFile(identity, info) || !identity.ModTime().Equal(info.ModTime()) {
			return failure.Validation("omarchy-setup: reviewed policy changed during generation")
		}
		current, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(policy, current) {
			return failure.Validation("omarchy-setup: reviewed policy bytes changed during generation")
		}
		currentState, err := treeState(checkout)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(sourceState, currentState) {
			return failure.Validation("omarchy-setup: pinned source changed during generation")
		}
		return nil
	}
	command := procx.Command{
		Argv:    []string{filepath.Join(checkout, Generator), "--root", checkout, "--policy", path},
		Dir:     checkout,
		Env:     []string{"HOME=" + home, "TMPDIR=" + temporary, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"},
		Timeout: 120 * time.Second,
	}
	run := func(command procx.Command) error {
		result := runner(command)
		if !result.OK() {
			return failure.Validation("omarchy-setup: policy generator refused (exit %d): %s",
				result.Code, tail(strings.TrimSpace(result.Output()), 400))
		}
		return unchanged()
	}
	if err := run(command); err != nil {
		return agentenv.DerivedSources{}, err
	}
	instructions := map[string]agentenv.DerivedFile{}
	for _, relative := range policyInstructions {
		info, err := regularOutput(home, relative)
		if err != nil {
			return agentenv.DerivedSources{}, err
		}
		switch info.Mode().Perm() {
		case 0o600, 0o644, 0o700, 0o755:
		default:
			return agentenv.DerivedSources{}, failure.Validation("omarchy-setup: unsupported output mode for %s", relative)
		}
		content, err := os.ReadFile(filepath.Join(home, relative))
		if err != nil {
			return agentenv.DerivedSources{}, err
		}
		if len(content) == 0 || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
			return agentenv.DerivedSources{}, failure.Validation("omarchy-setup: invalid output %s", relative)
		}
		instructions[relative] = agentenv.DerivedFile{Content: content, Permissions: int64(info.Mode().Perm())}
	}
	// The pinned generator is the projection authority. Its check mode validates
	// the same explicit policy and compares every expected instruction/native byte.
	outputState, err := treeState(home)
	if err != nil {
		return agentenv.DerivedSources{}, err
	}
	command.Argv = append(command.Argv, "--check")
	if err := run(command); err != nil {
		return agentenv.DerivedSources{}, err
	}
	currentState, err := treeState(home)
	if err != nil {
		return agentenv.DerivedSources{}, err
	}
	if !reflect.DeepEqual(outputState, currentState) {
		return agentenv.DerivedSources{}, failure.Validation("omarchy-setup: outputs changed during generator check")
	}
	// Empty scratch HOME never has OpenCode. Refuse an unexpected side effect
	// rather than capturing it or taking ownership of additional output files.
	if _, err := os.Lstat(filepath.Join(home, ".config/opencode")); !os.IsNotExist(err) {
		return agentenv.DerivedSources{}, failure.Validation("omarchy-setup: generator unexpectedly created OpenCode output")
	}
	return agentenv.DerivedSources{
		SkillsRoot:             filepath.Join(checkout, skillsDirectory),
		Instructions:           instructions,
		InstructionDirectories: []string{instructionDirectory},
	}, nil
}
