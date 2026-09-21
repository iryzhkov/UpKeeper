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
		"projection-client-homelab.json",
		"projection-client-omarchy-pc.json",
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
		if string(client) != string(fixture(t, "projection-client-"+host+".json")) {
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

// Every legacy worker entry still co-published in the release must match fleet
// intent. Fleet-only hosts have already retired the legacy component.
// Compare the release's current fleet intent, rather than the frozen migration
// fixture: providers may be added after migration, while the shared fixtures
// above continue to qualify the original wire representation.
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
	intent, ok := components["steward-fleet-configuration"].(map[string]any)
	if !ok {
		t.Fatal("the published release configures workers without fleet intent")
	}
	document, err := fleet.Parse(intent)
	if err != nil {
		t.Fatalf("parse the published fleet intent: %v", err)
	}
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

// The client projection is UpKeeper's own document on the host, so its field
// set is closed for the same reason the worker bootstrap's is: a document that
// may carry anything is a document a credential value can be smuggled into.
func TestTheClientProjectionIsAClosedSetOfReferences(t *testing.T) {
	document := parsed(t)
	projection, err := document.ClientProjection("homelab")
	if err != nil {
		t.Fatalf("client projection: %v", err)
	}
	if len(projection) != len(fleet.ClientFields) {
		t.Fatalf("the projection has %d fields, want %d", len(projection), len(fleet.ClientFields))
	}
	for _, field := range fleet.ClientFields {
		if _, present := projection[field]; !present {
			t.Errorf("the projection is missing %s", field)
		}
	}
	if projection["credential_ref"] != "secretref:f03-admin/homelab" {
		t.Errorf("credential_ref is %v", projection["credential_ref"])
	}
	// The admin client and the worker protocol use different namespaces, so
	// that distributing one never distributes the other.
	worker, err := document.WorkerProjection("homelab")
	if err != nil {
		t.Fatalf("worker projection: %v", err)
	}
	if worker["credential_ref"] == projection["credential_ref"] {
		t.Error("the client and the worker share one credential reference")
	}
	body := string(fixture(t, "projection-client-homelab.json"))
	if strings.Count(body, "secretref:") != 1 {
		t.Errorf("the client fixture carries %d references", strings.Count(body, "secretref:"))
	}
}

// An endpoint authored against the frozen four-key shape still validates, and
// gets explicit limits rather than an unbounded client.
func TestAbsentMessageLimitsTakeTheExplicitDefault(t *testing.T) {
	document, err := fleet.Parse(edit(t, func(d map[string]any) {
		delete(object(t, d, "coordinator", "client_endpoint"), "message_limits")
	}))
	if err != nil {
		t.Fatalf("an endpoint without message limits was refused: %v", err)
	}
	projection, err := document.ClientProjection("homelab")
	if err != nil {
		t.Fatalf("client projection: %v", err)
	}
	limits, _ := projection["message_limits"].(map[string]any)
	if limits["max_bytes"] != fleet.DefaultMessageLimits.MaxBytes ||
		limits["max_artifact_bytes"] != fleet.DefaultMessageLimits.MaxArtifactBytes {
		t.Errorf("limits are %v, want the explicit default", limits)
	}
	// The names belong to t3-steward's V2MessageLimits vocabulary. One concept
	// with two names across the two repositories is the drift this component
	// exists to remove, so the spelling is pinned here.
	if fleet.DefaultMessageLimits.MaxBytes != 4194304 ||
		fleet.DefaultMessageLimits.MaxArtifactBytes != 1073741824 {
		t.Errorf("the defaults are not t3-steward's: %#v", fleet.DefaultMessageLimits)
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
			name: "message limits are a closed set",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["message_limits"] =
					map[string]any{
						"max_bytes":          json.Number("1024"),
						"max_artifact_bytes": json.Number("1024"),
						"max_everything":     json.Number("1024"),
					}
			},
			errorText: "message_limits must name only",
		},
		{
			name: "a message limit of zero is not a limit",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["message_limits"] =
					map[string]any{
						"max_bytes":          json.Number("0"),
						"max_artifact_bytes": json.Number("1024"),
					}
			},
			errorText: "max_bytes must be 1 through",
		},
		{
			name: "an unbounded artifact limit is refused",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["message_limits"] =
					map[string]any{
						"max_bytes":          json.Number("1024"),
						"max_artifact_bytes": json.Number("1073741825"),
					}
			},
			errorText: "max_artifact_bytes must be 1 through",
		},
		{
			name: "a timeout in seconds is not a duration",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["request_timeout"] =
					json.Number("30")
			},
			errorText: "must be a duration string",
		},
		{
			name: "a timeout that is not a duration at all",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["request_timeout"] = "soon"
			},
			errorText: "is not a duration",
		},
		{
			name: "a timeout below the bound",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["request_timeout"] = "500ms"
			},
			errorText: "request_timeout must be 1s through 1h0m0s",
		},
		{
			name: "a timeout above the bound",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["request_timeout"] = "2h"
			},
			errorText: "request_timeout must be 1s through 1h0m0s",
		},
		{
			name: "a remote command with arguments is not a program name",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["remote_command"] =
					"t3-steward coordinator serve --stdio"
			},
			errorText: "must be a bare program name",
		},
		{
			name: "a remote command with a path is not a program name",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["remote_command"] =
					"/home/igor/.local/bin/t3-steward"
			},
			errorText: "must be a bare program name",
		},
		{
			name: "the client endpoint takes no invented keys",
			change: func(d map[string]any) {
				object(t, d, "coordinator", "client_endpoint")["credential"] = "hunter2"
			},
			errorText: `fleet client endpoint carries an unknown field "credential"`,
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

// Every nested key set is closed, and this is why: a field nothing validates is
// a field a credential value can be authored into, and this component is
// published in the release manifest and distributed to every host. Forward
// compatibility is schema_version's job, not an open field set's.
func TestUnknownNestedFieldsAreRefused(t *testing.T) {
	cases := []struct {
		name   string
		change func(document map[string]any)
	}{
		{
			name: "in a host",
			change: func(d map[string]any) {
				object(t, d, "hosts", "homelab")["admin_token"] = "a-real-secret"
			},
		},
		{
			name: "in a profile",
			change: func(d map[string]any) {
				object(t, d, "profiles", "standard")["api_key"] = "a-real-secret"
			},
		},
		{
			name: "in a project",
			change: func(d map[string]any) {
				object(t, d, "projects", "upkeeper")["deploy_key"] = "a-real-secret"
			},
		},
		{
			name: "in an observation",
			change: func(d map[string]any) {
				d["observed"] = map[string]any{"hosts": map[string]any{
					"homelab": map[string]any{"session_token": "a-real-secret"},
				}}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			document := edit(t, test.change)
			_, err := fleet.Parse(document)
			if err == nil {
				t.Fatal("an unvalidated field was accepted into fleet intent")
			}
			// The refusal names the offending key and says why the set is
			// closed, so that nobody reads it as an incomplete document.
			if !strings.Contains(err.Error(), "carries an unknown field") ||
				!strings.Contains(err.Error(), "references only") {
				t.Fatalf("the refusal does not explain itself: %v", err)
			}
			if !strings.Contains(err.Error(), "token") &&
				!strings.Contains(err.Error(), "key") {
				t.Errorf("the refusal does not name the offending key: %v", err)
			}
			if strings.Contains(err.Error(), "a-real-secret") {
				t.Error("the refusal quotes the value it refused")
			}
		})
	}
}

// A missing key and an invented one are different mistakes, and reporting one
// as the other sends the reader looking for the wrong thing.
func TestAMissingKeyAndAnInventedKeyReadDifferently(t *testing.T) {
	_, invented := fleet.Parse(edit(t, func(d map[string]any) {
		object(t, d, "coordinator", "client_endpoint")["credential"] = "hunter2"
	}))
	if invented == nil ||
		!strings.Contains(invented.Error(), `carries an unknown field "credential"`) {
		t.Fatalf("an invented key reads as %v", invented)
	}
	_, absent := fleet.Parse(edit(t, func(d map[string]any) {
		delete(object(t, d, "coordinator", "client_endpoint"), "address")
	}))
	if absent == nil || !strings.Contains(absent.Error(), "is missing address") {
		t.Fatalf("an absent key reads as %v", absent)
	}
}

// The timeout is rendered as a duration string because the runtime that reads
// the document parses it with time.ParseDuration, inside its configuration
// load. A number there does not degrade one request: it stops that host's
// worker daemon and both exchange commands from starting at all.
func TestTheClientTimeoutIsRenderedAsADurationString(t *testing.T) {
	projection, err := parsed(t).ClientProjection("homelab")
	if err != nil {
		t.Fatalf("client projection: %v", err)
	}
	if projection["request_timeout"] != "30s" {
		t.Fatalf("request_timeout is %#v, want the string \"30s\"", projection["request_timeout"])
	}
	// An authored duration is normalized, so one intent renders one spelling
	// however it was written.
	normalized, err := fleet.Parse(edit(t, func(d map[string]any) {
		object(t, d, "coordinator", "client_endpoint")["request_timeout"] = "1m"
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	renormalized, err := normalized.ClientProjection("homelab")
	if err != nil {
		t.Fatalf("client projection: %v", err)
	}
	if renormalized["request_timeout"] != "1m0s" {
		t.Errorf("request_timeout is %#v", renormalized["request_timeout"])
	}
}

// A coordinator administers itself over its own local socket and refuses to
// start while a remote client endpoint is configured. It may still be a worker,
// so it gets a worker bootstrap and no client document.
func TestTheCoordinatorHostGetsNoClientDocument(t *testing.T) {
	document, err := fleet.Parse(edit(t, func(d map[string]any) {
		coordinator := object(t, d, "coordinator")
		coordinator["host"] = "homelab"
		object(t, d, "coordinator", "client_endpoint")["address"] = "homelab"
	}))
	if err != nil {
		t.Fatalf("a coordinator that is also a worker was refused: %v", err)
	}
	if document.NeedsClientProjection("homelab") {
		t.Error("the coordinator host would be given a client document")
	}
	if !document.NeedsClientProjection("omarchy-pc") {
		t.Error("a plain worker was denied its client document")
	}
	if _, err := document.ClientProjection("homelab"); err == nil ||
		!strings.Contains(err.Error(), "administers itself locally") {
		t.Fatalf("rendering the coordinator's client produced %v", err)
	}
	// It is still a worker.
	if _, err := document.WorkerBytes("homelab"); err != nil {
		t.Errorf("the coordinator lost its worker bootstrap: %v", err)
	}
	report, err := document.Report()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	homelab := object(t, report, "hosts", "homelab")
	if homelab["client_sha256"] != nil {
		t.Error("the report claims a client document for the coordinator")
	}
	if text, _ := homelab["client"].(string); !strings.Contains(text, "administers itself") {
		t.Errorf("the report does not say why: %q", text)
	}
}

// Authored input is distinct from the coordinator's enrollment revision.
func TestTheCoordinatorProjectionIsReportedAsAuthoredConfiguration(t *testing.T) {
	report, err := parsed(t).Report()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if _, present := report["proposed_catalog_sha256"]; present {
		t.Error("the report names a proposed catalog digest nothing would accept")
	}
	artifact, _ := report["coordinator_projection"].(map[string]any)
	if purpose, _ := artifact["purpose"].(string); !strings.Contains(purpose, "authored coordinator configuration") {
		t.Errorf("the coordinator projection is not labelled: %q", purpose)
	}
	if digest, _ := artifact["sha256"].(string); len(digest) != 64 {
		t.Errorf("the review artifact has no digest: %q", digest)
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
	// A plan carries no digest of its own that could be mistaken for the
	// revision the coordinator fences an enrollment against: that revision is
	// computed over data this side does not hold, so any number rendered here
	// would always be rejected. It names the command that takes the real one.
	for _, invented := range []string{
		"previous_catalog_sha256", "proposed_catalog_sha256", "observed_catalog_sha256",
	} {
		if _, present := plan[invented]; present {
			t.Errorf("the plan carries %s, which no coordinator would accept", invented)
		}
	}
	advice, _ := plan["fence"].(string)
	if !strings.Contains(advice, "backlog workers --json") ||
		!strings.Contains(advice, "worker enroll --catalog-revision") {
		t.Errorf("the plan does not name how to read and pass the fence: %q", advice)
	}
	changes, _ := plan["authorization_changes"].([]any)
	if len(changes) == 0 {
		t.Fatal("a first enrollment authorizes nothing")
	}

	// Against a previous document only the authorization-relevant difference
	// is reported.
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
	// The model change alters homelab's catalog revision, so the plan also
	// tells homelab to re-enroll and says, once, that the coordinator refuses
	// the reload while homelab holds retained work.
	changes, _ = plan["authorization_changes"].([]any)
	if len(changes) != 3 ||
		changes[0] != "worker homelab is authorized for claudeAgent model claude-opus-4-5" ||
		!strings.Contains(changes[1].(string), "t3-steward worker enroll homelab --current-catalog") ||
		changes[2] != fleet.ReloadRefusalAdvice {
		t.Errorf("authorization changes are %v", changes)
	}
}

func TestTheExpectedRevisionComesFromAnObservationOrIsAbsent(t *testing.T) {
	plan, err := parsed(t).EnrollmentPlan(nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan["expected_revision"] != nil {
		t.Error("a plan invented a revision nobody observed")
	}
	withObservation, err := fleet.Parse(edit(t, func(d map[string]any) {
		d["observed"] = map[string]any{
			"coordinator": map[string]any{"revision": json.Number("41")},
		}
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err = withObservation.EnrollmentPlan(nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan["expected_revision"] == nil {
		t.Error("a plan dropped the observation it is fenced against")
	}
	// A digest of the coordinator's own catalog is not something this side can
	// compute, so it is not part of the observation vocabulary either.
	_, err = fleet.Parse(edit(t, func(d map[string]any) {
		d["observed"] = map[string]any{
			"coordinator": map[string]any{"catalog_sha256": strings.Repeat("a", 64)},
		}
	}))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("an uncheckable catalog digest was accepted: %v", err)
	}
}
