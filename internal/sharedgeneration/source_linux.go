//go:build linux

package sharedgeneration

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/author"
)

// The only production caller receives p directly from PrepareShared; this
// helper is private so caller-supplied hashes cannot authenticate a Stage.
func observePreparation(x *filesystem, p *author.SharedPreparation) (Receipt, map[string][]byte, error) {
	if p == nil || !pin(p.Commit, 40) || p.Repository != "github.com/iryzhkov/UpKeeper" {
		return Receipt{}, nil, errors.New("prepared identity")
	}
	source := filepath.Dir(filepath.Dir(filepath.Dir(p.Sources.SkillsRoot)))
	if p.Sources.SkillsRoot != filepath.Join(source, "config/claude/skills") {
		return Receipt{}, nil, errors.New("prepared source path")
	}
	fd, e := absolute(source, true)
	if e != nil {
		return Receipt{}, nil, e
	}
	defer syscall.Close(fd)
	ob, _, e := x.read(fd, "owner.json", metadataLimit, 0644)
	if e != nil {
		return Receipt{}, nil, e
	}
	var owner author.SharedOwner
	if e = strictJSON(ob, &owner, false); e != nil {
		return Receipt{}, nil, e
	}
	if !reflect.DeepEqual(owner, p.Owner) || (owner.SchemaVersion != 1 && owner.SchemaVersion != 2) || owner.Owner != "upkeeper" || owner.State != "candidate-inert" ||
		owner.SourceRepository != p.Repository || owner.Generator != author.Generator {
		return Receipt{}, nil, errors.New("prepared owner")
	}
	if len(owner.SourceFiles) == 0 || len(owner.SourceFiles) > 1023 || len(owner.InstructionFiles) == 0 || len(owner.InstructionFiles) > 1024 {
		return Receipt{}, nil, errors.New("owner inventory limit")
	}
	if owner.SchemaVersion == 2 && !reflect.DeepEqual(owner.SourceFiles, author.SharedSourcePaths()) {
		return Receipt{}, nil, errors.New("v2 source closure")
	}
	expected := map[string]uint32{"owner.json": 0644}
	prev := ""
	for _, path := range owner.SourceFiles {
		if !validPath(path) || path <= prev || path == "owner.json" {
			return Receipt{}, nil, errors.New("owner source inventory")
		}
		prev = path
		mode := uint32(0644)
		if path == author.Generator || owner.SchemaVersion == 2 && path == "config/bin/agents-role-project" {
			mode = 0755
		}
		expected[path] = mode
	}
	data, e := x.treeMode(fd, expected, true)
	if e != nil {
		return Receipt{}, nil, e
	}
	if !bytes.Equal(data["owner.json"], ob) {
		return Receipt{}, nil, errors.New("owner changed")
	}
	hashes := map[string]string{}
	for path, b := range data {
		hashes[path] = digest(b)
	}
	if !reflect.DeepEqual(hashes, p.SourceSHA256) {
		return Receipt{}, nil, errors.New("observed source hashes differ")
	}
	r := Receipt{SchemaVersion: owner.SchemaVersion, State: "candidate-inert", Owner: "upkeeper", SourceRepository: p.Repository, SourceCommit: p.Commit, OwnerDescriptorSHA256: digest(ob), SourceSHA256: hashes, Files: []File{}, Policy: p.Policy}
	if p.Policy != nil {
		policy := *p.Policy
		r.Policy = &policy
	}
	payload := map[string][]byte{}
	if len(p.Sources.Instructions) != len(owner.InstructionFiles) || len(p.OutputSHA256) != len(owner.InstructionFiles) {
		return Receipt{}, nil, errors.New("generated inventory")
	}
	prev = ""
	total := int64(0)
	for _, dest := range owner.InstructionFiles {
		if !validPath(dest) || dest <= prev {
			return Receipt{}, nil, errors.New("instruction destination")
		}
		prev = dest
		file, ok := p.Sources.Instructions[dest]
		if !ok || !allowedFileMode(owner.SchemaVersion, File{Kind: "instruction", Mode: int(file.Permissions)}) || len(file.Content) > fileLimit || !utf8.Valid(file.Content) || bytes.IndexByte(file.Content, 0) >= 0 || digest(file.Content) != p.OutputSHA256[dest] {
			return Receipt{}, nil, errors.New("generated instruction hash or mode")
		}
		path := "instructions/" + dest
		if !validPath(path) {
			return Receipt{}, nil, errors.New("instruction payload path")
		}
		b := append([]byte(nil), file.Content...)
		payload[path] = b
		total += int64(len(b))
		r.Files = append(r.Files, File{path, digest(b), int(file.Permissions), "instruction", dest})
	}
	for _, path := range owner.SourceFiles {
		if !strings.HasPrefix(path, "config/claude/skills/") {
			continue
		}
		dest := strings.TrimPrefix(path, "config/claude/skills/")
		if !validPath(dest) || !strings.Contains(dest, "/") {
			return Receipt{}, nil, errors.New("canonical skill path")
		}
		target := "skills/" + dest
		if !validPath(target) {
			return Receipt{}, nil, errors.New("skill payload path")
		}
		b := append([]byte(nil), data[path]...)
		payload[target] = b
		total += int64(len(b))
		r.Files = append(r.Files, File{target, digest(b), 0644, "skill", dest})
	}
	if owner.SchemaVersion == 2 {
		if !validPolicy(p.Policy) || digest(p.PolicyBytes) != p.Policy.CanonicalPolicySHA256 {
			return Receipt{}, nil, errors.New("prepared policy binding")
		}
		if e = validatePolicyBytes(p.PolicyBytes); e != nil {
			return Receipt{}, nil, e
		}
		b := append([]byte(nil), p.PolicyBytes...)
		payload["inputs/route-policy.json"] = b
		total += int64(len(b))
		r.Files = append(r.Files, File{"inputs/route-policy.json", digest(b), 0600, "policy-input", "route-policy"})
	}
	if total > aggregateLimit {
		return Receipt{}, nil, errors.New("payload aggregate")
	}
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })
	if e = validateReceipt(r); e != nil {
		return Receipt{}, nil, e
	}
	if e = physicalBind(source, fd); e != nil {
		return Receipt{}, nil, e
	}
	return r, payload, nil
}
