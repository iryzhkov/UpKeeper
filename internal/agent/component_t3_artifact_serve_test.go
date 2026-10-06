package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const t3ServeArgs = "serve --host 127.0.0.1 --port 7391 --no-browser"

// Exercise the same configured and kernel invocation through the real handler,
// with fixture acquisition/process evidence and independent health/idle reports.
func t3ServeHost(t *testing.T, mode, args string, fail bool) (*Context, map[string]any, *adapterTestRunner, string, *int) {
	t.Helper()
	c, spec, runner, prefix := t3ArtifactHost(t, false, fail, 0, 0)
	native := filepath.Join(prefix, "lib/node_modules/t3/node_modules/@t3code/t3-"+release.T3Target()+"/t3")
	path := filepath.Join(prefix, "bin/t3")
	mise := c.Path("owned/mise")
	if mode == "native" {
		adapterTestWrite(t, native, "previous-native")
		if e := os.Chmod(native, 0755); e != nil {
			t.Fatal(e)
		}
		path = native
	}
	if mode == "shim" {
		adapterTestWrite(t, mise, "mise")
		path = c.Path(".local/share/mise/shims/t3")
		if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(mise, path); e != nil {
			t.Fatal(e)
		}
	}
	original := runner.handle
	restarts := new(int)
	runner.handle = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "ExecStart") {
			return procx.Result{Stdout: t3BindingRecord(path, args)}
		}
		if len(cmd.Argv) == 3 && cmd.Argv[0] == mise && cmd.Argv[1] == "which" {
			return procx.Result{Stdout: filepath.Join(prefix, "bin/t3")}
		}
		r := original(cmd)
		if adapterTestMatches(cmd.Argv, "restart") {
			*restarts++
			if r.Code == 0 {
				if _, e := os.Stat(filepath.Join(t3ProcRoot, "124/cmdline")); e == nil {
					adapterTestWrite(t, filepath.Join(t3ProcRoot, "124/cmdline"), native+"\x00"+strings.ReplaceAll(args, " ", "\x00")+"\x00")
				}
			}
		}
		return r
	}
	return c, spec, runner, prefix, restarts
}

func TestT3ServeInstalledConvergedAndRecovery(t *testing.T) {
	for _, mode := range []string{"native", "launcher", "shim"} {
		for _, args := range []string{"start", t3ServeArgs} {
			for _, fail := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/recovery=%v", mode, strings.Fields(args)[0], fail), func(t *testing.T) {
					c, spec, _, prefix, restarts := t3ServeHost(t, mode, args, fail)
					root := filepath.Join(prefix, "lib/node_modules/t3")
					old, e := release.T3TreeDigest(root)
					if e != nil {
						t.Fatal(e)
					}
					oldLink, e := os.Readlink(filepath.Join(prefix, "bin/t3"))
					if e != nil {
						t.Fatal(e)
					}
					before, e := t3Observe(c, spec)
					if e != nil {
						t.Fatal(e)
					}
					actions := []string{}
					status, e := t3Apply(c, spec, before, &actions)
					if fail {
						var rollback *RolledBack
						now, err := release.T3TreeDigest(root)
						link, linkErr := os.Readlink(filepath.Join(prefix, "bin/t3"))
						if !errors.As(e, &rollback) || *restarts != 2 || err != nil || now != old || linkErr != nil || link != oldLink {
							t.Fatalf("recovery: %s %v restarts=%d restored=%v", status, e, *restarts, now == old)
						}
						return
					}
					if e != nil || status != "updated" || *restarts != 1 {
						t.Fatalf("install: %s %v restarts=%d", status, e, *restarts)
					}
					after, e := t3Observe(c, spec)
					if e != nil || after["artifact_installed"] != true || after["artifact_served"] != true || after["service_active"] != true || after["check_ok"] != true {
						t.Fatalf("serving identity: %v %v", after, e)
					}
					status, e = t3Apply(c, spec, after, &actions)
					if e != nil || status != "converged" || *restarts != 1 {
						t.Fatalf("convergence: %s %v restarts=%d", status, e, *restarts)
					}
				})
			}
		}
	}
}

func TestT3ServeRetainedUpstream(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			c, spec, runner, prefix := t3RepairUpstreamHost(t)
			native := filepath.Join(prefix, "lib/node_modules/t3/node_modules/@t3code/t3-"+release.T3Target()+"/t3")
			exe := filepath.Join(t3ProcRoot, "124/exe")
			adapterTestWrite(t, filepath.Join(t3ProcRoot, "124/cmdline"), native+"\x00"+strings.ReplaceAll(t3ServeArgs, " ", "\x00")+"\x00")
			if stale {
				if e := os.Remove(exe); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(native+" (deleted)", exe); e != nil {
					t.Fatal(e)
				}
			}
			original := runner.handle
			restarts := 0
			runner.handle = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "ExecStart") {
					return procx.Result{Stdout: t3BindingRecord(native, t3ServeArgs)}
				}
				if adapterTestMatches(cmd.Argv, "restart") {
					restarts++
					if e := os.Remove(exe); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink(native, exe); e != nil {
						t.Fatal(e)
					}
				}
				return original(cmd)
			}
			before, e := t3Observe(c, spec)
			if e != nil {
				t.Fatal(e)
			}
			if before["artifact_installed"] != true || (before["artifact_served"] == true) == stale {
				t.Fatalf("upstream identity: %v", before)
			}
			actions := []string{}
			got, e := t3Apply(c, spec, before, &actions)
			want, n := "converged", 0
			if stale {
				want, n = "updated", 1
			}
			if e != nil || got != want || restarts != n {
				t.Fatalf("%s %v restarts=%d", got, e, restarts)
			}
			after, e := t3Observe(c, spec)
			if e != nil || after["artifact_served"] != true {
				t.Fatalf("upstream recovery: %v %v", after, e)
			}
			got, e = t3Apply(c, spec, after, &actions)
			if e != nil || got != "converged" || restarts != n {
				t.Fatalf("upstream convergence: %s %v", got, e)
			}
		})
	}
}

func TestT3ServeKernelAndHealthRefusals(t *testing.T) {
	for _, state := range []string{"wrong-command", "unknown-serve-args", "unknown-start-args", "foreign-native", "old-inode", "hash", "changed-pid", "duplicate-native", "failed-health", "busy", "config-drift"} {
		t.Run(state, func(t *testing.T) {
			c, spec, runner, prefix, restarts := t3ServeHost(t, "launcher", t3ServeArgs, false)
			actions := []string{}
			before, _ := t3Observe(c, spec)
			if got, e := t3Apply(c, spec, before, &actions); e != nil || got != "updated" {
				t.Fatalf("setup: %s %v", got, e)
			}
			native := filepath.Join(prefix, "lib/node_modules/t3/node_modules/@t3code/t3-"+release.T3Target()+"/t3")
			exe := filepath.Join(t3ProcRoot, "124/exe")
			cmdline := filepath.Join(t3ProcRoot, "124/cmdline")
			original := runner.handle
			mainReads := 0
			runner.handle = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "MainPID") {
					mainReads++
					if mainReads == 2 {
						if state == "changed-pid" {
							return procx.Result{Stdout: "125"}
						}
					}
				}
				if state == "config-drift" && adapterTestMatches(cmd.Argv, "ExecStart") {
					return procx.Result{Stdout: t3BindingRecord("/foreign/t3", t3ServeArgs)}
				}
				if cmd.Argv[len(cmd.Argv)-1] == "check" {
					if state == "failed-health" {
						return procx.Result{Code: 1}
					}
					if state == "busy" {
						return procx.Result{Stdout: "T3 server 0.0.45 serving\nshell snapshot: 4 threads, 2 running"}
					}
				}
				return original(cmd)
			}
			switch state {
			case "wrong-command":
				adapterTestWrite(t, cmdline, native+"\x00update\x00")
			case "unknown-serve-args":
				adapterTestWrite(t, cmdline, native+"\x00serve\x00--eval\x00anything\x00")
			case "unknown-start-args":
				adapterTestWrite(t, cmdline, native+"\x00start\x00--eval\x00anything\x00")
			case "foreign-native":
				foreign := c.Path("foreign/t3")
				adapterTestWrite(t, foreign, "foreign")
				if e := os.Remove(exe); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(foreign, exe); e != nil {
					t.Fatal(e)
				}
			case "old-inode":
				old := native + ".old"
				if e := os.Rename(native, old); e != nil {
					t.Fatal(e)
				}
				b, e := os.ReadFile(old)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(native, b, 0755); e != nil {
					t.Fatal(e)
				}
				if e = os.Remove(exe); e != nil {
					t.Fatal(e)
				}
				if e = os.Symlink(old+" (deleted)", exe); e != nil {
					t.Fatal(e)
				}
			case "hash":
				adapterTestWrite(t, native, "tampered-native")
			case "duplicate-native":
				t3FakeProcess(t, "125", native)
				adapterTestWrite(t, filepath.Join(t3ProcRoot, "124/task/124/children"), "124 125")
			}
			if state == "changed-pid" {
				retained, err := release.ReadT3Retained(c.Home)
				if err != nil {
					t.Fatal(err)
				}
				if t3ServedArtifact(c, spec, retained) {
					t.Fatal("changed MainPID accepted")
				}
				return
			}
			if state == "busy" {
				adapterTestWrite(t, cmdline, native+"\x00update\x00")
			}
			after, e := t3Observe(c, spec)
			if e != nil {
				t.Fatal(e)
			}
			if state == "failed-health" {
				if after["check_ok"] == true {
					t.Fatal("failed health accepted")
				}
			} else if state == "busy" {
				got, e := t3Apply(c, spec, after, &actions)
				if e != nil || got != "deferred" || *restarts != 1 {
					t.Fatalf("busy: %s %v restarts=%d", got, e, *restarts)
				}
			} else if after["artifact_served"] == true {
				t.Fatalf("unsafe serving identity: %v", after)
			}
		})
	}
}

func TestT3ServeChangedProcessLifetime(t *testing.T) {
	for _, pid := range []string{"123", "124"} {
		t.Run(pid, func(t *testing.T) {
			c, spec, runner, prefix, _ := t3ServeHost(t, "launcher", t3ServeArgs, false)
			before, _ := t3Observe(c, spec)
			actions := []string{}
			if _, e := t3Apply(c, spec, before, &actions); e != nil {
				t.Fatal(e)
			}
			retained, e := release.ReadT3Retained(c.Home)
			if e != nil {
				t.Fatal(e)
			}
			// Bind a launcher MainPID to its native child, and change exactly one
			// lifetime between the predicate's initial and final stat samples.
			adapterTestWrite(t, filepath.Join(t3ProcRoot, "123/task/123/children"), "124")
			original := runner.handle
			runner.handle = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "MainPID") {
					return procx.Result{Stdout: "123"}
				}
				return original(cmd)
			}
			stat := filepath.Join(t3ProcRoot, pid, "stat")
			old, e := os.ReadFile(stat)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Remove(stat); e != nil {
				t.Fatal(e)
			}
			if e = syscall.Mkfifo(stat, 0600); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() {
				f, err := os.OpenFile(stat, os.O_WRONLY, 0600)
				if err != nil {
					done <- err
					return
				}
				// The reader has opened the FIFO. Replace only its pathname; its
				// already-open descriptor receives the old sample, the next read the new.
				err = os.Remove(stat)
				if err == nil {
					err = os.WriteFile(stat, []byte(pid+" (t3) S "+strings.Repeat("0 ", 18)+"456\n"), 0600)
				}
				if err == nil {
					_, err = f.Write(old)
				}
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
				done <- err
			}()
			served := t3ServedArtifact(c, spec, retained)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if served {
				t.Fatalf("changed PID%s lifetime accepted under %s", pid, prefix)
			}
		})
	}
}
