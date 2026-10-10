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

// This reproduces the clientless preflight boundary, then exercises the same
// projection command path with the intended client. Only transport is isolated;
// the installed Steward policy parser and configured catalog validator execute.
func TestFleetPolicyClientlessAgentsUseIntendedAuthorization(t *testing.T) {
	for _, host := range []string{"agent-a", "agent-b"} {
		t.Run(host, func(t *testing.T) {
			for _, outcome := range []string{"authorized", "unauthorized", "effect-then-42"} {
				t.Run(outcome, func(t *testing.T) {
					c, component := policyContext(t)
					c.Host.Name = host
					hosts := component["hosts"].(map[string]any)
					raw, _ := json.Marshal(hosts[fleetTestHost])
					var entry map[string]any
					if err := json.Unmarshal(raw, &entry); err != nil {
						t.Fatal(err)
					}
					entry["worker_id"] = host
					entry["credential_ref"] = "secretref:f02-protocol/" + host
					entry["admin_credential_ref"] = "secretref:f03-admin/" + host
					hosts[host] = entry
					for _, ref := range []string{"f02-protocol/" + host, "f03-admin/" + host} {
						path := c.Path(workerSecretRoot, ref)
						adapterTestWrite(t, path, "credential-not-for-records")
						if err := os.Chmod(path, 0600); err != nil {
							t.Fatal(err)
						}
					}
					if outcome == "unauthorized" {
						policytest.Catalog(t, []string{"different-model"})
						c.Env = os.Environ()
					}
					calls := 0
					var stagedPaths []string
					fixture := policyFixtureRunner(t)
					c.Runner = func(cmd procx.Command) procx.Result {
						if !strings.Contains(strings.Join(cmd.Argv, " "), "policy validate") {
							return fixture(cmd)
						}
						calls++
						index := -1
						for i, arg := range cmd.Argv {
							if arg == "--config" {
								index = i
							}
						}
						if index < 0 {
							return procx.Result{Code: 3, Stderr: "client-configuration: absent client"}
						}
						path := cmd.Argv[index+1]
						stagedPaths = append(stagedPaths, filepath.Dir(path))
						info, err := os.Stat(path)
						if err != nil || info.Mode().Perm() != 0600 {
							t.Fatalf("private config: %v", err)
						}
						dirInfo, err := os.Stat(filepath.Dir(path))
						if err != nil || dirInfo.Mode().Perm() != 0700 {
							t.Fatalf("private staging: %v", err)
						}
						body, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						var config map[string]any
						if err := json.Unmarshal(body, &config); err != nil {
							t.Fatal(err)
						}
						client := config["backlog_v2"].(map[string]any)["coordinator_client"].(map[string]any)
						if client["credential"] != "secretref:f03-admin/"+host || client["coordinator_id"] != "normandy-coordinator" || client["address"] != "normandy" {
							t.Fatalf("client identity/reference changed: %v", client)
						}
						if bytes.Contains(body, []byte("credential-not-for-records")) {
							t.Fatal("secret copied")
						}
						if calls == 2 {
							for _, dest := range []string{fleet.ClientDestination, fleet.WorkerDestination, fleetPolicyReceiptPath} {
								if _, err := os.Stat(c.Path(dest)); !errors.Is(err, os.ErrNotExist) {
									t.Fatalf("baseline preflight wrote %s", dest)
								}
							}
						}
						result := fixture(cmd)
						if outcome == "effect-then-42" && result.OK() {
							result.Code = 42
						}
						return result
					}
					// Exact original call has no intended client and refuses before effects.
					if _, err := fleetPlanPolicy(c, []byte(policytest.Policy)); err == nil || !strings.Contains(err.Error(), "absent client") {
						t.Fatalf("baseline boundary: %v", err)
					}
					result, err := fleetProjection(c, component, host, "clientless-"+outcome, false)
					if outcome == "authorized" {
						if err != nil || result["enrolled"] != false {
							t.Fatalf("repaired projection: %v %v", result, err)
						}
						receipt, err := fleetReadClientReceipt(c)
						live, readErr := os.ReadFile(c.Path(fleet.ClientDestination))
						if err != nil || readErr != nil || !fleetReceiptMatches(receipt, live) {
							t.Fatalf("client receipt: %v %v", err, readErr)
						}
						if err := FleetRestorePrior(c.Home, "clientless-"+outcome); err != nil {
							t.Fatal(err)
						}
					} else if err == nil {
						t.Fatal("unauthorized/nonzero validation passed")
					}
					if calls != 2 && outcome != "authorized" {
						t.Fatalf("unexpected retries: %d", calls)
					}
					for _, path := range stagedPaths {
						if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("stage leaked: %v", err)
						}
					}
					for _, dest := range []string{fleet.ClientDestination, fleet.WorkerDestination, fleet.RoutePolicyDestination} {
						if _, err := os.Stat(c.Path(dest)); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("preflight/rollback left %s: %v", dest, err)
						}
					}
				})
			}
		})
	}
}
