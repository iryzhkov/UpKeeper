package author

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// CatalogSnapshotOptions pins integrity only. Hashes do not approve source,
// authorize tools, or establish producer lineage or preparation success.
// Maps must contain exactly the frozen source26 and input13 inventories.
type CatalogSnapshotOptions struct {
	Repository   string
	Commit       string
	Tree         string
	InputRoot    string
	SourceSHA256 map[string]string
	InputSHA256  map[string]string
}

// CatalogSnapshot owns an inert byte snapshot and physical custody resources.
// It is deliberately distinct from SharedPreparation and cannot enter a store.
// Semantic input approval, configured catalog validation, Stage A/b32,
// interpreter/tokenizer acquisition, handoff and runtime gates remain required.
type CatalogSnapshot struct {
	mu       sync.Mutex
	sources  map[string][]byte
	inputs   map[string][]byte
	commit   string
	tree     string
	verify   func() error
	close    func() error
	closed   bool
	closeErr error
}

// SourceBytes returns a defensive copy, or nil for an unknown path/closed owner.
func (s *CatalogSnapshot) SourceBytes(path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return bytes.Clone(s.sources[path])
}

// InputBytes returns a defensive copy, preserving even blocking audit labels.
func (s *CatalogSnapshot) InputBytes(path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return bytes.Clone(s.inputs[path])
}

// Identity returns the actual selected full Git commit and tree, not approval.
func (s *CatalogSnapshot) Identity() (commit, tree string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commit, s.tree
}

// Verify independently rereads held bytes and rechecks physical/Git custody.
// It does not run any producer or confer semantic or runtime acceptance.
func (s *CatalogSnapshot) Verify() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.verify == nil {
		return errors.New("catalog snapshot closed or uninitialized")
	}
	return s.verify()
}

// Close releases only owned descriptors; it never removes caller inputs.
// Repeated calls return the same cleanup result.
func (s *CatalogSnapshot) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if s.close != nil {
			s.closeErr = s.close()
		}
		s.sources, s.inputs = nil, nil
		s.verify, s.close = nil, nil
	}
	return s.closeErr
}

// AcquireCatalogSnapshot captures a real inert integrity snapshot on Linux.
// Cancellation releases all owned resources and returns no usable snapshot.
func AcquireCatalogSnapshot(ctx context.Context, o CatalogSnapshotOptions) (*CatalogSnapshot, error) {
	if ctx == nil {
		return nil, errors.New("catalog snapshot context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !sharedPhysicalPath(o.Repository) || !sharedPhysicalPath(o.InputRoot) ||
		!sharedHex(o.Commit, 40) || !sharedHex(o.Tree, 40) || snapshotOverlap(o.Repository, o.InputRoot) {
		return nil, errors.New("catalog snapshot physical roots/full pins/overlap")
	}
	sources, inputs := snapshotSourceSpecs(), snapshotInputSpecs()
	if err := snapshotPins(o.SourceSHA256, sources); err != nil {
		return nil, err
	}
	if err := snapshotPins(o.InputSHA256, inputs); err != nil {
		return nil, err
	}
	// Retain no mutable caller map.
	o.SourceSHA256 = snapshotCopyPins(o.SourceSHA256)
	o.InputSHA256 = snapshotCopyPins(o.InputSHA256)
	return acquireCatalogSnapshot(ctx, o, sources, inputs)
}

type snapshotSpec struct {
	mode  uint32
	limit int64
}

func snapshotSourceSpecs() map[string]snapshotSpec {
	m := map[string]snapshotSpec{}
	for _, p := range SharedSourcePaths() {
		m[SharedDirectory+"/"+p] = snapshotSpec{0644, 16 << 20}
	}
	m[SharedDirectory+"/owner.json"] = snapshotSpec{0644, 256 << 10}
	for _, p := range []string{"config/bin/agents-instructions-gen", "config/bin/agents-role-project"} {
		m[SharedDirectory+"/"+p] = snapshotSpec{0755, 16 << 20}
	}
	const root = "shared/catalog-guidance/"
	m[root+"owner.json"] = snapshotSpec{0644, 256 << 10}
	m[root+"config/bin/agents-tool-project"] = snapshotSpec{0755, 16 << 20}
	m[root+"templates/discovery.txt"] = snapshotSpec{0644, 131072}
	for _, p := range []string{"manifest", "required-global-policy", "fixture-cli", "fixture-mcp", "fixture-third-party", "huyang"} {
		m[root+"guidance/"+p+".json"] = snapshotSpec{0644, 262144}
	}
	for _, id := range []string{"fixture-cli", "fixture-mcp", "fixture-third-party"} {
		m["internal/toolproducer/testdata/descriptors/"+id+".json"] = snapshotSpec{0644, 262144}
	}
	return m
}

func snapshotInputSpecs() map[string]snapshotSpec {
	m := map[string]snapshotSpec{}
	for _, p := range catalogNewInputPaths {
		m[p] = snapshotSpec{0600, 262144}
	}
	m["inputs/guidance-layout.json"] = snapshotSpec{0600, 131072}
	for _, p := range []string{"tool-catalog", "tool-producer-input", "tool-producer-receipt", "route-policy"} {
		m["inputs/"+p+".json"] = snapshotSpec{0600, 262144}
	}
	return m
}

func snapshotPins(pins map[string]string, specs map[string]snapshotSpec) error {
	if len(pins) != len(specs) {
		return errors.New("catalog snapshot pin inventory")
	}
	for path := range specs {
		if !sharedHex(pins[path], 64) {
			return fmt.Errorf("catalog snapshot missing/invalid pin: %s", path)
		}
	}
	return nil
}
func snapshotCopyPins(m map[string]string) map[string]string {
	n := make(map[string]string, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}
func snapshotPaths(m map[string]snapshotSpec) []string {
	p := make([]string, 0, len(m))
	for k := range m {
		p = append(p, k)
	}
	sort.Strings(p)
	return p
}
func snapshotOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}

// Input parsing here checks canonical encoding, bounded scalar structure and
// integer branch versions. Full input schemas/cross-document relationships are
// future producer/preparation gates. The frozen owner5 uses its owning parser.
func snapshotCanonical(raw []byte) error {
	if err := boundedSharedJSON(raw, 12, 1024); err != nil {
		return err
	}
	v, err := canonjson.DecodeObject(raw)
	if err != nil {
		return err
	}
	b, err := canonjson.MarshalFile(v)
	if err != nil || !bytes.Equal(b, raw) {
		return errors.New("catalog snapshot noncanonical JSON")
	}
	if err := catalogScalars(v); err != nil {
		return err
	}
	return snapshotTextBounds(v)
}

func snapshotTextBounds(v any) error {
	switch x := v.(type) {
	case string:
		if len(x) > 65536 {
			return errors.New("catalog text byte bound")
		}
	case map[string]any:
		for k, e := range x {
			if err := snapshotTextBounds(k); err != nil {
				return err
			}
			if err := snapshotTextBounds(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := snapshotTextBounds(e); err != nil {
				return err
			}
		}
	}
	return nil
}
