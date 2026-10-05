//go:build linux

package author

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

type snapshotDirectory struct {
	fd     int
	stat   syscall.Stat_t
	stable bool
}
type snapshotCustody struct {
	dirs   map[string]*snapshotDirectory
	checks []func() error
	closes []func()
}

func (c *snapshotCustody) release() error {
	for i := len(c.closes) - 1; i >= 0; i-- {
		c.closes[i]()
	}
	var err error
	for _, d := range c.dirs {
		err = errors.Join(err, syscall.Close(d.fd))
	}
	c.dirs = nil
	c.closes = nil
	c.checks = nil
	return err
}

// Every ancestor remains held, rather than retaining only the terminal inode.
// Private directories have exact caller-owned 0700; safe external ancestors use
// the existing root/caller-owned nofollow/sticky-parent rule.
func (c *snapshotCustody) directory(path, privateRoot string, stable bool) error {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	current := ""
	for _, part := range parts {
		current += "/" + part
		private := privateRoot != "" && (current == privateRoot || strings.HasPrefix(current, privateRoot+"/"))
		if d := c.dirs[current]; d != nil {
			if private && (d.stat.Uid != uint32(os.Getuid()) || d.stat.Mode&07777 != 0700) {
				return errors.New("catalog private directory mode/owner")
			}
			d.stable = d.stable || (stable && current == path) || private
			continue
		}
		fd, err := sharedHeld(current, true)
		if err != nil {
			return err
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil || st.Mode&07000 != 0 && st.Mode&syscall.S_ISVTX == 0 ||
			private && (st.Uid != uint32(os.Getuid()) || st.Mode&07777 != 0700) {
			syscall.Close(fd)
			return errors.New("catalog directory mode/owner")
		}
		c.dirs[current] = &snapshotDirectory{fd, st, (stable && current == path) || private}
	}
	return nil
}
func (c *snapshotCustody) verifyDirectories() error {
	for path, d := range c.dirs {
		fd, err := sharedHeld(path, true)
		if err != nil {
			return err
		}
		var st, held syscall.Stat_t
		a, b := syscall.Fstat(fd, &st), syscall.Fstat(d.fd, &held)
		syscall.Close(fd)
		if a != nil || b != nil || st.Dev != d.stat.Dev || st.Ino != d.stat.Ino ||
			st.Mode != d.stat.Mode || st.Uid != d.stat.Uid || held.Dev != st.Dev || held.Ino != st.Ino ||
			d.stable && (!sharedSame(d.stat, st) || !sharedSame(d.stat, held)) {
			return fmt.Errorf("catalog directory custody changed: %s", path)
		}
	}
	return nil
}
func (c *snapshotCustody) file(path string, spec snapshotSpec, private string) ([]byte, error) {
	if err := c.directory(filepath.Dir(path), private, false); err != nil {
		return nil, err
	}
	// The seam holds the file and procfs, independently rereads and binds identity.
	raw, verify, closeFile, err := acquireSharedFile(path, spec.limit, nil)
	if err != nil {
		return nil, err
	}
	c.closes = append(c.closes, closeFile)
	c.checks = append(c.checks, verify)
	fd, err := sharedHeld(path, false)
	if err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	err = syscall.Fstat(fd, &st)
	syscall.Close(fd)
	if err != nil || st.Uid != uint32(os.Getuid()) || st.Mode&07777 != spec.mode || st.Nlink != 1 {
		return nil, errors.New("catalog exact file mode/owner/links")
	}
	if err := verify(); err != nil {
		return nil, err
	}
	return raw, nil
}

// Read directory names through the held descriptor. Enumeration never follows
// an ambient path, and the acquired directory is rechecked around the read.
func (c *snapshotCustody) names(path string) ([]string, error) {
	d := c.dirs[path]
	if d == nil {
		return nil, errors.New("catalog directory not held")
	}
	fd, err := syscall.Open("/proc/self/fd/"+strconv.Itoa(d.fd), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "catalog held directory")
	defer f.Close()
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || !sharedSame(st, d.stat) {
		return nil, errors.New("catalog directory enumeration binding")
	}
	// The frozen tree has fewer than 64 children in any directory.
	names, err := f.Readdirnames(65)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > 64 || syscall.Fstat(fd, &st) != nil || !sharedSame(st, d.stat) {
		return nil, errors.New("catalog directory enumeration bound/change")
	}
	sort.Strings(names)
	return names, nil
}

// Closed owning subtrees only: unrelated repository/descriptor paths are allowed.
func (c *snapshotCustody) closure(root string, specs map[string]snapshotSpec, prefix string, private bool) error {
	children := map[string]map[string]bool{"": {}}
	for path := range specs {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		relative := strings.TrimPrefix(path, prefix)
		parts := strings.Split(relative, "/")
		parent := ""
		for i, part := range parts {
			if children[parent] == nil {
				children[parent] = map[string]bool{}
			}
			children[parent][part] = true
			if i < len(parts)-1 {
				parent = filepath.Join(parent, part)
			}
		}
	}
	dirs := make([]string, 0, len(children))
	for d := range children {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, relative := range dirs {
		path := filepath.Join(root, relative)
		privateRoot := ""
		if private {
			privateRoot = root
		}
		if err := c.directory(path, privateRoot, true); err != nil {
			return err
		}
		actual, err := c.names(path)
		if err != nil {
			return err
		}
		want := make([]string, 0, len(children[relative]))
		for n := range children[relative] {
			want = append(want, n)
		}
		sort.Strings(want)
		if !catalogEqual(actual, want) {
			return fmt.Errorf("catalog closed directory inventory: %s", path)
		}
	}
	return nil
}

type snapshotOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *snapshotOutput) Len() int      { return b.buffer.Len() }
func (b *snapshotOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *snapshotOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("catalog Git output bound")
	}
	return b.buffer.Write(p)
}

// No ambient Git environment, config includes, replacements, attributes,
// archive conversion, hooks, checkout, fetch or executable source is used.
func snapshotGit(ctx context.Context, repository string, limit int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := []string{"--no-replace-objects", "--literal-pathspecs", "--git-dir=" + repository + "/.git",
		"-c", "core.useReplaceRefs=false", "-c", "core.fsmonitor=false"}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "/usr/bin/git", argv...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}
	out, stderr := &snapshotOutput{limit: limit}, &snapshotOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("catalog Git acquisition: %w", err)
	}
	if stderr.Len() != 0 {
		return nil, errors.New("catalog Git unexpected stderr")
	}
	return bytes.Clone(out.Bytes()), nil
}

// Git SHA1 object IDs bind the typed header and exact bytes, independently of
// filenames in local object storage. Caller SHA256 pins remain integrity only.
func snapshotObjectID(kind string, raw []byte) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "%s %d%c", kind, len(raw), 0)
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

func snapshotTreeObjects(ctx context.Context, o CatalogSnapshotOptions, specs map[string]snapshotSpec) error {
	parents := map[string]bool{"": true}
	for path := range specs {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			parents[parent] = true
		}
	}
	paths := make([]string, 0, len(parents))
	for path := range parents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var total int64
	for _, path := range paths {
		id := o.Tree
		if path != "" {
			raw, e := snapshotGit(ctx, o.Repository, 128, "rev-parse", "--verify", "--end-of-options", o.Tree+":"+path)
			if e != nil {
				return e
			}
			id = strings.TrimSuffix(string(raw), "\n")
		}
		if !sharedHex(id, 40) {
			return errors.New("catalog Git tree object ID")
		}
		raw, e := snapshotGit(ctx, o.Repository, 16<<20, "cat-file", "tree", id)
		if e != nil || snapshotObjectID("tree", raw) != id {
			return errors.New("catalog Git tree object hash")
		}
		total += int64(len(raw))
		if total > 64<<20 {
			return errors.New("catalog Git tree aggregate")
		}
	}
	return nil
}

func snapshotGitIdentity(ctx context.Context, o CatalogSnapshotOptions) error {
	config, err := snapshotGit(ctx, o.Repository, 65536, "config", "--local", "--no-includes", "--list", "-z")
	if err != nil {
		return err
	}
	originCount := 0
	for _, entry := range strings.Split(string(config), "\x00") {
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "\n")
		if !ok {
			return errors.New("catalog Git config encoding")
		}
		key = strings.ToLower(key)
		if strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "includeif.") || strings.HasPrefix(key, "extensions.") ||
			key == "core.worktree" || key == "core.repositoryformatversion" && value != "0" {
			return errors.New("catalog Git indirect/extended configuration")
		}
		if key == "remote.origin.url" || key == "remote.origin.pushurl" {
			if value != "https://github.com/iryzhkov/UpKeeper.git" && value != "git@github.com:iryzhkov/UpKeeper.git" {
				return errors.New("catalog Git origin identity")
			}
			if key == "remote.origin.url" {
				originCount++
			}
		}
	}
	if originCount != 1 {
		return errors.New("catalog Git exact origin")
	}
	for _, path := range []string{".git/commondir", ".git/objects/info/alternates", ".git/info/grafts"} {
		if _, err := os.Lstat(filepath.Join(o.Repository, path)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("catalog Git indirect object storage")
		}
	}
	refs, err := snapshotGit(ctx, o.Repository, 65536, "for-each-ref", "--format=%(refname)", "refs/replace/")
	if err != nil || len(refs) != 0 {
		return errors.New("catalog Git replacement refs")
	}
	commitBytes, err := snapshotGit(ctx, o.Repository, 262144, "cat-file", "commit", o.Commit)
	if err != nil || snapshotObjectID("commit", commitBytes) != o.Commit || !bytes.HasPrefix(commitBytes, []byte("tree "+o.Tree+"\n")) {
		return errors.New("catalog Git commit object hash/tree binding")
	}
	typ, err := snapshotGit(ctx, o.Repository, 128, "cat-file", "-t", o.Commit)
	if err != nil || string(typ) != "commit\n" {
		return errors.New("catalog Git full commit")
	}
	commit, err := snapshotGit(ctx, o.Repository, 128, "rev-parse", "--verify", "--end-of-options", o.Commit+"^{commit}")
	if err != nil || string(commit) != o.Commit+"\n" {
		return errors.New("catalog Git selected commit")
	}
	tree, err := snapshotGit(ctx, o.Repository, 128, "rev-parse", "--verify", "--end-of-options", o.Commit+"^{tree}")
	if err != nil || string(tree) != o.Tree+"\n" {
		return errors.New("catalog Git selected tree")
	}
	return nil
}
func snapshotBlob(ctx context.Context, o CatalogSnapshotOptions, path string, spec snapshotSpec) ([]byte, error) {
	record, err := snapshotGit(ctx, o.Repository, 1024, "ls-tree", "-z", o.Tree, "--", path)
	if err != nil {
		return nil, err
	}
	mode := "100644"
	if spec.mode == 0755 {
		mode = "100755"
	}
	// Exactly one selected blob with the exact path; symlinks/gitlinks refuse.
	fields := strings.Split(string(record), "\t")
	if len(fields) != 2 || fields[1] != path+"\x00" {
		return nil, errors.New("catalog selected Git path")
	}
	head := strings.Split(fields[0], " ")
	if len(head) != 3 || head[0] != mode || head[1] != "blob" || !sharedHex(head[2], 40) {
		return nil, errors.New("catalog selected Git mode/object")
	}
	size, err := snapshotGit(ctx, o.Repository, 128, "cat-file", "-s", head[2])
	n, e := strconv.ParseInt(strings.TrimSuffix(string(size), "\n"), 10, 64)
	if err != nil || e != nil || n <= 0 || n > spec.limit {
		return nil, errors.New("catalog Git blob size")
	}
	blob, err := snapshotGit(ctx, o.Repository, int(spec.limit), "cat-file", "blob", head[2])
	if err != nil || int64(len(blob)) != n || snapshotObjectID("blob", blob) != head[2] {
		return nil, errors.New("catalog Git blob bound")
	}
	return blob, nil
}
func snapshotGitClosure(ctx context.Context, o CatalogSnapshotOptions, specs map[string]snapshotSpec) error {
	for _, prefix := range []string{SharedDirectory + "/", "shared/catalog-guidance/"} {
		raw, err := snapshotGit(ctx, o.Repository, 65536, "ls-tree", "-r", "--name-only", "-z", o.Tree, "--", strings.TrimSuffix(prefix, "/"))
		if err != nil {
			return err
		}
		want := []string{}
		for _, path := range snapshotPaths(specs) {
			if strings.HasPrefix(path, prefix) {
				want = append(want, path)
			}
		}
		actual := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if !catalogEqual(actual, want) {
			return errors.New("catalog Git owning subtree closure")
		}
	}
	return nil
}

func snapshotImportedOwner(raw []byte) error {
	if err := ValidateSharedOwnerEncoding(raw); err != nil {
		return err
	}
	var o SharedOwner
	if err := json.Unmarshal(raw, &o); err != nil {
		return err
	}
	instructions := PolicyInstructionPaths()
	sort.Strings(instructions)
	if o.SchemaVersion != 2 || o.Owner != "upkeeper" || o.State != "candidate-inert" ||
		o.SourceRepository != "github.com/iryzhkov/UpKeeper" || o.LegacyRepository != "github.com/iryzhkov/omarchy-setup" ||
		o.LegacyCommit != sharedUpstreamCommit || o.LegacyTree != sharedUpstreamTree || o.Generator != Generator ||
		o.PolicyInputContract != sharedInputContract || !catalogEqual(o.SourceFiles, SharedSourcePaths()) ||
		!catalogEqual(o.InstructionFiles, instructions) ||
		!catalogEqual(o.SkillRoots, []string{".agents/skills", ".claude/skills"}) ||
		!catalogEqual(o.FutureMerges, []string{".claude/CLAUDE.md:omarchy-setup import fence", ".config/opencode/opencode.json:instructions"}) ||
		o.RetiredReference != ".config/agents/agent99.md" {
		return errors.New("catalog imported owner identity/inventory")
	}
	return nil
}

func snapshotInputEncoding(path string, raw []byte) error {
	if err := snapshotCanonical(raw); err != nil {
		return err
	}
	v, err := canonjson.DecodeObject(raw)
	if err != nil {
		return err
	}
	if path == "inputs/route-policy.json" {
		if v["schema"] != "route-policy/v1" || v["schema_version"] != nil {
			return errors.New("catalog historical policy kind")
		}
		return nil
	}
	version := json.Number("1")
	if path == "inputs/guidance-layout.json" {
		version = json.Number("4")
	}
	if v["schema_version"] != version {
		return errors.New("catalog input integer branch version")
	}
	schemas := map[string]string{
		"inputs/guidance-profile.json":       "guidance-profile/v1",
		"inputs/guidance-observation.json":   "guidance-observation/v1",
		"inputs/required-global-policy.json": "required-global-policy/v1",
		"inputs/tool-guidance-manifest.json": "tool-guidance-manifest/v1",
	}
	if strings.HasPrefix(path, "inputs/tool-guidance/") {
		schemas[path] = "tool-guidance/v1"
	}
	if want := schemas[path]; want != "" && v["schema"] != want {
		return errors.New("catalog input kind")
	}
	return nil
}

func acquireCatalogSnapshot(ctx context.Context, o CatalogSnapshotOptions, sources, inputs map[string]snapshotSpec) (*CatalogSnapshot, error) {
	return acquireCatalogSnapshotHeld(ctx, o, sources, inputs, nil)
}

// afterReads is a package-private deterministic fault seam, never a caller grant.
func acquireCatalogSnapshotHeld(ctx context.Context, o CatalogSnapshotOptions, sources, inputs map[string]snapshotSpec, afterReads func()) (_ *CatalogSnapshot, err error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	c := &snapshotCustody{dirs: map[string]*snapshotDirectory{}}
	defer func() {
		if err != nil {
			err = errors.Join(err, c.release())
		}
	}()
	if err = c.directory(o.Repository, "", false); err != nil {
		return nil, err
	}
	if err = c.directory(o.Repository+"/.git/objects", "", false); err != nil {
		return nil, err
	}
	// Config and HEAD are held as Git identity evidence, not selected source bytes.
	for _, path := range []string{".git/config", ".git/HEAD"} {
		raw, verify, closeFile, e := acquireSharedFile(filepath.Join(o.Repository, path), 65536, nil)
		if e != nil {
			return nil, e
		}
		_ = raw
		c.checks = append(c.checks, verify)
		c.closes = append(c.closes, closeFile)
	}
	if err = c.closure(o.InputRoot, inputs, "", true); err != nil {
		return nil, err
	}
	for _, prefix := range []string{SharedDirectory + "/", "shared/catalog-guidance/"} {
		if err = c.closure(filepath.Join(o.Repository, strings.TrimSuffix(prefix, "/")), sources, prefix, false); err != nil {
			return nil, err
		}
	}
	repo, input := c.dirs[o.Repository].stat, c.dirs[o.InputRoot].stat
	if repo.Dev == input.Dev && repo.Ino == input.Ino {
		return nil, errors.New("catalog aliased roots")
	}
	if err = snapshotGitIdentity(ctx, o); err != nil {
		return nil, err
	}
	if err = snapshotTreeObjects(ctx, o, sources); err != nil {
		return nil, err
	}
	if err = snapshotGitClosure(ctx, o, sources); err != nil {
		return nil, err
	}
	s := &CatalogSnapshot{sources: map[string][]byte{}, inputs: map[string][]byte{}, commit: o.Commit, tree: o.Tree}
	var total int64
	for _, path := range snapshotPaths(sources) {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		spec := sources[path]
		raw, e := c.file(filepath.Join(o.Repository, path), spec, "")
		if e != nil {
			return nil, e
		}
		total += int64(len(raw))
		if total > 64<<20 {
			return nil, errors.New("catalog source aggregate")
		}
		if digestOf(raw) != o.SourceSHA256[path] {
			return nil, fmt.Errorf("catalog source pin: %s", path)
		}
		blob, e := snapshotBlob(ctx, o, path, spec)
		if e != nil || !bytes.Equal(raw, blob) {
			return nil, fmt.Errorf("catalog Git/physical source parity: %s: %w", path, e)
		}
		if strings.HasPrefix(path, "shared/catalog-guidance/guidance/") || strings.HasPrefix(path, "internal/toolproducer/testdata/descriptors/") || path == "shared/catalog-guidance/templates/discovery.txt" {
			inputPath := "inputs/" + strings.TrimPrefix(path, "shared/catalog-guidance/guidance/")
			switch path {
			case "shared/catalog-guidance/templates/discovery.txt":
				inputPath = "inputs/guidance-layout.json"
			case "shared/catalog-guidance/guidance/manifest.json":
				inputPath = "inputs/tool-guidance-manifest.json"
			}
			if strings.HasPrefix(path, "internal/toolproducer/testdata/descriptors/") {
				inputPath = "inputs/descriptor.json"
			}
			if strings.Contains(path, "/guidance/") && inputPath != "inputs/required-global-policy.json" && inputPath != "inputs/tool-guidance-manifest.json" {
				inputPath = "inputs/tool-guidance/" + filepath.Base(path)
			}
			if e := snapshotInputEncoding(inputPath, blob); e != nil {
				return nil, e
			}
		}
		s.sources[path] = bytes.Clone(blob)
	}
	if _, err = ParseCatalogDocumentEncoding(CatalogOwner5, s.sources["shared/catalog-guidance/owner.json"]); err != nil {
		return nil, err
	}
	if err = snapshotImportedOwner(s.sources[SharedDirectory+"/owner.json"]); err != nil {
		return nil, err
	}
	total = 0
	for _, path := range snapshotPaths(inputs) {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		raw, e := c.file(filepath.Join(o.InputRoot, path), inputs[path], o.InputRoot)
		if e != nil {
			return nil, e
		}
		total += int64(len(raw))
		if total > 13*262144 {
			return nil, errors.New("catalog input aggregate")
		}
		if digestOf(raw) != o.InputSHA256[path] {
			return nil, fmt.Errorf("catalog input pin: %s", path)
		}
		if err = snapshotInputEncoding(path, raw); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		s.inputs[path] = bytes.Clone(raw)
	}
	if afterReads != nil {
		afterReads()
	}
	// Recheck both physical closure and every held resource after all reads.
	// Each call derives its Git bound from the context it is given, so a
	// shorter caller deadline prevails and acquisition ctx is never retained.
	// Git helpers may replace a cancelled command's error with an integrity
	// error; catalogContextErr restores it after the command has been reaped.
	verify := func(verifyCtx context.Context) error {
		if e := verifyCtx.Err(); e != nil {
			return e
		}
		if e := c.verifyDirectories(); e != nil {
			return e
		}
		for _, check := range c.checks {
			if e := check(); e != nil {
				return e
			}
		}
		if e := verifyCtx.Err(); e != nil {
			return e
		}
		gitCtx, cancel := context.WithTimeout(verifyCtx, 30*time.Second)
		defer cancel()
		if e := snapshotGitIdentity(gitCtx, o); e != nil {
			return catalogContextErr(gitCtx, e)
		}
		if e := snapshotTreeObjects(gitCtx, o, sources); e != nil {
			return catalogContextErr(gitCtx, e)
		}
		if e := snapshotGitClosure(gitCtx, o, sources); e != nil {
			return catalogContextErr(gitCtx, e)
		}
		for _, path := range snapshotPaths(sources) {
			if e := gitCtx.Err(); e != nil {
				return e
			}
			blob, e := snapshotBlob(gitCtx, o, path, sources[path])
			if e != nil || !bytes.Equal(blob, s.sources[path]) {
				return catalogContextErr(gitCtx, errors.New("catalog selected Git bytes changed"))
			}
		}
		if e := verifyCtx.Err(); e != nil {
			return e
		}
		for _, check := range c.checks {
			if e := check(); e != nil {
				return e
			}
		}
		if e := c.verifyDirectories(); e != nil {
			return e
		}
		return verifyCtx.Err()
	}
	s.verify = verify
	s.close = c.release
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = verify(ctx); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return s, nil
}
