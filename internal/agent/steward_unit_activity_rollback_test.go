package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// A successful restart changes activity, unlike the older fixed host fixture.
// Exercise the actual Steward adapter through Execute; only the independent
// later component is a stub, so continuation and abort are observable.
func TestStewardUnitMigrationRestoresPriorActivity(t *testing.T) {
	for _, priorActive := range []bool{false, true} {
		for _, mode := range []string{"shadowed", "activation-failure", "restore-refusal", "restore-ignored", "restore-query-failure", "restore-query-unknown", "restore-byte-drift", "restore-reload-failure", "snapshot-failure"} {
			label := "inactive/" + mode
			if priorActive {
				label = "active/" + mode
			}
			t.Run(label, func(t *testing.T) {
				later, _ := newPartialRun(t, map[string][]string{}, "later")
				c, runner := stewardTestPrepare(t, stewardTestHost{active: priorActive})
				c.lockDirectory = c.Path(".local/state/dev-fleet/test-lock")
				spec := stewardTestSpec(false)
				c.Components["t3-steward"] = spec
				c.Components["later"] = map[string]any{}
				c.Selected = []string{"t3-steward", "later"}
				adapterTestWrite(t, c.Path(".local/bin/t3-steward"), stewardTestBinary)
				p := c.Path(stewardUnitPath)
				adapterTestWrite(t, p, stewardLegacyUnitFixture)
				base := runner.handle
				active := priorActive
				restarts, stops, reloads, restores, proofQueries := 0, 0, 0, 0, 0
				migrationStarted := false
				runner.handle = func(cmd procx.Command) procx.Result {
					switch {
					case adapterTestMatches(cmd.Argv, "is-active", "t3-steward.service"):
						if !migrationStarted && mode == "snapshot-failure" {
							return procx.Result{Code: 1, Stderr: "query refused"}
						}
						if restores > 0 {
							proofQueries++
							if mode == "restore-query-failure" {
								return procx.Result{Code: 1, Stderr: "query refused"}
							}
							if mode == "restore-query-unknown" {
								return procx.Result{Code: 3, Stdout: "deactivating"}
							}
						}
						if active {
							return procx.Result{Stdout: "active"}
						}
						return procx.Result{Code: 3, Stdout: "inactive"}
					case adapterTestMatches(cmd.Argv, "daemon-reload"):
						reloads++
						migrationStarted = true
						if reloads == 2 && mode == "restore-reload-failure" {
							return procx.Result{Code: 1, Stderr: "reload refused"}
						}
						return procx.Result{}
					case adapterTestMatches(cmd.Argv, "restart", "t3-steward.service"):
						restarts++
						if restarts == 1 {
							// Failed activation can still have changed service state.
							active = !priorActive
							if mode == "activation-failure" {
								return procx.Result{Code: 1, Stderr: "activation failed"}
							}
							active = true
							return procx.Result{}
						}
						fallthrough
					case adapterTestMatches(cmd.Argv, "stop", "t3-steward.service"):
						if adapterTestMatches(cmd.Argv, "stop") {
							stops++
						}
						restores++
						if mode == "restore-refusal" {
							return procx.Result{Code: 1, Stderr: "restoration refused"}
						}
						if mode == "restore-ignored" {
							active = !priorActive
						} else {
							active = priorActive
						}
						if mode == "restore-byte-drift" {
							adapterTestWrite(t, p, stewardLegacyUnitFixture+"# concurrent change\n")
						}
						return procx.Result{}
					case adapterTestMatches(cmd.Argv, "--property=ProtectSystem"):
						return procx.Result{Stdout: "full"}
					default:
						return base(cmd)
					}
				}
				result := Execute(c)
				entry := resultEntry(t, result, "t3-steward")
				wantState := "failed"
				incomplete := strings.HasPrefix(mode, "restore-")
				if incomplete {
					wantState = "rollback-failed"
				}
				if entry["state"] != wantState || result["result"] != wantState {
					t.Fatalf("state: entry=%v result=%v", entry, result)
				}
				body, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "restore-byte-drift" && string(body) != stewardLegacyUnitFixture {
					t.Fatal("original bytes not restored")
				}
				if incomplete {
					if !strings.Contains(entryText(entry, "error"), "rollback failed") || later.calls["later"] != 0 || resultEntry(t, result, "later")["aborted_by"] != "t3-steward" {
						t.Fatalf("incomplete restoration continued: %v calls=%v", result, later.calls)
					}
				} else {
					if active != priorActive || later.calls["later"] == 0 || resultEntry(t, result, "later")["state"] != "updated" {
						t.Fatalf("restoration/continuation: active=%v result=%v", active, result)
					}
					if mode != "snapshot-failure" && proofQueries < 2 {
						t.Fatal("restoration was not explicitly proved")
					}
				}
				if mode == "snapshot-failure" {
					if restarts+stops+reloads != 0 {
						t.Fatal("effect before activity snapshot")
					}
					if _, err := os.Stat(c.Path(stewardUnitPending)); !os.IsNotExist(err) {
						t.Fatal("pending receipt written before snapshot")
					}
				} else if mode != "restore-reload-failure" {
					if restores != 1 || (priorActive && (restarts != 2 || stops != 0)) || (!priorActive && (restarts != 1 || stops != 1)) {
						t.Fatalf("wrong activity restoration: restart=%d stop=%d restore=%d", restarts, stops, restores)
					}
				}
				t.Logf("prior_active=%v final_active=%v state=%s restarts=%d stops=%d proof_queries=%d later_calls=%d", priorActive, active, wantState, restarts, stops, proofQueries, later.calls["later"])
			})
		}
	}
}
