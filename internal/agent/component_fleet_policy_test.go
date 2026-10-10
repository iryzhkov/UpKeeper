package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/policytest"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// The isolated catalog fixture uses a local socket. Production's intended SSH
// endpoint is replaced only at this transport boundary, after checking that the
// complete staged client was supplied. The real CLI still authorizes the policy.
func policyFixtureRunner(t *testing.T) procx.Runner {
	t.Helper()
	return func(cmd procx.Command) procx.Result {
		argv := append([]string(nil), cmd.Argv...)
		for i := 0; i < len(argv); i++ {
			if argv[i] != "--config" {
				continue
			}
			if i+1 >= len(argv) {
				t.Fatal("missing staged client configuration")
			}
			body, err := os.ReadFile(argv[i+1])
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				V2 struct {
					Client map[string]any `json:"coordinator_client"`
				} `json:"backlog_v2"`
			}
			if err := json.Unmarshal(body, &config); err != nil {
				t.Fatal(err)
			}
			if config.V2.Client["credential"] == nil || config.V2.Client["coordinator_id"] == nil {
				t.Fatal("staged client lost identity or secret reference")
			}
			argv = append(argv[:i], argv[i+2:]...)
			break
		}
		cmd.Argv = argv
		return procx.Run(cmd)
	}
}

func policyContext(t *testing.T) (*Context, map[string]any) {
	t.Helper()
	policytest.RequireCLI(t)
	policytest.Catalog(t, []string{"gpt-6.1-sol"})
	c := fleetTestPrepare(t)
	c.Runner, c.Env = policyFixtureRunner(t), os.Environ()
	component := fleetTestComponent(t)
	var policy map[string]any
	if err := json.Unmarshal([]byte(policytest.Policy), &policy); err != nil {
		t.Fatal(err)
	}
	component["route_policy"] = policy
	return c, component
}
func policyRead(t *testing.T, c *Context) []byte {
	t.Helper()
	b, err := os.ReadFile(c.Path(fleet.RoutePolicyDestination))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFleetPolicyInstallLastGoodAndOwnedRetirement(t *testing.T) {
	c, component := policyContext(t)
	original := []byte("# unmanaged original policy\n")
	adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), string(original))
	if _, err := fleetProjection(c, component, fleetTestHost, "policy-first", false); err != nil {
		t.Fatal(err)
	}
	first := policyRead(t, c)
	d, err := fleet.Parse(component)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, d.RoutePolicyBytes()) {
		t.Fatal("wrong rendered bytes")
	}
	info, _ := os.Stat(c.Path(fleet.RoutePolicyDestination))
	if info.Mode().Perm() != 0644 {
		t.Fatalf("policy mode %o", info.Mode().Perm())
	}
	// A second update retains the first managed version, while ownership keeps
	// the original unowned boundary for retirement.
	role := component["route_policy"].(map[string]any)["roles"].([]any)[0].(map[string]any)
	role["candidates"].([]any)[0].(map[string]any)["effort"] = "high"
	if _, err := fleetProjection(c, component, fleetTestHost, "policy-second", false); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, policyRead(t, c)) {
		t.Fatal("update did not apply")
	}
	if err := FleetRestorePrior(c.Home, "policy-second"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, policyRead(t, c)) {
		t.Fatal("rollback lost exact last-good bytes")
	}
	delete(component, "route_policy")
	if _, err := fleetProjection(c, component, fleetTestHost, "policy-retire", false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, policyRead(t, c)) {
		t.Fatal("retirement lost original ownership boundary")
	}
	if _, err := fleetProjection(c, component, fleetTestHost, "policy-retire-again", false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, policyRead(t, c)) {
		t.Fatal("retirement is not idempotent")
	}
	adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), "operator edit after retirement")
	if _, err := fleetProjection(c, component, fleetTestHost, "policy-unowned-again", false); err != nil {
		t.Fatal(err)
	}
	if string(policyRead(t, c)) != "operator edit after retirement" {
		t.Fatal("tombstone retained authority")
	}
}

func TestFleetPolicyMissingKeyPreservesUnownedFile(t *testing.T) {
	c := fleetTestPrepare(t)
	path := c.Path(fleet.RoutePolicyDestination)
	adapterTestWrite(t, path, "unmanaged malformed policy")
	if _, err := fleetProjection(c, fleetTestComponent(t), fleetTestHost, "legacy", false); err != nil {
		t.Fatal(err)
	}
	if string(policyRead(t, c)) != "unmanaged malformed policy" {
		t.Fatal("legacy claimed policy")
	}
	if _, err := os.Stat(c.Path(fleetPolicyReceiptPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy created policy receipt")
	}
	if _, err := fleetProjection(c, nil, fleetTestHost, "legacy-retire", false); err != nil {
		t.Fatal(err)
	}
	if string(policyRead(t, c)) != "unmanaged malformed policy" {
		t.Fatal("legacy retirement removed unowned policy")
	}
}

func TestFleetPolicyRefusesBeforeLiveWritesAndPreservesLastGood(t *testing.T) {
	for _, failure := range []string{"schema", "catalog", "transport"} {
		t.Run(failure, func(t *testing.T) {
			c, component := policyContext(t)
			old := []byte("last known good policy")
			adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), string(old))
			if failure == "schema" {
				component["route_policy"].(map[string]any)["schema"] = "route-policy/v2"
			} else {
				c.Runner = func(cmd procx.Command) procx.Result {
					if failure == "transport" {
						return procx.Result{Code: 1, Stderr: "configured route catalog unavailable"}
					}
					return procx.Result{Code: 1, Stderr: "candidate is not catalog-authorized"}
				}
			}
			if _, err := fleetProjection(c, component, fleetTestHost, "policy-refused", false); err == nil {
				t.Fatal("accepted invalid policy")
			}
			if !bytes.Equal(old, policyRead(t, c)) {
				t.Fatal("validation changed last-good policy")
			}
			for _, path := range []string{fleet.WorkerDestination, fleet.ClientDestination, fleetPolicyReceiptPath, fleet.StateDirectory + "/policy-refused"} {
				if _, err := os.Stat(c.Path(path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("validation wrote %s (%v)", path, err)
				}
			}
		})
	}
}

func TestFleetPolicyStagedFailureRollsBackEveryProjection(t *testing.T) {
	c, component := policyContext(t)
	old := []byte("last-good")
	adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), string(old))
	calls := 0
	c.Runner = func(cmd procx.Command) procx.Result {
		if strings.Contains(strings.Join(cmd.Argv, " "), "policy validate") {
			calls++
			if calls == 2 {
				return procx.Result{Code: 1, Stderr: "staged candidate no longer catalog-authorized"}
			}
		}
		return policyFixtureRunner(t)(cmd)
	}
	_, err := fleetProjection(c, component, fleetTestHost, "policy-stage-failure", false)
	var rollback *RolledBack
	if !errors.As(err, &rollback) {
		t.Fatalf("want rollback, got %v", err)
	}
	if calls != 2 || !bytes.Equal(old, policyRead(t, c)) {
		t.Fatalf("validation calls %d; policy lost", calls)
	}
	for _, path := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
		if _, err := os.Stat(c.Path(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial activation %s", path)
		}
	}
	leftovers, _ := filepath.Glob(c.Path(".config/t3-steward/.route-policy.yaml-*"))
	if len(leftovers) > 0 {
		t.Fatal("staged file leaked")
	}
}

func TestFleetPolicyStagedTamperingAndWriteFailure(t *testing.T) {
	c, _ := policyContext(t)
	old := []byte("last-good")
	adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), string(old))
	planned, err := fleetPlanPolicy(c, []byte(policytest.Policy))
	if err != nil {
		t.Fatal(err)
	}
	staged, err := fleetStage(planned.target, planned.payload)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(staged)
	if err := os.WriteFile(staged, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fleetValidateStaged(staged, planned); err == nil {
		t.Fatal("accepted staged tampering")
	}
	if !bytes.Equal(old, policyRead(t, c)) {
		t.Fatal("tampering activated")
	}
	// An impossible destination tests the write-failure path and retained rollback.
	planned.target = c.Path("blocked/policy.yaml")
	adapterTestWrite(t, c.Path("blocked"), "not a directory")
	err = fleetWriteProjection(c, "policy-write-failure", []*fleetDocument{planned})
	var rollback *RolledBack
	if !errors.As(err, &rollback) || !bytes.Equal(old, policyRead(t, c)) {
		t.Fatalf("write failure: %v", err)
	}
}

func TestFleetPolicyRefusesOwnershipDriftAndSymlinks(t *testing.T) {
	c, component := policyContext(t)
	if _, err := fleetProjection(c, component, fleetTestHost, "policy-owned", false); err != nil {
		t.Fatal(err)
	}
	adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), "operator edit")
	delete(component, "route_policy")
	if _, err := fleetProjection(c, component, fleetTestHost, "policy-drift", false); err == nil {
		t.Fatal("retired drifted policy")
	}
	if string(policyRead(t, c)) != "operator edit" {
		t.Fatal("lost operator edit")
	}
	if err := os.Remove(c.Path(fleet.RoutePolicyDestination)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "external")
	adapterTestWrite(t, outside, "external")
	if err := os.Symlink(outside, c.Path(fleet.RoutePolicyDestination)); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetPlanPolicy(c, []byte(policytest.Policy)); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestFleetPolicyCoordinatorActivationFailureRestoresLastGood(t *testing.T) {
	for _, outcome := range []string{"rejected", "missing", "signal"} {
		t.Run(outcome, func(t *testing.T) {
			policytest.RequireCLI(t)
			policytest.Catalog(t, []string{"gpt-6.1-sol"})
			c, runner, fake := fleetTestCoordinatorContext(t, true)
			c.Env = os.Environ()
			if outcome == "rejected" {
				fake.receipt.Outcome = "rejected"
				fake.receipt.Error = "fixture catalog rejected"
			}
			if outcome == "missing" {
				fake.receipt = nil
			}
			c.Runner = func(cmd procx.Command) procx.Result {
				if len(cmd.Argv) > 0 && cmd.Argv[0] == "t3-steward" {
					return policyFixtureRunner(t)(cmd)
				}
				if outcome == "signal" && adapterTestMatches(cmd.Argv, "kill", "HUP") {
					return procx.Result{Code: 1, Stderr: "fixture signal failed"}
				}
				return runner.run(cmd)
			}
			component := fleetTestComponent(t)
			var policy map[string]any
			if err := json.Unmarshal([]byte(policytest.Policy), &policy); err != nil {
				t.Fatal(err)
			}
			component["route_policy"] = policy
			old := []byte("last-good")
			adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), string(old))
			if _, err := fleetProjection(c, component, c.Host.Name, "policy-reload-"+outcome, false); err == nil {
				t.Fatal("activation failure accepted")
			}
			if !bytes.Equal(old, policyRead(t, c)) {
				t.Fatal("activation failure left new policy")
			}
			r, err := fleetReadPolicyReceipt(c)
			if err != nil {
				t.Fatal(err)
			}
			if !fleetReceiptMatches(r, old) {
				t.Fatal("activation rollback invalidated ownership")
			}
		})
	}
}

func TestFleetPolicyRetirementFailureRollsBackPolicyAndClient(t *testing.T) {
	c, component := policyContext(t)
	if _, err := fleetProjection(c, component, fleetTestHost, "retirement-start", false); err != nil {
		t.Fatal(err)
	}
	policyBefore := policyRead(t, c)
	clientBefore, err := os.ReadFile(c.Path(fleet.ClientDestination))
	if err != nil {
		t.Fatal(err)
	}
	// An owned coordinator projection left by a previous host role is eligible
	// for retirement, but its retained-run directory collision makes the actual
	// retirement fail after client/policy retirement has already succeeded.
	adapterTestWrite(t, c.Path(fleet.CoordinatorDestination), string(fleetTestFixture(t, "projection-coordinator.json")))
	if err := os.Chmod(c.Path(fleet.CoordinatorDestination), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.Path(fleet.StateDirectory, "retirement-collision"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = fleetProjection(c, nil, fleetTestHost, "retirement-collision", false)
	var rolledBack *RolledBack
	if !errors.As(err, &rolledBack) {
		t.Fatalf("want rollback: %v", err)
	}
	clientAfter, err := os.ReadFile(c.Path(fleet.ClientDestination))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(policyBefore, policyRead(t, c)) || !bytes.Equal(clientBefore, clientAfter) {
		t.Fatal("partial retirement")
	}
	r, err := fleetReadClientReceipt(c)
	if err != nil || !fleetReceiptMatches(r, clientAfter) {
		t.Fatalf("client receipt mismatch: %v", err)
	}
}

func TestFleetPolicyComponentRetirementRemovesOnlyOwnedPolicy(t *testing.T) {
	c, component := policyContext(t)
	if _, err := fleetProjection(c, component, fleetTestHost, "policy-new", false); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetProjection(c, nil, fleetTestHost, "policy-component-retire", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Path(fleet.RoutePolicyDestination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned policy not retired")
	}
}
