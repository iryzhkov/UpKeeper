package report

import (
	"errors"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// The commits the fixtures agree on. The steward is pinned to a full commit and
// reports an abbreviated one, which is the case the prefix rule exists for.
const (
	stewardCommit = "81f1965d614db49891230e8b6c6e23db6c43f806"
	stewardShort  = "81f1965"
	bridgeCommit  = "ffffffffffffffffffffffffffffffffffffffff"
	manifestHead  = "0123456789abcdef0123456789abcdef01234567"
)

// gitRunner answers the one command a report runs, so that no test depends on
// the checkout it happens to run from.
func gitRunner(head string, code int) procx.Runner {
	return func(procx.Command) procx.Result {
		return procx.Result{Code: code, Stdout: head + "\n"}
	}
}

func host(name string) inventory.HostSpec {
	return inventory.HostSpec{
		Name:                  name,
		Address:               "10.0.0.1",
		User:                  "igor",
		CanonicalHostname:     name,
		Architecture:          "x86_64",
		OSID:                  "debian",
		OSVersion:             "12",
		MachineIDSHA256:       strings.Repeat("a", 64),
		StableAddressVerified: true,
	}
}

// pinned is a manifest component pinned to one immutable commit.
func pinned(commit string) map[string]any {
	return map[string]any{"pin": map[string]any{"kind": "git", "value": commit}}
}

func testConfig(t *testing.T, components map[string]any) *inventory.Config {
	t.Helper()
	return &inventory.Config{
		Root: t.TempDir(),
		Hosts: map[string]inventory.HostSpec{
			"normandy": host("normandy"),
			"laptop":   host("laptop"),
		},
		Manifest: map[string]any{"components": components},
	}
}

// healthy is an observation of a host on which every prerequisite holds.
func healthy(name string) map[string]any {
	spec := host(name)
	return map[string]any{
		"reachable":         true,
		"transport":         "ssh",
		"hostname":          spec.CanonicalHostname,
		"user":              spec.User,
		"architecture":      spec.Architecture,
		"machine_id_sha256": spec.MachineIDSHA256,
		"os":                map[string]any{"id": spec.OSID, "version": spec.OSVersion},
		"python":            map[string]any{"version": "3.11.2", "executable": "/usr/bin/python3"},
		"ansible":           map[string]any{"available": true, "version": "ansible 2.16"},
		"systemd_user":      map[string]any{"state": "running", "linger": "yes"},
		"forgejo_access":    true,
		"locks":             []any{},
		"source_access": map[string]any{
			"t3-steward": map[string]any{"reachable": true, "via": "ssh"},
			"agent99":    map[string]any{"reachable": true, "via": "ssh"},
		},
		"components": map[string]any{
			"t3-steward": map[string]any{
				"version": "t3-steward 0.10.1 (commit " + stewardShort + ")",
				"service": map[string]any{"ActiveState": "active", "UnitFileState": "enabled"},
			},
			"agent99": map[string]any{
				"checkout":       map[string]any{"present": true, "head": bridgeCommit, "dirty_entries": 0},
				"active_bridges": []any{map[string]any{"pid": 12}},
			},
			"agent-config": map[string]any{"config": map[string]any{"exists": true}},
		},
	}
}

// offline is the document a host that could not be reached produces.
func offline() map[string]any {
	return map[string]any{"reachable": false, "transport": "ssh", "error": "TimeoutExpired"}
}

// mutated copies an observation and applies one change to it, so that a table
// entry can describe exactly the one thing that is wrong with a host.
func mutated(name string, change func(map[string]any)) map[string]any {
	document := healthy(name)
	change(document)
	return document
}

// failedChecks reads the failed check names out of one host entry.
func failedChecks(t *testing.T, entry map[string]any) []string {
	t.Helper()
	items, ok := entry["failed_checks"].([]any)
	if !ok {
		t.Fatalf("host entry has no failed_checks: %v", entry)
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		name, _ := item.(string)
		names = append(names, name)
	}
	return names
}

func hostEntries(t *testing.T, document map[string]any) []map[string]any {
	t.Helper()
	items, ok := document["hosts"].([]any)
	if !ok {
		t.Fatalf("report has no hosts: %v", document)
	}
	entries := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("host entry is not an object: %v", item)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestDoctorGradesAFleetAndNeverClaimsMoreThanItSaw(t *testing.T) {
	components := map[string]any{
		"t3-steward": pinned(stewardCommit),
		"agent99":    pinned(bridgeCommit),
	}
	tests := []struct {
		name         string
		order        []string
		observations map[string]any
		wantResult   string
		wantCode     int
		wantHost     string
		wantFailed   []string
	}{
		{
			name:         "a healthy fleet passes",
			order:        []string{"normandy"},
			observations: map[string]any{"normandy": healthy("normandy")},
			wantResult:   "healthy",
			wantCode:     exitcode.Success,
			wantHost:     "healthy",
			wantFailed:   []string{},
		},
		{
			name:  "a missing Ansible controller is a preflight failure",
			order: []string{"normandy"},
			observations: map[string]any{"normandy": mutated("normandy", func(document map[string]any) {
				document["ansible"] = map[string]any{"available": false, "version": nil}
			})},
			wantResult: "failed",
			wantCode:   exitcode.Preflight,
			wantHost:   "failed",
			wantFailed: []string{"ansible-controller"},
		},
		{
			name:  "several broken prerequisites are all named",
			order: []string{"normandy"},
			observations: map[string]any{"normandy": mutated("normandy", func(document map[string]any) {
				document["systemd_user"] = map[string]any{"state": "degraded", "linger": "no"}
				document["source_access"] = map[string]any{
					"t3-steward": map[string]any{"reachable": false, "via": nil},
				}
			})},
			wantResult: "failed",
			wantCode:   exitcode.Preflight,
			wantHost:   "failed",
			wantFailed: []string{"systemd-user", "linger", "source-access:t3-steward"},
		},
		{
			name:         "an offline host makes the whole report partial",
			order:        []string{"normandy"},
			observations: map[string]any{"normandy": offline()},
			wantResult:   "partial",
			wantCode:     exitcode.Partial,
			wantHost:     "offline",
		},
		{
			name:  "an offline host outranks a failed one",
			order: []string{"normandy", "laptop"},
			observations: map[string]any{
				"normandy": mutated("normandy", func(document map[string]any) {
					document["forgejo_access"] = false
				}),
				"laptop": offline(),
			},
			wantResult: "partial",
			wantCode:   exitcode.Partial,
			wantHost:   "failed",
			wantFailed: []string{"forgejo-access"},
		},
		{
			name:         "a host that was never observed is offline, not healthy",
			order:        []string{"normandy"},
			observations: map[string]any{},
			wantResult:   "partial",
			wantCode:     exitcode.Partial,
			wantHost:     "offline",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t, components)
			document, code := DoctorWith(
				config, test.observations, test.order, gitRunner(manifestHead, 0),
			)

			if code != test.wantCode {
				t.Errorf("exit code = %d, want %d", code, test.wantCode)
			}
			if document["exit_code"] != test.wantCode {
				t.Errorf("exit_code field = %v, want %d", document["exit_code"], test.wantCode)
			}
			if document["result"] != test.wantResult {
				t.Errorf("result = %v, want %s", document["result"], test.wantResult)
			}
			if document["command"] != "doctor" {
				t.Errorf("command = %v, want doctor", document["command"])
			}

			entries := hostEntries(t, document)
			if len(entries) != len(test.order) {
				t.Fatalf("reported %d hosts, want %d", len(entries), len(test.order))
			}
			for position, name := range test.order {
				if entries[position]["name"] != name {
					t.Fatalf("host %d is %v, want %s in selection order",
						position, entries[position]["name"], name)
				}
			}
			first := entries[0]
			if first["result"] != test.wantHost {
				t.Errorf("host result = %v, want %s", first["result"], test.wantHost)
			}
			if test.wantHost == "offline" {
				if first["error"] == nil {
					t.Errorf("an offline host must name its error: %v", first)
				}
				return
			}
			got := failedChecks(t, first)
			if strings.Join(got, ",") != strings.Join(test.wantFailed, ",") {
				t.Errorf("failed_checks = %v, want %v", got, test.wantFailed)
			}
		})
	}
}

func TestDoctorRecordsTheHeaderEveryReportCarries(t *testing.T) {
	config := testConfig(t, map[string]any{"t3-steward": pinned(stewardCommit)})
	document, _ := DoctorWith(
		config,
		map[string]any{"normandy": healthy("normandy")},
		[]string{"normandy"},
		gitRunner(manifestHead, 0),
	)

	if document["schema_version"] != exitcode.SchemaVersion {
		t.Errorf("schema_version = %v, want %d", document["schema_version"], exitcode.SchemaVersion)
	}
	if document["desired_manifest_commit"] != manifestHead {
		t.Errorf("desired_manifest_commit = %v, want %s",
			document["desired_manifest_commit"], manifestHead)
	}
	runID, _ := document["run_id"].(string)
	if len(runID) != 25 {
		t.Errorf("run_id = %q, want a stamped identifier", runID)
	}
	if document["generated_at"] == "" || document["generated_at"] == nil {
		t.Errorf("generated_at = %v, want a timestamp", document["generated_at"])
	}
	// The controller is only named when this machine is in the inventory, and
	// the fixture's identity is deliberately not this machine's.
	if document["controller"] != "unknown" {
		t.Errorf("controller = %v, want unknown", document["controller"])
	}
}

func TestReportsRefuseToGuessAManifestCommit(t *testing.T) {
	tests := []struct {
		name   string
		runner procx.Runner
		want   any
	}{
		{"a checkout answers", gitRunner(manifestHead, 0), manifestHead},
		{"a directory that is no checkout", gitRunner("", 128), nil},
		{"a truncated answer", gitRunner("0123456", 0), nil},
		{
			name: "Git could not be started",
			runner: func(procx.Command) procx.Result {
				return procx.Result{Code: -1, Err: errors.New("git: no such file or directory")}
			},
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t, map[string]any{"t3-steward": pinned(stewardCommit)})
			document, _ := StatusWith(
				config,
				map[string]any{"normandy": healthy("normandy")},
				[]string{"normandy"},
				test.runner,
			)
			if document["desired_manifest_commit"] != test.want {
				t.Errorf("desired_manifest_commit = %v, want %v",
					document["desired_manifest_commit"], test.want)
			}
		})
	}
}

// componentOf finds one component entry in the first host of a status report.
func componentOf(t *testing.T, document map[string]any, name string) map[string]any {
	t.Helper()
	entries := hostEntries(t, document)
	if len(entries) == 0 {
		t.Fatalf("report has no hosts: %v", document)
	}
	items, _ := entries[0]["components"].([]any)
	for _, item := range items {
		component, _ := item.(map[string]any)
		if component["name"] == name {
			return component
		}
	}
	t.Fatalf("no component named %s in %v", name, items)
	return nil
}

func TestStatusJudgesEveryComponentAgainstItsPin(t *testing.T) {
	tests := []struct {
		name         string
		components   map[string]any
		observations map[string]any
		wantStates   map[string]string
		wantResult   string
		wantCode     int
	}{
		{
			name: "an abbreviated installed commit still converges",
			components: map[string]any{
				"t3-steward": pinned(stewardCommit),
				"agent99":    pinned(bridgeCommit),
			},
			observations: map[string]any{"normandy": healthy("normandy")},
			wantStates:   map[string]string{"t3-steward": "converged", "agent99": "converged"},
			wantResult:   "converged",
			wantCode:     exitcode.Success,
		},
		{
			name: "a different installed commit is drift",
			components: map[string]any{
				"t3-steward": pinned(stewardCommit),
				"agent99":    pinned(strings.Repeat("c", 40)),
			},
			observations: map[string]any{"normandy": healthy("normandy")},
			wantStates:   map[string]string{"agent99": "drift"},
			wantResult:   "drift",
			wantCode:     exitcode.Drift,
		},
		{
			name: "a component with no pin needs a decision",
			components: map[string]any{
				"t3-steward":   pinned(stewardCommit),
				"agent-config": map[string]any{"pin": nil},
			},
			observations: map[string]any{"normandy": healthy("normandy")},
			wantStates:   map[string]string{"agent-config": "decision-required"},
			wantResult:   "drift",
			wantCode:     exitcode.Drift,
		},
		{
			name: "a pinned component nobody could observe is unknown",
			components: map[string]any{
				"t3-steward":  pinned(stewardCommit),
				"homelab-cli": pinned(strings.Repeat("d", 40)),
			},
			observations: map[string]any{"normandy": healthy("normandy")},
			wantStates:   map[string]string{"homelab-cli": "unknown"},
			wantResult:   "drift",
			wantCode:     exitcode.Drift,
		},
		{
			name:         "an offline host is partial, never drift",
			components:   map[string]any{"t3-steward": pinned(stewardCommit)},
			observations: map[string]any{"normandy": offline()},
			wantResult:   "partial",
			wantCode:     exitcode.Partial,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t, test.components)
			document, code := StatusWith(
				config, test.observations, []string{"normandy"}, gitRunner(manifestHead, 0),
			)

			if code != test.wantCode {
				t.Errorf("exit code = %d, want %d", code, test.wantCode)
			}
			if document["result"] != test.wantResult {
				t.Errorf("result = %v, want %s", document["result"], test.wantResult)
			}
			if document["command"] != "status" {
				t.Errorf("command = %v, want status", document["command"])
			}
			for name, want := range test.wantStates {
				if got := componentOf(t, document, name)["state"]; got != want {
					t.Errorf("%s state = %v, want %s", name, got, want)
				}
			}
		})
	}
}

func TestStatusKeepsDesiredInstalledActivatedAndRunningApart(t *testing.T) {
	config := testConfig(t, map[string]any{
		"t3-steward":   pinned(stewardCommit),
		"agent99":      pinned(bridgeCommit),
		"agent-config": map[string]any{"pin": nil},
	})
	document, _ := StatusWith(
		config,
		map[string]any{"normandy": healthy("normandy")},
		[]string{"normandy"},
		gitRunner(manifestHead, 0),
	)

	steward := componentOf(t, document, "t3-steward")
	for _, key := range []string{"desired", "installed", "activated", "running"} {
		if _, found := steward[key]; !found {
			t.Errorf("t3-steward has no %s field: %v", key, steward)
		}
	}
	if desired := steward["desired"].(map[string]any); desired["commit"] != stewardCommit {
		t.Errorf("desired commit = %v, want %s", desired["commit"], stewardCommit)
	}
	if installed := steward["installed"].(map[string]any); installed["commit"] != stewardShort {
		t.Errorf("installed commit = %v, want %s", installed["commit"], stewardShort)
	}
	activated := steward["activated"].(map[string]any)
	if activated["service_state"] != "active" || activated["unit_state"] != "enabled" {
		t.Errorf("activated = %v, want the observed unit state", activated)
	}
	// A running service does not report which revision it was started from, so
	// the running commit stays null rather than repeating the installed one.
	if running := steward["running"].(map[string]any); running["commit"] != nil {
		t.Errorf("running commit = %v, want null", running["commit"])
	}

	bridge := componentOf(t, document, "agent99")
	if running := bridge["running"].(map[string]any); running["active_processes"] != 1 {
		t.Errorf("active_processes = %v, want 1", running["active_processes"])
	}
	if bridge["dirty_entries"] != 0 {
		t.Errorf("dirty_entries = %v, want 0", bridge["dirty_entries"])
	}

	configured := componentOf(t, document, "agent-config")
	if activated := configured["activated"].(map[string]any); activated["config_present"] != true {
		t.Errorf("agent-config activated = %v, want a present configuration", activated)
	}
	if configured["running"] != nil {
		t.Errorf("agent-config running = %v, want null", configured["running"])
	}
}

func TestStatusReportsIdentityAndLocksForAReachableHost(t *testing.T) {
	config := testConfig(t, map[string]any{"t3-steward": pinned(stewardCommit)})
	observed := mutated("normandy", func(document map[string]any) {
		document["locks"] = []any{"normandy.lock"}
	})
	document, _ := StatusWith(
		config,
		map[string]any{"normandy": observed},
		[]string{"normandy"},
		gitRunner(manifestHead, 0),
	)

	entry := hostEntries(t, document)[0]
	identity := entry["identity"].(map[string]any)
	if identity["hostname"] != "normandy" || identity["architecture"] != "x86_64" {
		t.Errorf("identity = %v, want the observed identity", identity)
	}
	locks := entry["locks"].([]any)
	if len(locks) != 1 || locks[0] != "normandy.lock" {
		t.Errorf("locks = %v, want the held lock", locks)
	}
}
