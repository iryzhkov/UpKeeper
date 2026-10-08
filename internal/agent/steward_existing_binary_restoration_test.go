package agent

import (
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"os"
	"strings"
	"testing"
)

// Use the real adapter and controller with stateful service replies.
func TestExistingStewardBinaryRestoration(t *testing.T) {
	for _, priorActive := range []bool{false, true} {
		for _, mode := range []string{"restored", "no-unit", "unknown-prior", "restore-refused", "restore-ignored", "restore-unknown", "binary-drift", "mode-drift", "hook-drift", "hook-mode-drift", "hook-refused"} {
			label := "inactive/" + mode
			if priorActive {
				label = "active/" + mode
			}
			t.Run(label, func(t *testing.T) {
				later, _ := newPartialRun(t, map[string][]string{}, "later")
				c, runner := stewardTestPrepare(t, stewardTestHost{active: priorActive})
				c.lockDirectory = c.Path(".local/state/dev-fleet/test-lock")
				spec := stewardTestSpec(true)
				c.Components["t3-steward"] = spec
				c.Components["later"] = map[string]any{}
				c.Selected = []string{"t3-steward", "later"}
				binary := c.Path(".local/bin/t3-steward")
				previous := stewardTestBinary + "# previous\n"
				adapterTestWrite(t, binary, previous)
				if err := os.Chmod(binary, 0750); err != nil {
					t.Fatal(err)
				}
				hook := stewardSessionHook(c)
				adapterTestWrite(t, hook, "previous hook\n")
				if err := os.Chmod(hook, 0600); err != nil {
					t.Fatal(err)
				}
				if mode != "no-unit" {
					adapterTestWrite(t, c.Path(stewardUnitPath), strings.Replace(stewardLegacyUnitFixture, "ProtectSystem=full", "ProtectSystem=false", 1))
				}
				archive := stewardTestArchive(t, []byte(stewardTestBinary), []byte(stewardTestHook), false)
				stewardTestServe(t, archive, stewardTestDigest(archive))
				base := runner.handle
				active := priorActive
				restarts, stops, restores := 0, 0, 0
				runner.handle = func(cmd procx.Command) procx.Result {
					switch {
					case adapterTestMatches(cmd.Argv, "cat", "t3-steward.service") && mode == "no-unit":
						return procx.Result{Code: 1, Stderr: "No files found for t3-steward.service"}
					case adapterTestMatches(cmd.Argv, "is-active", "t3-steward.service"):
						if mode == "unknown-prior" || (restores > 0 && mode == "restore-unknown") {
							return procx.Result{Code: 3, Stdout: "deactivating"}
						}
						if active {
							return procx.Result{Stdout: "active"}
						}
						return procx.Result{Code: 3, Stdout: "inactive"}
					case adapterTestMatches(cmd.Argv, "restart", "t3-steward.service"):
						restarts++
						if restarts == 1 {
							active = true
							return procx.Result{}
						}
						fallthrough
					case adapterTestMatches(cmd.Argv, "stop", "t3-steward.service"):
						if adapterTestMatches(cmd.Argv, "stop") {
							stops++
						}
						restores++
						if mode == "restore-refused" {
							return procx.Result{Code: 1, Stderr: "restoration refused"}
						}
						active = priorActive
						if mode == "restore-ignored" {
							active = !priorActive
						}
						if mode == "binary-drift" {
							adapterTestWrite(t, binary, "changed")
						}
						if mode == "mode-drift" {
							if err := os.Chmod(binary, 0700); err != nil {
								t.Fatal(err)
							}
						}
						if mode == "hook-drift" {
							adapterTestWrite(t, hook, "changed")
						}
						if mode == "hook-mode-drift" {
							if err := os.Chmod(hook, 0644); err != nil {
								t.Fatal(err)
							}
						}
						return procx.Result{}
					case len(cmd.Argv) > 1 && cmd.Argv[0] == binary && cmd.Argv[1] == "version" && (restarts == 1 || mode == "no-unit"):
						if mode == "hook-refused" && restores == 0 {
							if err := os.Remove(hook); err != nil {
								t.Fatal(err)
							}
							adapterTestWrite(t, hook+"/child", "retained")
						}
						return procx.Result{Code: 1, Stderr: "injected verification refusal"}
					default:
						return base(cmd)
					}
				}
				result := Execute(c)
				entry := resultEntry(t, result, "t3-steward")
				incomplete := strings.HasPrefix(mode, "restore-") || strings.HasSuffix(mode, "drift") || mode == "hook-refused"
				want := "rolled-back"
				if mode == "unknown-prior" {
					want = "failed"
				}
				if incomplete {
					want = "rollback-failed"
				}
				if entry["state"] != want {
					t.Fatalf("restoration classification: got=%v want=%s", entry["state"], want)
				}
				if incomplete {
					if later.calls["later"] != 0 || resultEntry(t, result, "later")["aborted_by"] != "t3-steward" {
						t.Fatal("incomplete restoration continued")
					}
				} else {
					if later.calls["later"] == 0 {
						t.Fatal("safe restoration prevented independent continuation")
					}
					if active != priorActive {
						t.Fatal("prior activity not restored")
					}
				}
				if mode != "binary-drift" {
					body, err := os.ReadFile(binary)
					if err != nil || string(body) != previous {
						t.Fatal("original binary bytes not restored")
					}
				}
				if mode != "mode-drift" {
					info, err := os.Stat(binary)
					if err != nil || info.Mode().Perm() != 0750 {
						t.Fatal("original binary mode not restored")
					}
				}
				if mode != "hook-drift" && mode != "hook-mode-drift" && mode != "hook-refused" {
					body, err := os.ReadFile(hook)
					info, statErr := os.Stat(hook)
					if err != nil || statErr != nil || string(body) != "previous hook\n" || info.Mode().Perm() != 0600 {
						t.Fatal("original hook not restored")
					}
				}
				if mode == "unknown-prior" || mode == "no-unit" {
					if restarts+stops != 0 {
						t.Fatal("unexpected service effect")
					}
				} else if restores != 1 || (!priorActive && (restarts != 1 || stops != 1)) || (priorActive && (restarts != 2 || stops != 0)) {
					t.Fatal("wrong restoration action")
				}
			})
		}
	}
}
