package sharedgeneration

import (
	"bytes"
	"encoding/json"
	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"sort"
	"strings"
	"testing"
)

// These are schema fixtures, not configured catalog or real parser proof.
func policyFixture() *author.SharedPolicy {
	return &author.SharedPolicy{
		InputManifestSHA256: strings.Repeat("1", 64), InputContract: "canonical-release-manifest-v1",
		CanonicalPolicySHA256: digest([]byte("{}\n")), PolicyFormat: "route-policy/v1-canonical-json",
		Derivation: "author-derive-policy-v1", UpstreamRepository: "github.com/iryzhkov/omarchy-setup",
		UpstreamCommit: "b32c0b6f48e12b7d74a60f8a010f84bab73d299f",
		UpstreamTree:   "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52",
		Interpretation: "steward-parser-and-pinned-generator", ConfiguredCatalogValidation: "completed-at-preparation",
		CurrentAuthorization: "unknown", Readiness: "unassessed",
	}
}
func receiptFixture() Receipt {
	h := strings.Repeat("2", 64)
	r := Receipt{SchemaVersion: 2, State: "candidate-inert", Owner: "upkeeper", SourceRepository: "github.com/iryzhkov/UpKeeper", SourceCommit: strings.Repeat("3", 40), OwnerDescriptorSHA256: h,
		SourceSHA256: map[string]string{"owner.json": h}, Files: []File{}, Policy: policyFixture()}
	for _, p := range author.SharedSourcePaths() {
		r.SourceSHA256[p] = h
		if strings.HasPrefix(p, "config/claude/skills/") {
			dest := strings.TrimPrefix(p, "config/claude/skills/")
			r.Files = append(r.Files, File{"skills/" + dest, h, 0644, "skill", dest})
		}
	}
	modes := []int{0600, 0644, 0700, 0755}
	for n, p := range author.PolicyInstructionPaths() {
		r.Files = append(r.Files, File{"instructions/" + p, h, modes[n%len(modes)], "instruction", p})
	}
	r.Files = append(r.Files, File{"inputs/route-policy.json", r.Policy.CanonicalPolicySHA256, 0600, "policy-input", "route-policy"})
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })
	return r
}
func TestV2ReceiptClosedPolicyAndModes(t *testing.T) {
	r := receiptFixture()
	if e := validateReceipt(r); e != nil {
		t.Fatal(e)
	}
	b, e := canonjson.MarshalFile(r)
	if e != nil {
		t.Fatal(e)
	}
	var got Receipt
	if e = strictJSON(b, &got, true); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*Receipt){
		func(r *Receipt) { r.Policy = nil },
		func(r *Receipt) { r.Policy.CurrentAuthorization = "authorized" },
		func(r *Receipt) { r.Policy.UpstreamCommit = strings.Repeat("0", 40) },
		func(r *Receipt) { r.Policy.InputManifestSHA256 = "HEAD" },
		func(r *Receipt) { r.Files = r.Files[1:] },
		func(r *Receipt) { r.Files = append(r.Files, r.Files[0]) },
		func(r *Receipt) { r.Files[0].Mode = 0644 },
		func(r *Receipt) { r.Files[0].LogicalDestination = "manifest" },
		func(r *Receipt) { r.Files[1].Mode = 0664 },
		func(r *Receipt) { r.SourceSHA256["config/bin/unlisted"] = strings.Repeat("4", 64) },
		func(r *Receipt) { delete(r.SourceSHA256, "config/bin/agents-role-project") },
	} {
		var bad Receipt
		if e = json.Unmarshal(b, &bad); e != nil {
			t.Fatal(e)
		}
		mutate(&bad)
		if e = validateReceipt(bad); e == nil {
			t.Fatal("bad receipt accepted")
		}
	}
	for _, bad := range [][]byte{
		bytes.Replace(b, []byte("\"policy\": {"), []byte("\"policy\": null, \"extra\": {"), 1),
		bytes.Replace(b, []byte("\"readiness\": \"unassessed\""), []byte("\"readiness\": \"unassessed\", \"grant\": true"), 1),
		bytes.Replace(b, []byte("\"readiness\": \"unassessed\""), []byte("\"readiness\": \"unassessed\", \"readiness\": \"unassessed\""), 1),
		bytes.Replace(b, []byte("\"schema_version\": 2"), []byte("\"schema_version\": 1"), 1),
	} {
		var v Receipt
		if e = strictJSON(bad, &v, true); e == nil {
			t.Fatal("bad wire accepted")
		}
	}
}
func TestPolicyPayloadCanonicalBoundsAndDuplicates(t *testing.T) {
	good, e := canonjson.MarshalFile(map[string]any{"roles": []any{}})
	if e != nil {
		t.Fatal(e)
	}
	if e = validatePolicyBytes(good); e != nil {
		t.Fatal(e)
	}
	// Receipt metadata limits do not narrow owning canonical policy structures.
	nested := any(0)
	for n := 0; n < 13; n++ {
		nested = []any{nested}
	}
	deep, e := canonjson.MarshalFile(map[string]any{"x": nested})
	if e != nil {
		t.Fatal(e)
	}
	if e = validatePolicyBytes(deep); e != nil {
		t.Fatal(e)
	}

	for _, b := range [][]byte{nil, []byte("{}"), []byte("null\n"), []byte("[]\n"), []byte("{\"x\":1,\"x\":2}\n"), []byte(strings.Repeat(" ", policyLimit+1)), []byte("{\"x\":" + strings.Repeat("[", 13) + "0" + strings.Repeat("]", 13) + "}\n")} {
		if e = validatePolicyBytes(b); e == nil {
			t.Fatal("bad policy payload accepted")
		}
	}
}
func TestV1PolicyFieldRejectedEvenNull(t *testing.T) {
	r := receiptFixture()
	r.SchemaVersion = 1
	r.Policy = nil
	b, e := canonjson.MarshalFile(r)
	if e != nil {
		t.Fatal(e)
	}
	bad := append([]byte("{\"policy\":null,"), b[1:]...)
	var got Receipt
	if e = strictJSON(bad, &got, false); e == nil {
		t.Fatal("v1 accepted v2 field")
	}
}
