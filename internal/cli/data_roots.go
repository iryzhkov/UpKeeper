package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const inspectionOutputLimit = 64 << 10

// These are mutable, unqualified observations of ORIGINAL selected Git objects.
// They are never snapshot custody, executable authority, compatibility or approval.
type inspectedRoot struct {
	Path           string
	HEAD           string
	Tree           string
	ObservedOrigin string
}
type rootPairObservation struct {
	Code                  inspectedRoot
	Data                  inspectedRoot
	Mode                  string
	Qualification         string
	MarkerSource          string
	WorktreeQualification string
}
type inspectionGitOperation uint8

const (
	inspectionTop inspectionGitOperation = iota
	inspectionBare
	inspectionHEAD
	inspectionTree
	inspectionOrigin
	inspectionPartial
	inspectionMarkers
	inspectionType
	inspectionSize
	inspectionBlob
)

type inspectionGitCommand struct {
	root      string
	operation inspectionGitOperation
	object    string
	code      bool
}

// Implementations must synchronously stop and wait for their work on cancellation.
type inspectionGitRunner interface {
	run(context.Context, inspectionGitCommand) ([]byte, []byte, error)
}
type inspectionExecRunner struct{ executable string }

func inspectionEnvironment() []string {
	return []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "LANG=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat"}
}
func inspectionSystemGit() (string, error) {
	for _, path := range []string{"/usr/bin/git", "/bin/git"} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return resolved, nil
		}
	}
	return "", errors.New("inspection system Git unavailable")
}
func inspectionHex(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (c inspectionGitCommand) argv() ([]string, error) {
	if !filepath.IsAbs(c.root) {
		return nil, errors.New("invalid inspection command root")
	}
	args := []string{"--no-replace-objects", "--no-pager", "-c", "core.fsmonitor=false", "-C", c.root}
	var tail []string
	switch c.operation {
	case inspectionTop:
		tail = []string{"rev-parse", "--show-toplevel"}
	case inspectionBare:
		tail = []string{"rev-parse", "--is-bare-repository"}
	case inspectionHEAD:
		tail = []string{"rev-parse", "--verify", "HEAD"}
	case inspectionOrigin:
		tail = []string{"config", "--no-includes", "--local", "--get-all", "remote.origin.url"}
	case inspectionPartial:
		tail = []string{"config", "--no-includes", "--local", "--get-regexp", "^(extensions\\.(partialclone|worktreeconfig)|remote\\..*\\.promisor|include\\.path|includeif\\..*\\.path)$"}
	case inspectionTree, inspectionMarkers, inspectionType, inspectionSize, inspectionBlob:
		if !inspectionHex(c.object) {
			return nil, errors.New("invalid inspection object")
		}
		switch c.operation {
		case inspectionTree:
			tail = []string{"rev-parse", "--verify", c.object + "^{tree}"}
		case inspectionMarkers:
			tail = []string{"ls-tree", "-z", c.object, "--"}
			if c.code {
				tail = append(tail, "go.mod", "cmd/upkeeper/main.go")
			} else {
				tail = append(tail, "inventory/hosts.yml", "desired/components.yml", "desired/release.json")
			}
		case inspectionType:
			tail = []string{"cat-file", "-t", c.object}
		case inspectionSize:
			tail = []string{"cat-file", "-s", c.object}
		case inspectionBlob:
			tail = []string{"cat-file", "blob", c.object}
		}
	default:
		return nil, errors.New("unsupported inspection command")
	}
	return append(args, tail...), nil
}

type inspectionBuffer struct {
	buffer   bytes.Buffer
	overflow bool
	cancel   context.CancelFunc
}

func (b *inspectionBuffer) Len() int      { return b.buffer.Len() }
func (b *inspectionBuffer) Bytes() []byte { return b.buffer.Bytes() }

func (b *inspectionBuffer) Write(p []byte) (int, error) {
	remaining := inspectionOutputLimit - b.Len()
	if len(p) > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		b.overflow = true
		if b.cancel != nil {
			b.cancel()
		}
		return remaining, errors.New("inspection output limit")
	}
	return b.buffer.Write(p)
}
func (r inspectionExecRunner) run(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	args, err := c.argv()
	if err != nil {
		return nil, nil, err
	}
	processContext, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(processContext, r.executable, args...)
	cmd.Dir, cmd.Env = c.root, inspectionEnvironment()
	// Bound pipe drainage too; Run always waits for the direct process.
	cmd.WaitDelay = time.Second
	out, stderr := inspectionBuffer{cancel: stop}, inspectionBuffer{cancel: stop}
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if out.overflow || stderr.overflow {
		return nil, nil, errors.New("inspection output limit")
	}
	if err != nil {
		// A missing optional config key is distinct from an unsupported command.
		var exit *exec.ExitError
		if c.operation == inspectionPartial && errors.As(err, &exit) && exit.ExitCode() == 1 && out.Len() == 0 && stderr.Len() == 0 {
			return nil, nil, nil
		}
		return nil, nil, errors.New("inspection Git failed")
	}
	return out.Bytes(), stderr.Bytes(), nil
}

func inspectRootPair(ctx context.Context, codePath, dataPath string, runner inspectionGitRunner) (rootPairObservation, error) {
	parent, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	zero := rootPairObservation{}
	fail := func(err error) (rootPairObservation, error) {
		if parent.Err() != nil {
			return zero, parent.Err()
		}
		return zero, err
	}
	if err := parent.Err(); err != nil {
		return fail(err)
	}
	if runner == nil {
		binary, err := inspectionSystemGit()
		if err != nil {
			return fail(err)
		}
		runner = inspectionExecRunner{executable: binary}
	}
	code, err := inspectOneRoot(parent, codePath, true, runner)
	if err != nil {
		return fail(err)
	}
	data, err := inspectOneRoot(parent, dataPath, false, runner)
	if err != nil {
		return fail(err)
	}
	// Recheck both only after all object reads, including the other root.
	for _, root := range []inspectedRoot{code, data} {
		head, err := inspectionRun(parent, runner, inspectionGitCommand{root: root.Path, operation: inspectionHEAD})
		if err != nil {
			return fail(err)
		}
		if string(head) != root.HEAD+"\n" {
			return fail(errors.New("inspection HEAD moved"))
		}
	}
	if err := parent.Err(); err != nil {
		return fail(err)
	}
	mode := "split"
	if code.Path == data.Path {
		mode = "colocated"
	}
	return rootPairObservation{Code: code, Data: data, Mode: mode, Qualification: "unqualified",
		MarkerSource: "selected_commit", WorktreeQualification: "not_checked"}, nil
}
func inspectionRun(ctx context.Context, runner inspectionGitRunner, cmd inspectionGitCommand) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	child, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, stderr, err := runner.run(child, cmd)
	if child.Err() != nil {
		return nil, child.Err()
	}
	if len(out) > inspectionOutputLimit || len(stderr) > inspectionOutputLimit {
		return nil, errors.New("inspection output limit")
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, errors.New("inspection Git failed")
	}
	return out, nil
}
func inspectOneRoot(ctx context.Context, path string, code bool, runner inspectionGitRunner) (inspectedRoot, error) {
	zero := inspectedRoot{}
	if !filepath.IsAbs(path) {
		return zero, errors.New("inspection requires absolute roots")
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return zero, errors.New("inspection root unavailable")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return zero, errors.New("inspection root is not a directory")
	}
	run := func(op inspectionGitOperation, object string) ([]byte, error) {
		return inspectionRun(ctx, runner, inspectionGitCommand{root: path, operation: op, object: object, code: code})
	}
	top, err := run(inspectionTop, "")
	if err != nil {
		return zero, err
	}
	if string(top) != path+"\n" {
		return zero, errors.New("inspection requires Git top level")
	}
	bare, err := run(inspectionBare, "")
	if err != nil {
		return zero, err
	}
	if string(bare) != "false\n" {
		return zero, errors.New("inspection refuses bare repository")
	}
	policy, err := run(inspectionPartial, "")
	if err != nil {
		return zero, err
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(policy), "\n"), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		key = strings.ToLower(key)
		if !ok {
			return zero, errors.New("unsupported inspection config policy")
		}
		if key == "extensions.partialclone" {
			return zero, errors.New("inspection refuses partial clone")
		}
		// Object commands read repository includes and enabled worktree config.
		// Refuse these unsupported sources before interpreting any object.
		if key == "include.path" || strings.HasPrefix(key, "includeif.") {
			return zero, errors.New("inspection refuses repository includes")
		}
		if key == "extensions.worktreeconfig" {
			switch strings.ToLower(value) {
			case "false", "no", "off", "0":
				continue
			default:
				return zero, errors.New("inspection refuses worktree configuration")
			}
		}
		if !strings.HasPrefix(key, "remote.") || !strings.HasSuffix(key, ".promisor") {
			return zero, errors.New("unsupported inspection config policy")
		}
		switch strings.ToLower(value) {
		case "false", "no", "off", "0":
		default:
			return zero, errors.New("inspection refuses promisor configuration")
		}
	}
	head, err := run(inspectionHEAD, "")
	if err != nil {
		return zero, err
	}
	selected := strings.TrimSuffix(string(head), "\n")
	if !inspectionHex(selected) || string(head) != selected+"\n" {
		return zero, errors.New("invalid inspection HEAD")
	}
	tree, err := run(inspectionTree, selected)
	if err != nil {
		return zero, err
	}
	treeID := strings.TrimSuffix(string(tree), "\n")
	if !inspectionHex(treeID) || string(tree) != treeID+"\n" {
		return zero, errors.New("invalid inspection tree")
	}
	for object, kind := range map[string]string{selected: "commit", treeID: "tree"} {
		if _, err := inspectionObject(ctx, runner, path, object, kind); err != nil {
			return zero, err
		}
	}
	origin, err := run(inspectionOrigin, "")
	if err != nil {
		return zero, err
	}
	observed, err := inspectionOriginValue(origin)
	if err != nil {
		return zero, err
	}
	entries, err := run(inspectionMarkers, selected)
	if err != nil {
		return zero, err
	}
	required := map[string]bool{"inventory/hosts.yml": false, "desired/components.yml": false, "desired/release.json": false}
	if code {
		required = map[string]bool{"go.mod": false, "cmd/upkeeper/main.go": false}
	}
	records := bytes.Split(entries, []byte{0})
	if len(records) < 2 || len(records[len(records)-1]) != 0 {
		return zero, errors.New("invalid inspection marker list")
	}
	for _, record := range records[:len(records)-1] {
		header, name, ok := strings.Cut(string(record), "\t")
		fields := strings.Fields(header)
		seen, wanted := required[name]
		if !ok || !wanted || seen || len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || !inspectionHex(fields[2]) {
			return zero, errors.New("inspection requires regular committed markers")
		}
		size, err := inspectionObject(ctx, runner, path, fields[2], "blob")
		if err != nil {
			return zero, err
		}
		blob, err := run(inspectionBlob, fields[2])
		if err != nil {
			return zero, err
		}
		digest := sha1.New()
		_, _ = fmt.Fprintf(digest, "blob %d%c", len(blob), byte(0))
		_, _ = digest.Write(blob)
		if int64(len(blob)) != size || fmt.Sprintf("%x", digest.Sum(nil)) != fields[2] {
			return zero, errors.New("inspection blob integrity failure")
		}
		if name == "go.mod" && !inspectionModule(blob) {
			return zero, errors.New("inspection module marker invalid")
		}
		required[name] = true
	}
	for _, found := range required {
		if !found {
			return zero, errors.New("inspection committed marker missing")
		}
	}
	return inspectedRoot{Path: path, HEAD: selected, Tree: treeID, ObservedOrigin: observed}, nil
}
func inspectionObject(ctx context.Context, runner inspectionGitRunner, root, object, kind string) (int64, error) {
	out, err := inspectionRun(ctx, runner, inspectionGitCommand{root: root, operation: inspectionType, object: object})
	if err != nil {
		return 0, err
	}
	if string(out) != kind+"\n" {
		return 0, errors.New("inspection object type invalid")
	}
	out, err = inspectionRun(ctx, runner, inspectionGitCommand{root: root, operation: inspectionSize, object: object})
	if err != nil {
		return 0, err
	}
	text := strings.TrimSuffix(string(out), "\n")
	size, err := strconv.ParseInt(text, 10, 64)
	if err != nil || size < 0 || string(out) != strconv.FormatInt(size, 10)+"\n" || size > inspectionOutputLimit {
		return 0, errors.New("inspection object size invalid")
	}
	return size, nil
}
func inspectionOriginValue(raw []byte) (string, error) {
	fail := errors.New("inspection origin unavailable or sensitive")
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return "", fail
	}
	s := string(raw[:len(raw)-1])
	if s == "" || strings.TrimSpace(s) != s {
		return "", fail
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return "", fail
		}
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", fail
		}
	} else if strings.Contains(s, "@") {
		// Only the conventional nonsecret SSH Git username is accepted.
		if !strings.HasPrefix(s, "git@") || strings.Count(s, "@") != 1 || !strings.Contains(s[4:], ":") {
			return "", fail
		}
	}
	return s, nil
}

// Strip Go-style comments while retaining line boundaries and quoted strings.
// Only one standalone module directive is accepted; block directives cannot own it.
func inspectionModule(blob []byte) bool {
	var clean strings.Builder
	inBlock, inString, escaped := false, false, false
	for i := 0; i < len(blob); i++ {
		c := blob[i]
		if inBlock {
			if c == '*' && i+1 < len(blob) && blob[i+1] == '/' {
				inBlock = false
				i++
				clean.WriteByte(' ')
			} else if c == '\n' {
				clean.WriteByte(c)
			}
			continue
		}
		if inString {
			clean.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			clean.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(blob) {
			if blob[i+1] == '*' {
				inBlock = true
				i++
				clean.WriteByte(' ')
				continue
			}
			if blob[i+1] == '/' {
				for i < len(blob) && blob[i] != '\n' {
					i++
				}
				if i < len(blob) {
					clean.WriteByte('\n')
				}
				continue
			}
		}
		clean.WriteByte(c)
	}
	if inBlock || inString {
		return false
	}
	count, depth := 0, 0
	for _, line := range strings.Split(clean.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "module" {
			count++
			if depth != 0 || len(fields) != 2 {
				return false
			}
			module := fields[1]
			if strings.HasPrefix(module, "\"") {
				value, err := strconv.Unquote(module)
				if err != nil {
					return false
				}
				module = value
			}
			if module != "github.com/iryzhkov/UpKeeper" {
				return false
			}
		}
		if fields[len(fields)-1] == "(" {
			depth++
		}
		if fields[0] == ")" {
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return count == 1 && depth == 0
}

var _ io.Writer = (*inspectionBuffer)(nil)
