package fleet_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// fixtureDir holds the documents both this repository and t3-steward carry
// with identical bytes: UpKeeper renders them, Steward parses them.
const fixtureDir = "../../testdata/fleet"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return data
}

func intent(t *testing.T) map[string]any {
	t.Helper()
	document, err := canonjson.DecodeObject(fixture(t, "fleet-intent.json"))
	if err != nil {
		t.Fatalf("decode the intent fixture: %v", err)
	}
	return document
}

func parsed(t *testing.T) *fleet.Document {
	t.Helper()
	document, err := fleet.Parse(intent(t))
	if err != nil {
		t.Fatalf("the shared intent fixture does not parse: %v", err)
	}
	return document
}

// edit changes one value inside the intent fixture and returns the result, so
// that a refusal test says exactly which field it broke.
func edit(t *testing.T, change func(document map[string]any)) map[string]any {
	t.Helper()
	document := intent(t)
	change(document)
	return document
}

func object(t *testing.T, document map[string]any, path ...string) map[string]any {
	t.Helper()
	current := document
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("fixture has no object at %s", strings.Join(path, "."))
		}
		current = next
	}
	return current
}

func TestTheSharedFixtureIsCanonicalAndSelfContained(t *testing.T) {
	raw := fixture(t, "fleet-intent.json")
	document, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	canonical, err := canonjson.MarshalFile(document)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(canonical) != string(raw) {
		t.Error("the shared intent fixture is not in canonical form")
	}
	// The other repository carries these exact bytes, so an absolute path from
	// this machine in them would be a path nobody else has.
	for _, name := range []string{
		"fleet-intent.json",
		"projection-worker-homelab.json",
		"projection-worker-omarchy-pc.json",
		"projection-coordinator.json",
		"projection-client-homelab.yaml",
		"projection-client-omarchy-pc.yaml",
	} {
		body := string(fixture(t, name))
		for _, forbidden := range []string{"/home/", "/Users/", "/root/"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s carries a host-specific absolute path", name)
			}
		}
	}
}

func TestRenderingIsDeterministicAndMatchesTheSharedFixtures(t *testing.T) {
	document := parsed(t)
	for _, host := range []string{"homelab", "omarchy-pc"} {
		worker, err := document.WorkerBytes(host)
		if err != nil {
			t.Fatalf("%s worker projection: %v", host, err)
		}
		if string(worker) != string(fixture(t, "projection-worker-"+host+".json")) {
			t.Errorf("%s worker projection is not the shared fixture:\n%s", host, worker)
		}
		client, err := document.ClientBytes(host)
		if err != nil {
			t.Fatalf("%s client projection: %v", host, err)
		}
		if string(client) != string(fixture(t, "projection-client-"+host+".yaml")) {
			t.Errorf("%s client projection is not the shared fixture:\n%s", host, client)
		}
	}
	catalog, err := document.CoordinatorBytes()
	if err != nil {
		t.Fatalf("coordinator projection: %v", err)
	}
	if string(catalog) != string(fixture(t, "projection-coordinator.json")) {
		t.Errorf("the coordinator projection is not the shared fixture:\n%s", catalog)
	}

	// The same intent parsed again renders the same bytes: nothing about the
	// machine, the map order or the run takes part in rendering.
	again := parsed(t)
	for _, host := range []string{"homelab", "omarchy-pc"} {
		first, _ := document.WorkerBytes(host)
		second, _ := again.WorkerBytes(host)
		if string(first) != string(second) {
			t.Errorf("%s worker projection is not deterministic", host)
		}
	}
	firstCatalog, _ := document.CoordinatorBytes()
	secondCatalog, _ := again.CoordinatorBytes()
	if string(firstCatalog) != string(secondCatalog) {
		t.Error("the coordinator projection is not deterministic")
	}
}

// This is the migration safety property: for a host whose intent has not
// changed, the worker projection is byte-identical to the document
// worker-configuration v1 publishes today. If this test fails, adding the new
// component would rewrite a live host's bootstrap.
func TestTheWorkerProjectionIsByteIdenticalToWorkerConfigurationV1(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "desired", "release.json"))
	if err != nil {
		t.Skipf("no published manifest in this checkout: %v", err)
	}
	manifest, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatalf("decode the published manifest: %v", err)
	}
	components, _ := manifest["components"].(map[string]any)
	workers, _ := components["worker-configuration"].(map[string]any)
	hosts, _ := workers["hosts"].(map[string]any)
	if len(hosts) == 0 {
		t.Skip("the published release configures no workers")
	}
	document := parsed(t)
	for name, entry := range hosts {
		published, err := canonjson.MarshalFile(entry)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		projected, err := document.WorkerBytes(name)
		if err != nil {
			t.Fatalf("%s is published as a worker but absent from fleet intent: %v", name, err)
		}
		if string(projected) != string(published) {
			t.Errorf("%s would be rewritten by the new component:\nv1:\n%s\nfleet:\n%s",
				name, published, projected)
		}
	}
}

func TestExceptionsOverrideTheProfileAndNothingElse(t *testing.T) {
	document := parsed(t)
	build := document.Profiles["build"]
	resolved, err := document.Resolved("omarchy-pc")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if build.ExecutorSlots != 3 || resolved.ExecutorSlots != 4 {
		t.Errorf("profile has %d slots and the host resolves to %d",
			build.ExecutorSlots, resolved.ExecutorSlots)
	}
	if resolved.CPUClass != build.CPUClass {
		t.Errorf("an exception changed a key it does not name: %q", resolved.CPUClass)
	}
	standard, err := document.Resolved("homelab")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if standard.ExecutorSlots != 2 {
		t.Errorf("a host without exceptions resolved to %d slots", standard.ExecutorSlots)
	}
}

func TestParseRefusals(t *testing.T) {
	cases := []struct {
		name      string
		change    func(document map[string]any)
		errorText string
	}{
		{
			name:      "an unknown schema version fails closed",
			change:    func(d map[string]any) { d["schema_version"] = json.Number("2") },
			errorText: "unsupported steward-fleet-configuration schema_version",
		},
		{
			name:      "the top-level field set stays closed",
			change:    func(d map[string]any) { d["catalog"] = map[string]any{} },
			errorText: "unknown top-level field",
		},
		{
			name: "worker identity must match its host key",
			change: func(d map[string]any) {
				d["hosts"].(map[string]any)["homelab"].(map[string]any)["worker_id"] = "elsewhere"
			},
			errorText: "worker identity must match its inventory host",
		},
		{
			name: "the worker credential namespace is fixed",
			change: func(d map[string]any) {
				d["hosts"].(map[string]any)["homelab"].(map[string]any)["credential_ref"] =
					"secretref:f03-admin/homelab"
			},
			errorText: "f02-protocol secret reference",
		},
		{
			name: "the admin credential namespace is fixed",
			change: func(d map[string]any) {
				d["hosts"].(map[string]any)["homelab"].(map[string]any)["admin_credential_ref"] =
					"secretref:f02-protocol/homelab"
			},
			errorText: "f03-admin secret reference",
		},
		{
			name: "an inline credential value is not a reference",
			change: func(d map[string]any) {
				d["hosts"].(map[string]any)["homelab"].(map[string]any)["credential_ref"] =
					"hunter2"
			},
			errorText: "f02-protocol secret reference",
		},
		{
			name: "lists must be sorted and unique",
			change: func(d map[string]any) {
				d["profiles"].(map[string]any)["standard"].(map[string]any)["capabilities"] =
					[]any{"huyang", "git"}
			},
			errorText: "sorted, unique string list",
		},
		{
			name: "a duplicate is refused rather than deduplicated",
			change: func(d map[string]any) {
				d["profiles"].(map[string]any)["standard"].(map[string]any)["quota_pools"] =
					[]any{"default", "default"}
			},
			errorText: "sorted, unique string list",
		},
		{
			name: "an exception may not invent a key",
			change: func(d map[string]any) {
				d["hosts"].(map[string]any)["omarchy-pc"].(map[string]any)["exceptions"] =
					map[string]any{"gpu": true}
			},
			errorText: "overrides a key its profile does not have",
		},
		{
			name: "a model may only be authorized on an instance the host runs",
			change: func(d map[string]any) {
				models := d["profiles"].(map[string]any)["standard"].(map[string]any)["models"]
				models.(map[string]any)["gemini"] = []any{"gemini-3"}
			},
			errorText: "an instance it does not run",
		},
		{
			name: "a project may not name an undeclared worker",
			change: func(d map[string]any) {
				d["projects"].(map[string]any)["upkeeper"].(map[string]any)["eligible_workers"] =
					[]any{"homelab", "nowhere"}
			},
			errorText: "eligible for an undeclared worker",
		},
		{
			name:      "the required profiles must exist",
			change:    func(d map[string]any) { delete(d["profiles"].(map[string]any), "build") },
			errorText: "must define build",
		},
		{
			name: "an observation may not name an undeclared host",
			change: func(d map[string]any) {
				d["observed"] = map[string]any{
					"hosts": map[string]any{"nowhere": map[string]any{}},
				}
			},
			errorText: "observed names an undeclared host",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := fleet.Parse(edit(t, test.change))
			if err == nil || !strings.Contains(err.Error(), test.errorText) {
				t.Fatalf("want an error containing %q, got %v", test.errorText, err)
			}
		})
	}
}

// A field this binary does not know is kept, not dropped: silently losing what
// a newer controller wrote is what turns a rollback into data loss.
func TestUnknownNestedFieldsArePreserved(t *testing.T) {
	document := edit(t, func(d map[string]any) {
		host := d["hosts"].(map[string]any)["homelab"].(map[string]any)
		host["future_field"] = "kept"
	})
	before, err := canonjson.MarshalFile(document)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsedDocument, err := fleet.Parse(document)
	if err != nil {
		t.Fatalf("an unknown nested field was refused: %v", err)
	}
	after, err := canonjson.MarshalFile(parsedDocument.Raw())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Error("an unknown nested field did not round-trip byte-for-byte")
	}
	// It is preserved, and it is still not authorization: the projections are
	// unchanged by it.
	worker, err := parsedDocument.WorkerBytes("homelab")
	if err != nil {
		t.Fatalf("worker projection: %v", err)
	}
	if string(worker) != string(fixture(t, "projection-worker-homelab.json")) {
		t.Error("an unknown field leaked into the worker projection")
	}
}

func TestInventoryDisagreementIsRefused(t *testing.T) {
	document := parsed(t)
	if err := document.CheckInventory(nil); err != nil {
		t.Fatalf("a check without an inventory must pass: %v", err)
	}
	known := map[string]bool{"homelab": true, "omarchy-pc": true, "normandy": true}
	if err := document.CheckInventory(known); err != nil {
		t.Fatalf("the fixture disagrees with its own inventory: %v", err)
	}
	delete(known, "omarchy-pc")
	err := document.CheckInventory(known)
	if err == nil || !strings.Contains(err.Error(), "not in the inventory") {
		t.Fatalf("want an inventory refusal, got %v", err)
	}
}

func TestAnEmptyDesiredSetIsNotAMissingObservation(t *testing.T) {
	// A host authorized for nothing.
	empty, err := fleet.Parse(edit(t, func(d map[string]any) {
		object(t, d, "profiles", "standard")["models"] = map[string]any{}
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sets, err := empty.Models("homelab")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(sets.Desired) != 0 || sets.HasObservation {
		t.Fatalf("an unobserved host reports %#v", sets)
	}
	report, err := empty.Report()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	homelab := object(t, report, "hosts", "homelab")
	if homelab["observed_models"] != nil {
		t.Error("a host with no observation claimed one")
	}

	// The same host, observed to have nothing.
	observed, err := fleet.Parse(edit(t, func(d map[string]any) {
		object(t, d, "profiles", "standard")["models"] = map[string]any{}
		d["observed"] = map[string]any{
			"hosts": map[string]any{
				"homelab": map[string]any{"models": map[string]any{}},
			},
		}
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sets, err = observed.Models("homelab")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if !sets.HasObservation || len(sets.Observed) != 0 {
		t.Fatalf("an observed-empty host reports %#v", sets)
	}
	report, err = observed.Report()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if object(t, report, "hosts", "homelab")["observed_models"] == nil {
		t.Error("an observed-empty host is indistinguishable from an unobserved one")
	}
}

func TestAnEnrollmentPlanProposesAndNeverApplies(t *testing.T) {
	proposed := parsed(t)
	plan, err := proposed.EnrollmentPlan(nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan["applied"] != false {
		t.Fatalf("a plan reported applied=%v", plan["applied"])
	}
	if plan["previous_catalog_sha256"] != nil {
		t.Error("a first enrollment claimed a previous catalog")
	}
	catalog, err := proposed.CoordinatorBytes()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if plan["proposed_catalog_sha256"] != fleet.Digest(catalog) {
		t.Error("the plan does not name the digest it proposes")
	}
	changes, _ := plan["authorization_changes"].([]any)
	if len(changes) == 0 {
		t.Fatal("a first enrollment authorizes nothing")
	}

	// Against a previous document only the authorization-relevant difference
	// is reported, and the previous digest is named so the change is fenced.
	previous, err := fleet.Parse(edit(t, func(d map[string]any) {
		object(t, d, "profiles", "standard")["models"].(map[string]any)["claudeAgent"] =
			[]any{"claude-sonnet-4-5"}
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err = proposed.EnrollmentPlan(previous)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan["previous_catalog_sha256"] == nil {
		t.Error("a plan against a previous catalog did not name its digest")
	}
	changes, _ = plan["authorization_changes"].([]any)
	if len(changes) != 1 ||
		changes[0] != "worker homelab is authorized for claudeAgent model claude-opus-4-5" {
		t.Errorf("authorization changes are %v", changes)
	}
}

func TestTheExpectedRevisionComesFromAnObservationOrIsAbsent(t *testing.T) {
	plan, err := parsed(t).EnrollmentPlan(nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan["expected_revision"] != nil || plan["observed_catalog_sha256"] != nil {
		t.Error("a plan invented a revision nobody observed")
	}
	withObservation, err := fleet.Parse(edit(t, func(d map[string]any) {
		d["observed"] = map[string]any{
			"coordinator": map[string]any{
				"catalog_sha256": strings.Repeat("a", 64),
				"revision":       json.Number("41"),
			},
		}
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err = withObservation.EnrollmentPlan(nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan["expected_revision"] == nil || plan["observed_catalog_sha256"] == nil {
		t.Error("a plan dropped the observation it is fenced against")
	}
}
