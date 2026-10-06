package author

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestCatalogBaselinePrivateResourcePredicates(t *testing.T) {
	parent := catalogEndpoint{"/safe", 1, 10}
	resource := func(path string, ino uint64, dir bool) catalogResource {
		r := catalogResource{endpoint: catalogEndpoint{path, 1, ino}, directory: dir, ancestors: []catalogEndpoint{parent}}
		if dir {
			r.ancestors = append(r.ancestors, r.endpoint)
		}
		return r
	}
	a, b := resource("/safe/a", 11, true), resource("/safe/b", 12, false)
	if baselineResourceOverlap(a, b) {
		t.Fatal("common parent is not overlap")
	}
	alias := resource("/safe/alias", 11, false)
	if !baselineResourceOverlap(a, alias) {
		t.Fatal("distinct-name endpoint alias")
	}
	nested := resource("/elsewhere/file", 20, false)
	nested.ancestors = append(nested.ancestors, a.endpoint)
	if !baselineResourceOverlap(a, nested) || !baselineResourceOverlap(nested, a) {
		t.Fatal("selected root in held ancestors")
	}
	if !baselineResourceOverlap(a, resource("/safe/a/file", 21, false)) {
		t.Fatal("path containment")
	}
	frozen := baselineBindResources([]catalogResource{a})
	a.ancestors[0].ino++
	if frozen([]catalogResource{a}) == nil {
		t.Fatal("metadata binding aliased mutable ancestors")
	}
	if baselineResourceComplete(catalogResource{}) {
		t.Fatal("zero metadata complete")
	}
	// Synthetic records exercise this private predicate, not bind-mount custody.
}

func TestCatalogBaselineZeroAndNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, parent := range []context.Context{nil, ctx, context.Background()} {
		a, e := InspectCatalogBaselineSnapshot(parent, nil, nil, nil, nil)
		if e == nil || a.Encoding() != nil || a.VerifiedCategories() != nil || a.Blockers() != nil || a.FutureRequired() != nil {
			t.Fatalf("nil refusal leaked audit: %v", e)
		}
		if parent == ctx && !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	}
	o, g, h := &CatalogOriginalImportSnapshot{}, &CatalogGeneratedBaselineSnapshot{}, &CatalogHistoricalTemplateSnapshot{}
	if c, tr := o.Identity(); c != "" || tr != "" {
		t.Fatal("zero original identity")
	}
	if a, b := g.Identity(); a != "" || b != "" {
		t.Fatal("zero evidence identity")
	}
	if h.Identity() != "" {
		t.Fatal("zero historical identity")
	}
	for _, v := range []interface {
		Verify() error
		Close() error
	}{o, g, h} {
		if e := v.Verify(); e == nil {
			t.Fatal("uninitialized owner verified")
		}
		if e := v.Close(); e != nil {
			t.Fatal(e)
		}
		if e := v.Close(); e != nil {
			t.Fatal(e)
		}
		if e := v.Verify(); e == nil {
			t.Fatal("closed owner verified")
		}
	}
	if o.SourceBytes("x") != nil || g.OutputBytes(0, "x") != nil || g.ArchiveBytes() != nil || h.TemplateBytes() != nil {
		t.Fatal("zero bytes")
	}
	if runtime.GOOS != "linux" {
		if s, e := AcquireCatalogOriginalImportSnapshot(context.Background(), "/unavailable"); s != nil || e == nil {
			t.Fatal("non-Linux original")
		}
		if s, e := AcquireCatalogGeneratedBaselineSnapshot(context.Background(), "/unavailable", "/unavailable-review"); s != nil || e == nil {
			t.Fatal("non-Linux generated")
		}
		if s, e := AcquireCatalogHistoricalTemplateSnapshot(context.Background(), "/unavailable"); s != nil || e == nil {
			t.Fatal("non-Linux history")
		}
	}
}
