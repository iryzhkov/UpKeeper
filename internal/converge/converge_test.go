package converge_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/locks"
	"github.com/iryzhkov/UpKeeper/internal/policytest"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/runstore"
)

func repositoryRoot() string { return filepath.Join("..", "..") }

func fleetRelease(t *testing.T) release.Release {
	t.Helper()
	previous := release.EnvironmentValidator
	release.EnvironmentValidator = func(map[string]any) error { return nil }
	t.Cleanup(func() { release.EnvironmentValidator = previous })
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(), release.File))
	if err != nil {
		t.Skipf("no published manifest in this checkout: %v", err)
	}
	document, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	policytest.ManifestCatalog(t, document)
	value, err := release.Validate(document)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return value
}

func fleetConfig(t *testing.T) *inventory.Config {
	t.Helper()
	config, err := inventory.Load(repositoryRoot())
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	return config
}

// options returns a convergence wired entirely to temporary state, so that a
// test can never take a real lock or write a real run record.
func options(t *testing.T, hosts []string) converge.Options {
	t.Helper()
	base := t.TempDir()
	return converge.Options{
		Hosts:       hosts,
		Timeout:     60 * time.Second,
		RunID:       "20260914T090000Z-0a1b2c3d",
		LockStore:   &locks.Store{Root: filepath.Join(base, "locks")},
		RecordStore: &runstore.Store{Root: filepath.Join(base, "runs")},
		StoredFiles: map[string]string{},
	}
}

func stateRunner(state string, seen *[]agent.Payload) converge.HostRunner {
	return func(
		spec inventory.HostSpec, payload agent.Payload, _ time.Duration, _ bool,
	) map[string]any {
		if seen != nil {
			*seen = append(*seen, payload)
		}
		return map[string]any{
			"host": spec.Name, "reachable": true, "result": state, "components": []any{},
		}
	}
}

func TestExitCodeAggregationOrder(t *testing.T) {
	cases := []struct {
		name     string
		hosts    []any
		expected int
	}{
		{"everything converged", []any{host("a", true, "converged")}, exitcode.Success},
		{"drift from an ahead host", []any{host("a", true, "ahead")}, exitcode.Drift},
		{"a deferred change is drift", []any{host("a", true, "deferred")}, exitcode.Drift},
		{
			"an offline host outranks drift",
			[]any{host("a", true, "ahead"), host("b", false, "failed-to-reach")},
			exitcode.Partial,
		},
		{
			"lock contention outranks an offline host",
			[]any{host("a", true, "lock-contention"), host("b", false, "offline")},
			exitcode.LockContention,
		},
		{
			"a failure outranks lock contention",
			[]any{host("a", true, "failed"), host("b", true, "lock-contention")},
			exitcode.Deployment,
		},
		{
			"a successful rollback is still a deployment failure",
			[]any{host("a", true, "rolled-back")},
			exitcode.Deployment,
		},
		{
			"a failed rollback outranks everything",
			[]any{host("a", true, "rollback-failed"), host("b", true, "failed")},
			exitcode.Rollback,
		},
		{
			"a skipped unreachable host is not partial",
			[]any{map[string]any{
				"host": "a", "reachable": false, "result": "skipped",
				"skipped_unreachable": true,
			}},
			exitcode.Success,
		},
		{
			"a self-managed host is not partial",
			[]any{map[string]any{
				"host": "laptop", "reachable": false, "result": "skipped",
				"self_managed": true,
			}},
			exitcode.Success,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if code := converge.ExitCodeFor(testCase.hosts); code != testCase.expected {
				t.Errorf("produced %d, expected %d", code, testCase.expected)
			}
		})
	}
}

func TestManualControllerPullIncludesSelfManagedLaptop(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	for _, command := range []string{"pull", "rollback"} {
		t.Run(command, func(t *testing.T) {
			choices := options(t, []string{"laptop"})
			choices.Command = command
			var seen []agent.Payload
			choices.Runner = stateRunner("converged", &seen)
			report, code, err := converge.Fleet(config, value, choices)
			if err != nil || code != exitcode.Success {
				t.Fatalf("Fleet: code=%d err=%v", code, err)
			}
			if len(seen) != 1 || seen[0].Host.Name != "laptop" {
				t.Fatalf("manual %s did not dispatch to laptop: %+v", command, seen)
			}
			hosts, _ := report["hosts"].([]any)
			entry, _ := hosts[0].(map[string]any)
			if entry["result"] != "converged" {
				t.Errorf("host entry is %#v", entry)
			}
		})
	}
}

func TestScheduledControllerPullStillSkipsSelfManagedLaptop(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"laptop"})
	choices.Command = "scheduled-pull"
	choices.Runner = func(inventory.HostSpec, agent.Payload, time.Duration, bool) map[string]any {
		t.Fatal("scheduled controller dispatched to self-managed laptop")
		return nil
	}
	report, code, err := converge.Fleet(config, value, choices)
	if err != nil || code != exitcode.Success {
		t.Fatalf("Fleet: code=%d err=%v", code, err)
	}
	hosts, _ := report["hosts"].([]any)
	entry, _ := hosts[0].(map[string]any)
	if entry["self_managed"] != true || entry["result"] != "skipped" {
		t.Errorf("host entry is %#v", entry)
	}
}

func TestADryRunTakesNoLockAndWritesNoRecord(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"normandy"})
	choices.DryRun = true
	var seen []agent.Payload
	choices.Runner = stateRunner("converged", &seen)

	report, code, err := converge.Fleet(config, value, choices)
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if code != exitcode.Success {
		t.Errorf("exit code is %d", code)
	}
	if report["dry_run"] != true {
		t.Error("the report does not say it was a dry run")
	}
	if len(choices.RecordStore.ListIDs()) != 0 {
		t.Error("a dry run wrote a run record")
	}
	if _, err := os.Stat(choices.LockStore.Root); err == nil {
		t.Error("a dry run created the lock directory")
	}
	if len(seen) != 1 || !seen[0].DryRun {
		t.Errorf("the agent was not told this is a dry run: %#v", seen)
	}
}

func TestARealRunWritesAStartAndAFinishRecordAndReleasesItsLock(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"normandy"})
	choices.Runner = stateRunner("updated", nil)

	_, code, err := converge.Fleet(config, value, choices)
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if code != exitcode.Success {
		t.Errorf("exit code is %d", code)
	}
	record, err := choices.RecordStore.Read(choices.RunID)
	if err != nil {
		t.Fatalf("the run record is missing: %v", err)
	}
	if record["phase"] != "finished" || record["result"] != "success" {
		t.Errorf("the final record is %#v", record)
	}
	inspection, err := choices.LockStore.Inspect("normandy")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspection.State != "absent" {
		t.Errorf("the lock was not released: %q", inspection.State)
	}
}

func TestAConcurrentConvergenceReportsContentionWithoutDispatching(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	first := options(t, []string{"normandy"})
	started := make(chan struct{})
	releaseRunner := make(chan struct{})
	first.Runner = func(inventory.HostSpec, agent.Payload, time.Duration, bool) map[string]any {
		close(started)
		<-releaseRunner
		return map[string]any{"host": "normandy", "reachable": true, "result": "converged"}
	}
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := converge.Fleet(config, value, first)
		firstDone <- err
	}()
	<-started

	second := options(t, []string{"normandy"})
	second.LockStore = first.LockStore
	second.RunID = "20260914T080100Z-11112222"
	dispatched := false
	second.Runner = func(inventory.HostSpec, agent.Payload, time.Duration, bool) map[string]any {
		dispatched = true
		return nil
	}
	_, code, err := converge.Fleet(config, value, second)
	if err != nil {
		t.Fatalf("second Fleet: %v", err)
	}
	if dispatched {
		t.Fatal("second convergence dispatched while the first held the host lock")
	}
	if code != exitcode.LockContention {
		t.Errorf("exit code is %d, expected lock contention", code)
	}
	close(releaseRunner)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Fleet: %v", err)
	}
}

func TestTheT3PairIsAlwaysSelectedTogether(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"normandy"})
	choices.Components = []string{"t3"}
	var seen []agent.Payload
	choices.Runner = stateRunner("converged", &seen)

	if _, _, err := converge.Fleet(config, value, choices); err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	selected := seen[0].Components
	if len(selected) != 2 || selected[0] != "t3-steward" || selected[1] != "t3" {
		// Installing one half of the pair is exactly the transition the pair
		// exists to prevent, and the order is the convergence order.
		t.Errorf("selected components are %v", selected)
	}
}

func TestAnUnknownComponentIsRefused(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"normandy"})
	choices.Components = []string{"nonsense", "another"}
	choices.Runner = stateRunner("converged", nil)
	_, _, err := converge.Fleet(config, value, choices)
	if err == nil || err.Error() != "unknown components: another, nonsense" {
		t.Fatalf("produced %v", err)
	}
}

func TestSkipUnreachableSkipsOnlyTheOfflineHost(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"homelab", "normandy"})
	choices.SkipUnreachable = true
	choices.Runner = stateRunner("converged", nil)
	choices.Reachability = func(spec inventory.HostSpec) map[string]any {
		if spec.Name != "homelab" {
			return nil
		}
		return map[string]any{
			"host": spec.Name, "reachable": false, "result": "skipped",
			"skipped_unreachable": true, "error": "unreachable before dispatch",
		}
	}
	report, code, err := converge.Fleet(config, value, choices)
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if code != exitcode.Success {
		t.Errorf("exit code is %d, expected a skipped host not to fail the run", code)
	}
	hosts, _ := report["hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("the report lost a host: %#v", hosts)
	}
	first, _ := hosts[0].(map[string]any)
	if first["skipped_unreachable"] != true {
		t.Errorf("the offline host is %#v", first)
	}
}

func TestSelfOnlyRefusesAHostThatIsNotThisMachine(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"homelab", "normandy"})
	choices.SelfOnly = true
	choices.Runner = stateRunner("converged", nil)
	_, _, err := converge.Fleet(config, value, choices)
	if err == nil || err.Error() != "--self may select only this inventory machine" {
		t.Fatalf("produced %v", err)
	}
}

func TestRollbackTellsTheAgentThatADowngradeIsIntended(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"normandy"})
	choices.Command = "rollback"
	var seen []agent.Payload
	choices.Runner = stateRunner("updated", &seen)
	report, _, err := converge.Fleet(config, value, choices)
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if !seen[0].AllowDowngrade {
		// Without this a rollback would be refused by the very guard that
		// protects a normal pull from moving a checkout backwards.
		t.Error("a rollback did not permit moving a component backwards")
	}
	if report["command"] != "rollback" {
		t.Errorf("the report claims command %v", report["command"])
	}
}

func host(name string, reachable bool, result string) map[string]any {
	return map[string]any{"host": name, "reachable": reachable, "result": result}
}
