package toolproducer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/toolcatalog"
)

// Exactly ten sorted owning implementation files; tests and generated receipts
// are excluded, so the caller's immutable producer commit is noncircular.
var producerClosure = []string{
	"go.mod", "go.sum", "internal/canonjson/canonjson.go",
	"internal/toolcatalog/catalog.go", "internal/toolcatalog/inspect_linux.go",
	"internal/toolcatalog/inspect_other.go", "internal/toolproducer/compile.go",
	"internal/toolproducer/files_linux.go", "internal/toolproducer/files_other.go",
	"internal/toolproducer/schema.go",
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, refuse("git-output-limit")
	}
	return b.Buffer.Write(p)
}
func git(repo string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := append([]string{"--no-replace-objects", "-C", repo}, args...)
	c := exec.CommandContext(ctx, "/usr/bin/git", argv...)
	// No host config, PATH lookup, replacement objects or caller Git environment.
	c.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	out := &cappedBuffer{limit: fileLimit + 1024}
	errout := &cappedBuffer{limit: 4096}
	c.Stdout = out
	c.Stderr = errout
	if c.Run() != nil {
		return nil, refuse("git-object")
	}
	return out.Bytes(), nil
}
func gitText(repo string, args ...string) (string, error) {
	b, e := git(repo, args...)
	return strings.TrimSuffix(string(b), "\n"), e
}
func gitTree(repo, commit string) (string, error) {
	if !toolcatalog.Pin(commit, 40) {
		return "", refuse("git-commit")
	}
	got, e := gitText(repo, "rev-parse", "--verify", commit+"^{commit}")
	if e != nil || got != commit {
		return "", refuse("git-commit")
	}
	tree, e := gitText(repo, "rev-parse", "--verify", commit+"^{tree}")
	if e != nil || !toolcatalog.Pin(tree, 40) {
		return "", refuse("git-tree")
	}
	return tree, nil
}
func gitFile(repo, tree string, f sourceFile) ([]byte, error) {
	if !relative(f.Path) {
		return nil, refuse("source-path")
	}
	b, e := git(repo, "ls-tree", "-z", tree, "--", f.Path)
	if e != nil {
		return nil, e
	}
	fields := strings.Split(string(b), "\t")
	if len(fields) != 2 || fields[1] != f.Path+"\x00" {
		return nil, refuse("git-entry")
	}
	entry := strings.Split(fields[0], " ")
	mode := "100644"
	if f.Mode == 493 {
		mode = "100755"
	}
	if len(entry) != 3 || entry[0] != mode || entry[1] != "blob" || !toolcatalog.Pin(entry[2], 40) {
		return nil, refuse("git-mode")
	}
	size, e := gitText(repo, "cat-file", "-s", entry[2])
	if e != nil {
		return nil, e
	}
	var n int64
	if _, e = fmt.Sscan(size, &n); e != nil || n < 1 || n > fileLimit {
		return nil, refuse("git-blob-size")
	}
	raw, e := git(repo, "cat-file", "blob", entry[2])
	if e != nil || int64(len(raw)) != n || digest(raw) != f.SHA256 {
		return nil, refuse("git-blob-pin")
	}
	return raw, nil
}
func physicalGitFile(p *physical, repo, tree string, f sourceFile) ([]byte, error) {
	raw, e := gitFile(repo, tree, f)
	if e != nil {
		return nil, e
	}
	current, mode, e := p.file(filepath.Join(repo, filepath.FromSlash(f.Path)), fileLimit)
	if e != nil || mode != f.Mode || !bytes.Equal(raw, current) {
		return nil, refuse("source-snapshot")
	}
	return raw, nil
}
func origin(repo, want string) error {
	// Isolated checkouts may fetch from a local cache while retaining the
	// owning origin as their configured push identity. Neither is authentication.
	for _, args := range [][]string{{"remote", "get-url", "origin"}, {"remote", "get-url", "--push", "origin"}} {
		got, e := gitText(repo, args...)
		if strings.HasPrefix(got, "git@github.com:") {
			got = "https://github.com/" + strings.TrimPrefix(got, "git@github.com:")
		}
		if e == nil && strings.TrimSuffix(got, ".git") == want {
			return nil
		}
	}
	return refuse("git-origin")
}
func producer(p *physical, in Inputs) (producerSource, error) {
	out := producerSource{Repository: "https://github.com/iryzhkov/UpKeeper", Commit: in.ProducerCommit, Files: []sourceFile{}}
	if e := p.root(in.ProducerRepository); e != nil {
		return out, e
	}
	if e := origin(in.ProducerRepository, out.Repository); e != nil {
		return out, e
	}
	tree, e := gitTree(in.ProducerRepository, in.ProducerCommit)
	if e != nil {
		return out, e
	}
	out.Tree = tree
	var total int
	for _, path := range producerClosure {
		raw, mode, e := p.file(filepath.Join(in.ProducerRepository, filepath.FromSlash(path)), fileLimit)
		if e != nil {
			return out, e
		}
		total += len(raw)
		if total > aggregateLimit {
			return out, refuse("producer-size")
		}
		f := sourceFile{Path: path, Mode: mode, SHA256: digest(raw)}
		blob, e := gitFile(in.ProducerRepository, tree, f)
		if e != nil || !bytes.Equal(raw, blob) {
			return out, refuse("producer-source")
		}
		out.Files = append(out.Files, f)
	}
	return out, nil
}

var vocabulary = map[string]struct {
	kind, route, purpose string
	caps                 []string
}{
	"huyang":              {"mcp-only", "mcp", "Repository source operations declaration.", []string{"file-read", "file-search", "repository-operations", "transactional-edit"}},
	"fixture-cli":         {"cli", "cli", "Synthetic CLI inspection declaration.", []string{"fixture-inspect"}},
	"fixture-mcp":         {"mcp-only", "mcp", "Synthetic MCP metadata declaration.", []string{"fixture-metadata"}},
	"fixture-third-party": {"third-party", "external", "Synthetic third-party reference declaration.", []string{"fixture-reference"}},
}

func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
func validateTool(t toolcatalog.Tool, names map[string]bool) error {
	v, ok := vocabulary[t.ID]
	if !ok || t.CanonicalTool != t.ID || t.Kind != v.kind || t.PrimaryRoute.Kind != v.route || t.PrimaryRoute.Name != t.ID || t.Purpose != v.purpose {
		return refuse("vocabulary")
	}
	for _, a := range [][]string{t.Aliases, t.Capabilities, t.ArtifactIDs, t.Platforms} {
		if !sortedStrings(a) {
			return refuse("list-order")
		}
	}
	for _, c := range t.Capabilities {
		if !contains(v.caps, c) {
			return refuse("vocabulary")
		}
	}
	for _, a := range append([]string{t.ID}, t.Aliases...) {
		if !idRE.MatchString(a) || a == "agent99" || names[a] {
			return refuse("alias")
		}
		if _, reserved := vocabulary[a]; reserved && a != t.ID {
			return refuse("alias-route")
		}
		names[a] = true
	}
	return nil
}

// Compile never captures installed state. It returns inert declarations only
// after authentic local Git correspondence and unchanged inspector verification.
func Compile(in Inputs) (Result, error) { return compile(in, nil) }
func compile(in Inputs, configure func(*physical)) (Result, error) {
	var zero Result
	p, e := newPhysical()
	if e != nil {
		return zero, e
	}
	defer p.close()
	if configure != nil {
		configure(p)
	}
	fail := func(e error) (Result, error) {
		if e == nil {
			e = refuse("validation")
		}
		return zero, e
	}
	if !toolcatalog.Pin(in.ExpectedInputSHA256, 64) || !toolcatalog.Pin(in.ExpectedManifestSHA256, 64) {
		return fail(refuse("arguments"))
	}
	for _, root := range []string{in.DescriptorRoot, in.ArtifactRoot} {
		if e = p.root(root); e != nil {
			return fail(e)
		}
	}
	raw, _, e := p.file(in.InputPath, metadataLimit)
	if e != nil || digest(raw) != in.ExpectedInputSHA256 {
		return fail(refuse("input-pin"))
	}
	var input inputDocument
	if e = decode(raw, "producer_input_v1", &input); e != nil {
		return fail(e)
	}
	mr, _, e := p.file(in.ManifestPath, metadataLimit)
	if e != nil || digest(mr) != in.ExpectedManifestSHA256 || input.ManifestSHA256 != in.ExpectedManifestSHA256 {
		return fail(refuse("manifest-pin"))
	}
	var m manifest
	if e = decode(mr, "artifact_manifest_v1", &m); e != nil {
		return fail(e)
	}
	if !sortedStrings(input.ToolIDs) || !sorted(input.Descriptors, func(d descriptorPin) string { return d.ID }) || !sorted(m.Sources, func(s source) string { return s.ID }) || !sorted(m.Artifacts, func(a artifact) string { return a.ID }) {
		return fail(refuse("inventory-order"))
	}
	if len(input.ToolIDs) != len(input.Descriptors) || len(in.SourceRepositories) != len(m.Sources) {
		return fail(refuse("inventory"))
	}
	sourceFiles := map[string]sourceFile{}
	sourceBytes := map[string][]byte{}
	usedFiles := map[string]bool{}
	seenSources := map[string]bool{}
	total, files := 0, 0
	for _, s := range m.Sources {
		repo, ok := in.SourceRepositories[s.ID]
		if !ok || !repository(s.Repository) {
			return fail(refuse("source-repository"))
		}
		seenSources[s.ID] = true
		if e = p.root(repo); e != nil {
			return fail(e)
		}
		if e = origin(repo, s.Repository); e != nil {
			return fail(e)
		}
		tree, e := gitTree(repo, s.Commit)
		if e != nil || tree != s.Tree {
			return fail(refuse("source-tree"))
		}
		if !sorted(s.Files, func(f sourceFile) string { return f.Path }) {
			return fail(refuse("source-file-order"))
		}
		for _, f := range s.Files {
			if !relative(f.Path) {
				return fail(refuse("source-path"))
			}
			b, e := physicalGitFile(p, repo, tree, f)
			if e != nil {
				return fail(e)
			}
			total += len(b)
			files++
			if total > aggregateLimit || files > 128 {
				return fail(refuse("source-size"))
			}
			key := s.ID + "/" + f.Path
			sourceFiles[key] = f
			sourceBytes[key] = b
		}
	}
	for id := range in.SourceRepositories {
		if !seenSources[id] {
			return fail(refuse("source-extra"))
		}
	}
	tools := []toolcatalog.Tool{}
	names := map[string]bool{}
	descriptorPaths := map[string]bool{}
	for i, d := range input.Descriptors {
		if d.ID != input.ToolIDs[i] || !relative(d.Path) || !relative(d.SourcePath) || descriptorPaths[d.Path] {
			return fail(refuse("descriptor-path"))
		}
		descriptorPaths[d.Path] = true
		key := d.SourceID + "/" + d.SourcePath
		f, ok := sourceFiles[key]
		if !ok || f.SHA256 != d.SHA256 {
			return fail(refuse("descriptor-source"))
		}
		usedFiles[key] = true
		b, mode, e := p.file(filepath.Join(in.DescriptorRoot, filepath.FromSlash(d.Path)), metadataLimit)
		if e != nil || mode != f.Mode || digest(b) != d.SHA256 || !bytes.Equal(b, sourceBytes[key]) {
			return fail(refuse("descriptor-binding"))
		}
		var desc descriptor
		if e = decode(b, "descriptor_v1", &desc); e != nil {
			return fail(e)
		}
		if desc.Tool.ID != d.ID {
			return fail(refuse("descriptor-id"))
		}
		if e = validateTool(desc.Tool, names); e != nil {
			return fail(e)
		}
		tools = append(tools, desc.Tool)
	}
	owners := map[string]toolcatalog.Tool{}
	for _, t := range tools {
		for _, id := range t.ArtifactIDs {
			if _, ok := owners[id]; ok {
				return fail(refuse("artifact-owner"))
			}
			owners[id] = t
		}
	}
	if len(owners) != len(m.Artifacts) {
		return fail(refuse("artifact-inventory"))
	}
	decisions := []decision{}
	selected := map[string]bool{}
	missing := map[string]bool{}
	paths := map[string]bool{}
	artifactTotal := 0
	missingPaths := []string{}
	for _, a := range m.Artifacts {
		owner, ok := owners[a.ID]
		if !ok || owner.Required != a.Required || !relative(a.Path) || !relative(a.SourcePath) || paths[a.Path] {
			return fail(refuse("artifact-selection"))
		}
		paths[a.Path] = true
		if a.Transform != "identity-v1" {
			return fail(refuse("mapping-transform-unsupported"))
		}
		key := a.SourceID + "/" + a.SourcePath
		f, ok := sourceFiles[key]
		if !ok || f.SHA256 != a.SHA256 || f.Mode != a.Mode {
			return fail(refuse("identity-mapping"))
		}
		usedFiles[key] = true
		b, mode, e := p.file(filepath.Join(in.ArtifactRoot, filepath.FromSlash(a.Path)), fileLimit)
		if e != nil {
			if errors.Is(e, os.ErrNotExist) && !a.Required {
				missing[owner.ID] = true
				missingPaths = append(missingPaths, filepath.Join(in.ArtifactRoot, filepath.FromSlash(a.Path)))
				continue
			}
			return fail(refuse("artifact-physical"))
		}
		if mode != a.Mode || !bytes.Equal(b, sourceBytes[key]) {
			return fail(refuse("artifact-integrity"))
		}
		artifactTotal += len(b)
		if artifactTotal > aggregateLimit {
			return fail(refuse("artifact-size"))
		}
	}
	if len(usedFiles) != len(sourceFiles) {
		return fail(refuse("source-unexplained"))
	}
	c := catalog{SchemaVersion: 1, State: "candidate-inert", Sources: []toolcatalog.Source{}, Artifacts: []toolcatalog.Artifact{}, Tools: []toolcatalog.Tool{}}
	for _, t := range tools {
		result := "verified"
		if !contains(t.Platforms, input.Platform) {
			if t.Required {
				return fail(refuse("required-incompatible"))
			}
			result = "optional-incompatible"
		} else {
			selected[t.ID] = true
			c.Tools = append(c.Tools, t)
			if missing[t.ID] {
				result = "optional-missing"
			}
		}
		decisions = append(decisions, decision{ToolID: t.ID, Result: result})
	}
	usedSources := map[string]bool{}
	for _, a := range m.Artifacts {
		if selected[owners[a.ID].ID] {
			usedSources[a.SourceID] = true
			c.Artifacts = append(c.Artifacts, toolcatalog.Artifact{ID: a.ID, SourceID: a.SourceID, Path: a.Path, SHA256: a.SHA256, Mode: a.Mode, Required: a.Required})
		}
	}
	for _, s := range m.Sources {
		if usedSources[s.ID] {
			c.Sources = append(c.Sources, toolcatalog.Source{ID: s.ID, Repository: s.Repository, RevisionKind: "git-sha1", Revision: s.Commit})
		}
	}
	ps, e := producer(p, in)
	if e != nil {
		return fail(e)
	}
	cb, e := canonjson.MarshalFile(c)
	if e != nil || len(cb) > metadataLimit {
		return fail(refuse("catalog-size"))
	}
	r := receipt{SchemaVersion: 1, State: "candidate-inert", ProducerSource: ps, InputSHA256: in.ExpectedInputSHA256, ManifestSHA256: in.ExpectedManifestSHA256, Descriptors: input.Descriptors, Sources: m.Sources, Artifacts: m.Artifacts, CatalogSHA256: digest(cb), Platform: input.Platform, Decisions: decisions}
	rb, e := canonjson.MarshalFile(r)
	if e != nil || len(rb) > metadataLimit {
		return fail(refuse("receipt-size"))
	}
	var rr receipt
	if e = decode(rb, "producer_receipt_v1", &rr); e != nil {
		return fail(e)
	}
	scratch, e := os.MkdirTemp("", "upkeeper-toolproducer-")
	if e != nil {
		return fail(e)
	}
	defer os.RemoveAll(scratch)
	metadata := filepath.Join(scratch, "catalog.json")
	if e = os.WriteFile(metadata, cb, 0644); e != nil {
		return fail(e)
	}
	if _, e = toolcatalog.Inspect(toolcatalog.Options{Metadata: metadata, Root: in.ArtifactRoot, SHA256: digest(cb), Platform: input.Platform}); e != nil {
		return fail(e)
	}
	if e = p.check(); e != nil {
		return fail(e)
	}
	for _, path := range missingPaths {
		if _, _, e = p.file(path, fileLimit); !errors.Is(e, os.ErrNotExist) {
			return fail(refuse("optional-missing-binding"))
		}
	}
	return Result{CatalogBytes: cb, ReceiptBytes: rb, CatalogSHA256: digest(cb), ReceiptSHA256: digest(rb)}, nil
}
