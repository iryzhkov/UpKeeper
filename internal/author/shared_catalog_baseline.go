package author

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
)

// catalogResource is private, immutable custody metadata, never serialized or
// accepted through a parser or public setter. Acquisition binds it to held FDs.
type catalogEndpoint struct {
	path     string
	dev, ino uint64
}
type catalogResource struct {
	endpoint  catalogEndpoint
	directory bool
	ancestors []catalogEndpoint
}

// Bind an independent deep copy; caller-visible bytes cannot alter custody.
// The existing verifier authenticates live descriptors before this comparison.
func baselineBindResources(records []catalogResource) func([]catalogResource) error {
	frozen := make([]catalogResource, len(records))
	for i, r := range records {
		frozen[i] = r
		frozen[i].ancestors = append([]catalogEndpoint(nil), r.ancestors...)
	}
	return func(current []catalogResource) error {
		if len(current) != len(frozen) {
			return errors.New("baseline resource metadata changed")
		}
		for i, r := range current {
			f := frozen[i]
			if r.endpoint != f.endpoint || r.directory != f.directory || len(r.ancestors) != len(f.ancestors) {
				return errors.New("baseline resource metadata changed")
			}
			for j, a := range r.ancestors {
				if a != f.ancestors[j] {
					return errors.New("baseline ancestor metadata changed")
				}
			}
		}
		return nil
	}
}
func baselineResourceComplete(r catalogResource) bool {
	p := r.endpoint.path
	if p == "/" || !filepath.IsAbs(p) || filepath.Clean(p) != p || r.endpoint.ino == 0 {
		return false
	}
	parent := filepath.Dir(p)
	if r.directory {
		parent = p
	}
	for parent != "/" {
		found := false
		for _, a := range r.ancestors {
			if a.path == parent && a.ino != 0 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		parent = filepath.Dir(parent)
	}
	if r.directory {
		for _, a := range r.ancestors {
			if a.path == p {
				return a.dev == r.endpoint.dev && a.ino == r.endpoint.ino
			}
		}
		return false
	}
	return true
}
func baselineResourceOverlap(a, b catalogResource) bool {
	x, y := a.endpoint, b.endpoint
	if x.dev == y.dev && x.ino == y.ino || x.path == y.path ||
		strings.HasPrefix(x.path, y.path+"/") || strings.HasPrefix(y.path, x.path+"/") {
		return true
	}
	for _, pair := range [][2]catalogResource{{a, b}, {b, a}} {
		if !pair[0].directory {
			continue
		}
		for _, ancestor := range pair[1].ancestors {
			if pair[0].endpoint.dev == ancestor.dev && pair[0].endpoint.ino == ancestor.ino {
				return true
			}
		}
	}
	return false
}
func baselineResourceGroups(groups ...[]catalogResource) error {
	if len(groups) != 4 || len(groups[0]) != 2 || len(groups[1]) != 1 || len(groups[2]) != 2 || len(groups[3]) != 1 {
		return errors.New("baseline incomplete resource metadata")
	}
	var all []catalogResource
	for _, group := range groups {
		for _, r := range group {
			if !baselineResourceComplete(r) {
				return errors.New("baseline incomplete endpoint/ancestor metadata")
			}
			for _, prior := range all {
				if baselineResourceOverlap(r, prior) {
					return errors.New("baseline resource alias/containment")
				}
			}
			all = append(all, r)
		}
	}
	return nil
}

// baselineOwner is custody for this finite diagnostic unit. It has no store,
// preparation, execution or permission interface. Reads and decoding are finite
// synchronous operations; the logical deadline is not a hard realtime bound.
type baselineOwner struct {
	resources     []catalogResource
	resourceCheck func([]catalogResource) error
	mu            sync.Mutex
	data          map[string][]byte
	verify        func(context.Context) error
	release       func() error
	closed        bool
	initialized   bool
	closeErr      error
}

func (o *baselineOwner) verifyLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !o.initialized || o.closed || o.verify == nil || o.release == nil {
		return errors.New("baseline owner closed or uninitialized")
	}
	if err := o.verify(ctx); err != nil {
		return catalogContextErr(ctx, err)
	}
	return ctx.Err()
}
func (o *baselineOwner) read(path string) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	return bytes.Clone(o.data[path])
}
func (o *baselineOwner) check(ctx context.Context) error {
	if ctx == nil {
		return errors.New("baseline context required")
	}
	ctx, cancel := context.WithTimeout(ctx, catalogContextLimit)
	defer cancel()
	if err := catalogLock(ctx, &o.mu); err != nil {
		return err
	}
	defer o.mu.Unlock()
	return o.verifyLocked(ctx)
}
func (o *baselineOwner) closeOwner() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.closed = true
		if o.release != nil {
			o.closeErr = o.release()
		}
		o.resources = nil
		o.resourceCheck = nil
		o.data = nil
		o.verify = nil
		o.release = nil
	}
	return o.closeErr
}

// CatalogOriginalImportSnapshot owns acquired original typed Git objects.
// Its identity records retained acquisition provenance, not transport authentication.
type CatalogOriginalImportSnapshot struct{ owner baselineOwner }
type CatalogGeneratedBaselineSnapshot struct{ owner baselineOwner }
type CatalogHistoricalTemplateSnapshot struct{ owner baselineOwner }

func (s *CatalogOriginalImportSnapshot) SourceBytes(path string) []byte { return s.owner.read(path) }
func (s *CatalogOriginalImportSnapshot) Identity() (string, string) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if !s.owner.initialized {
		return "", ""
	}
	return sharedUpstreamCommit, sharedUpstreamTree
}
func (s *CatalogOriginalImportSnapshot) VerifyContext(ctx context.Context) error {
	return s.owner.check(ctx)
}
func (s *CatalogOriginalImportSnapshot) Verify() error { return s.VerifyContext(context.Background()) }
func (s *CatalogOriginalImportSnapshot) Close() error  { return s.owner.closeOwner() }
func (s *CatalogGeneratedBaselineSnapshot) OutputBytes(run int, path string) []byte {
	if run < 0 || run > 1 {
		return nil
	}
	prefix := "generated/run-0/"
	if run == 1 {
		prefix = "generated/run-1/"
	}
	return s.owner.read(prefix + path)
}
func (s *CatalogGeneratedBaselineSnapshot) ArchiveBytes() []byte { return s.owner.read("@archive") }
func (s *CatalogGeneratedBaselineSnapshot) ReviewBytes() []byte  { return s.owner.read("@review") }
func (s *CatalogGeneratedBaselineSnapshot) EvidenceBytes(path string) []byte {
	return s.owner.read(path)
}
func (s *CatalogGeneratedBaselineSnapshot) Identity() (string, string) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if !s.owner.initialized {
		return "", ""
	}
	return baselineEvidenceSHA, baselineReviewSHA
}
func (s *CatalogGeneratedBaselineSnapshot) VerifyContext(ctx context.Context) error {
	return s.owner.check(ctx)
}
func (s *CatalogGeneratedBaselineSnapshot) Verify() error {
	return s.VerifyContext(context.Background())
}
func (s *CatalogGeneratedBaselineSnapshot) Close() error { return s.owner.closeOwner() }
func (s *CatalogHistoricalTemplateSnapshot) TemplateBytes() []byte {
	return s.owner.read("history/templates/discovery.txt")
}
func (s *CatalogHistoricalTemplateSnapshot) CurrentTemplateBytes() []byte {
	return s.owner.read("templates/discovery.txt")
}
func (s *CatalogHistoricalTemplateSnapshot) ArchiveBytes() []byte { return s.owner.read("@archive") }
func (s *CatalogHistoricalTemplateSnapshot) Identity() string {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if !s.owner.initialized {
		return ""
	}
	return baselineDraftsSHA
}
func (s *CatalogHistoricalTemplateSnapshot) VerifyContext(ctx context.Context) error {
	return s.owner.check(ctx)
}
func (s *CatalogHistoricalTemplateSnapshot) Verify() error {
	return s.VerifyContext(context.Background())
}
func (s *CatalogHistoricalTemplateSnapshot) Close() error { return s.owner.closeOwner() }

const baselineEvidenceSHA = "49c8ff3650bcbbab0a498537cb5c74657735e09b77394e93af8d250908518e31"
const baselineReviewSHA = "14e5a71270553e273fa31fd7cdd2a3f3c4d0a5992e0df6adfd723d4b09799e4a"
const baselineDraftsSHA = "8ddcd0679217612b7a2612d475e95d5f7249ad4e2c9f96f47f5bccca509c13fc"
const baselineHistoricalSHA = "e32e70a92a6eee53d51b193fe314e9a60d68c971670852a8f2bd567b41edb80f"
const baselineCurrentSHA = "3fc46a2002e917bc1c8a1850769fc093a8b7703296955a61e67ca98ba7594334"

// CatalogBaselineAudit is immutable diagnostic evidence. It cannot enter a
// preparation or store and grants no runtime authority.
type CatalogBaselineAudit struct {
	commit, tree                            string
	encoding, originals, generated, history []byte
	verified, blockers, future              []string
}

func (a CatalogBaselineAudit) Identity() (string, string)        { return a.commit, a.tree }
func (a CatalogBaselineAudit) Encoding() []byte                  { return bytes.Clone(a.encoding) }
func (a CatalogBaselineAudit) OriginalImportRecords() []byte     { return bytes.Clone(a.originals) }
func (a CatalogBaselineAudit) GeneratedRecords() []byte          { return bytes.Clone(a.generated) }
func (a CatalogBaselineAudit) HistoricalTemplateBinding() []byte { return bytes.Clone(a.history) }
func (a CatalogBaselineAudit) VerifiedCategories() []string {
	return append([]string(nil), a.verified...)
}
func (a CatalogBaselineAudit) Blockers() []string       { return append([]string(nil), a.blockers...) }
func (a CatalogBaselineAudit) FutureRequired() []string { return append([]string(nil), a.future...) }

// InspectCatalogBaselineSnapshot borrows four owners under one inherited,
// cooperative context. Lock order is catalog/original/generated/history and
// unwind is synchronous in reverse order. No public inspector or getter is
// called under these locks; no verification survives a return.
func InspectCatalogBaselineSnapshot(parent context.Context, c *CatalogSnapshot, o *CatalogOriginalImportSnapshot, g *CatalogGeneratedBaselineSnapshot, h *CatalogHistoricalTemplateSnapshot) (CatalogBaselineAudit, error) {
	var zero CatalogBaselineAudit
	if parent == nil {
		return zero, errors.New("baseline context required")
	}
	if err := parent.Err(); err != nil {
		return zero, err
	}
	if c == nil || o == nil || g == nil || h == nil {
		return zero, errors.New("baseline owners required")
	}
	ctx, cancel := context.WithTimeout(parent, catalogContextLimit)
	defer cancel()
	locks := []*sync.Mutex{&c.mu, &o.owner.mu, &g.owner.mu, &h.owner.mu}
	acquired := 0
	defer func() {
		for i := acquired - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	for _, mu := range locks {
		if err := catalogLock(ctx, mu); err != nil {
			return zero, err
		}
		acquired++
	}
	fail := func(err error) (CatalogBaselineAudit, error) { return zero, catalogContextErr(ctx, err) }
	// Authenticate all custody before any composite semantics, under all locks.
	if err := c.verifyLocked(ctx); err != nil {
		return fail(err)
	}
	for _, owner := range []*baselineOwner{&o.owner, &g.owner, &h.owner} {
		if err := owner.verifyLocked(ctx); err != nil {
			return fail(err)
		}
	}
	if c.resourceCheck == nil {
		return fail(errors.New("baseline catalog resource metadata missing"))
	}
	if err := c.resourceCheck(c.resources); err != nil {
		return fail(err)
	}
	for _, owner := range []*baselineOwner{&o.owner, &g.owner, &h.owner} {
		if owner.resourceCheck == nil {
			return fail(errors.New("baseline owner resource metadata missing"))
		}
		if err := owner.resourceCheck(owner.resources); err != nil {
			return fail(err)
		}
	}
	if err := baselineResourceGroups(c.resources, o.owner.resources, g.owner.resources, h.owner.resources); err != nil {
		return fail(err)
	}
	guidance, err := inspectCatalogGuidanceLocked(ctx, c)
	if err != nil {
		return fail(err)
	}
	producer, err := inspectCatalogProducerLocked(ctx, c)
	if err != nil {
		return fail(err)
	}
	for _, owner := range []*baselineOwner{&o.owner, &g.owner, &h.owner} {
		if err := owner.verifyLocked(ctx); err != nil {
			return fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	audit, err := inspectCatalogBaselineLocked(ctx, c, o, g, h, guidance, producer)
	if err != nil {
		return fail(err)
	}
	if err := c.verifyLocked(ctx); err != nil {
		return fail(err)
	}
	for _, owner := range []*baselineOwner{&o.owner, &g.owner, &h.owner} {
		if err := owner.verifyLocked(ctx); err != nil {
			return fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return audit, nil
}
