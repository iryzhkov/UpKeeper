package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func TestT3RepairJournalOpenFailureHandler(t *testing.T) {
	c, spec, runner, prefix := t3ArtifactHost(t, false, false, 0, 0)
	original := runner.handle
	injected := false
	runner.handle = func(cmd procx.Command) procx.Result {
		r := original(cmd)
		if adapterTestMatches(cmd.Argv, t3QualificationSupervisorArg) {
			if _, e := os.Stat(filepath.Join(prefix, "lib/node_modules/t3/node_modules")); e == nil {
				journals, e := filepath.Glob(filepath.Join(prefix, "lib/node_modules/.upkeeper-t3-*/transaction.json"))
				if e != nil || len(journals) != 1 {
					t.Fatalf("journal discovery: %v %v", journals, e)
				}
				if e = os.Remove(journals[0]); e != nil {
					t.Fatal(e)
				}
				if e = os.Mkdir(journals[0], 0700); e != nil {
					t.Fatal(e)
				}
				injected = true
			}
		}
		return r
	}
	before, _ := t3Observe(c, spec)
	actions := []string{}
	got, e := t3Apply(c, spec, before, &actions)
	if !injected || e == nil {
		t.Fatalf("ignored actual journal open failure: %s %v", got, e)
	}
}

func TestT3RepairUnitMappingBeforeEffects(t *testing.T) {
	for _, when := range []string{"initial", "staged", "activated", "restoration"} {
		t.Run(when, func(t *testing.T) {
			c, spec, runner, prefix := t3ArtifactHost(t, false, false, 0, 0)
			root := filepath.Join(prefix, "lib/node_modules/t3")
			old, _ := release.T3TreeDigest(root)
			original := runner.handle
			foreign := when == "initial"
			restarts := 0
			runner.handle = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "ExecStart") && foreign {
					return procx.Result{Stdout: "{ path=/foreign/t3 ; argv[]=t3 start ; }"}
				}
				r := original(cmd)
				if adapterTestMatches(cmd.Argv, t3QualificationSupervisorArg) {
					_, e := os.Stat(filepath.Join(root, "node_modules"))
					if (when == "staged" && e != nil) || (when == "activated" && e == nil) {
						foreign = true
					}
				}
				if adapterTestMatches(cmd.Argv, "restart") {
					restarts++
					if when == "restoration" {
						foreign = true
						return procx.Result{Code: 1, Stderr: "injected restart failure"}
					}
				}
				return r
			}
			before, _ := t3Observe(c, spec)
			actions := []string{}
			_, e := t3Apply(c, spec, before, &actions)
			if e == nil {
				t.Fatal("accepted unsupported mapping")
			}
			want := 0
			if when == "restoration" {
				want = 1
			}
			if restarts != want {
				t.Fatalf("restarted foreign unit: %d", restarts)
			}
			digest, _ := release.T3TreeDigest(root)
			if digest != old {
				t.Fatal("old files not preserved/restored")
			}
		})
	}
}

func t3RepairUpstreamHost(t *testing.T) (*Context, map[string]any, *adapterTestRunner, string) {
	t.Helper()
	c, spec, runner, prefix := t3ArtifactHost(t, false, false, 0, 0)
	before, _ := t3Observe(c, spec)
	actions := []string{}
	if _, e := t3Apply(c, spec, before, &actions); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(prefix, "lib/node_modules/t3")
	body, _ := os.ReadFile(filepath.Join(root, "package.json"))
	var pkg map[string]any
	_ = json.Unmarshal(body, &pkg)
	delete(pkg, "upkeeperControlledBuild")
	body, _ = json.Marshal(pkg)
	if e := os.WriteFile(filepath.Join(root, "package.json"), body, 0644); e != nil {
		t.Fatal(e)
	}
	state := c.Path(release.T3StatePath, "current")
	if e := os.RemoveAll(state); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(state, 0700); e != nil {
		t.Fatal(e)
	}
	digest, _ := release.T3TreeDigest(root)
	body, _ = json.Marshal(map[string]any{"schema": "upkeeper-t3-upstream/v1", "prefix": prefix, "version": "0.0.45", "tree_sha256": digest, "link": "../lib/node_modules/t3/bin/t3.js"})
	if e := os.WriteFile(filepath.Join(state, "upstream.json"), body, 0600); e != nil {
		t.Fatal(e)
	}
	delete(spec, "artifact")
	return c, spec, runner, prefix
}

func TestT3RepairRetainedUpstreamServingSplit(t *testing.T) {
	for _, state := range []string{"healthy", "stale", "busy", "failed-check", "wrong-unit"} {
		t.Run(state, func(t *testing.T) {
			c, spec, runner, prefix := t3RepairUpstreamHost(t)
			native := filepath.Join(prefix, "lib/node_modules/t3/node_modules/@t3code/t3-"+release.T3Target()+"/t3")
			exe := filepath.Join(t3ProcRoot, "124/exe")
			if state == "stale" || state == "busy" {
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
				if adapterTestMatches(cmd.Argv, "restart") {
					restarts++
					if e := os.Remove(exe); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink(native, exe); e != nil {
						t.Fatal(e)
					}
				}
				if adapterTestMatches(cmd.Argv, "ExecStart") && state == "wrong-unit" {
					return procx.Result{Stdout: "{ path=/foreign/t3 ; }"}
				}
				if cmd.Argv[len(cmd.Argv)-1] == "check" {
					if state == "busy" {
						return procx.Result{Stdout: "T3 server 0.0.45 serving\nshell snapshot: 4 threads, 2 running"}
					}
					if state == "failed-check" {
						return procx.Result{Code: 1, Stdout: "T3 server 0.0.45 serving\nshell snapshot: 4 threads, 0 running"}
					}
				}
				return original(cmd)
			}
			before, _ := t3Observe(c, spec)
			actions := []string{}
			got, e := t3Apply(c, spec, before, &actions)
			switch state {
			case "healthy":
				if e != nil || got != "converged" || restarts != 0 {
					t.Fatalf("%s %v %d", got, e, restarts)
				}
			case "stale":
				if e != nil || got != "updated" || restarts != 1 {
					t.Fatalf("stale serving identity bypass: %s %v %d", got, e, restarts)
				}
			case "busy", "failed-check":
				if e != nil || got != "deferred" || restarts != 0 {
					t.Fatalf("idle/health bypass: %s %v %d", got, e, restarts)
				}
			case "wrong-unit":
				if e == nil || restarts != 0 {
					t.Fatalf("unit bypass: %s %v %d", got, e, restarts)
				}
			}
			for _, cmd := range runner.calls {
				if adapterTestMatches(cmd, "npm", "install") {
					t.Fatal("reacquired retained upstream files")
				}
			}
		})
	}
}

type t3RepairJournalFile struct {
	*os.File
	op string
}

func (f *t3RepairJournalFile) Write(b []byte) (int, error) {
	if f.op == "write" {
		_, _ = f.File.Write(b[:len(b)/2])
		return len(b) / 2, errors.New("injected write failure")
	}
	if f.op == "short-write" {
		return len(b) / 2, nil
	}
	return f.File.Write(b)
}
func (f *t3RepairJournalFile) Sync() error {
	if f.op == "sync" || f.op == "directory-sync" {
		return errors.New("injected sync failure")
	}
	return f.File.Sync()
}
func (f *t3RepairJournalFile) Close() error {
	e := f.File.Close()
	if f.op == "close" || f.op == "directory-close" {
		return errors.New("injected close failure")
	}
	return e
}

func TestT3RepairJournalAtomicFailure(t *testing.T) {
	for _, op := range []string{"open", "write", "short-write", "sync", "close", "directory-open", "directory-sync", "directory-close", "late-directory-sync", "late-directory-close"} {
		t.Run(op, func(t *testing.T) {
			c, tx, _, _ := t3TxPrepare(t, true)
			if e := tx.record("previous"); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(tx.dir, "transaction.json")
			old, _ := os.ReadFile(path)
			dirs := 0
			c.t3JournalFiles = &t3JournalFiles{
				create: func(dir, pattern string) (t3JournalFile, error) {
					if op == "open" {
						return nil, errors.New("injected open failure")
					}
					f, e := os.CreateTemp(dir, pattern)
					return &t3RepairJournalFile{f, op}, e
				},
				directory: func(dir string) (t3JournalFile, error) {
					dirs++
					if op == "directory-open" {
						return nil, errors.New("injected directory open failure")
					}
					f, e := os.Open(dir)
					injected := op
					if strings.HasPrefix(op, "late-") {
						injected = ""
						if dirs == 2 {
							injected = strings.TrimPrefix(op, "late-")
						}
					}
					return &t3RepairJournalFile{f, injected}, e
				},
			}
			if e := tx.record("next"); e == nil {
				t.Fatal("ignored storage failure")
			}
			got, e := os.ReadFile(path)
			if e != nil || string(got) != string(old) {
				t.Fatalf("previous usable journal destroyed: %s %v", got, e)
			}
			var decoded map[string]any
			if e := json.Unmarshal(got, &decoded); e != nil || decoded["phase"] != "previous" {
				t.Fatal("previous journal unusable")
			}
		})
	}
}

func TestT3RepairJournalHandlerFailures(t *testing.T) {
	for _, phase := range []string{"staging", "before-package-move", "package-moved", "package-activated", "link-activated", "state-moved", "state-activated", "activated", "deferred-before-activation", "installed-served-split", "restoration-required", "files-restored", "restored", "committed"} {
		t.Run(phase, func(t *testing.T) {
			stageIdle, activationIdle := 0, 0
			if phase == "deferred-before-activation" {
				stageIdle = 2
			}
			if phase == "installed-served-split" {
				activationIdle = 2
			}
			restore := phase == "restoration-required" || phase == "files-restored" || phase == "restored"
			c, spec, _, prefix := t3ArtifactHost(t, false, restore, stageIdle, activationIdle)
			old, _ := release.T3TreeDigest(filepath.Join(prefix, "lib/node_modules/t3"))
			fired := false
			c.t3JournalFiles = &t3JournalFiles{create: func(dir, pattern string) (t3JournalFile, error) {
				f, e := os.CreateTemp(dir, pattern)
				op := ""
				if pattern == ".journal-"+phase+"-*" {
					fired = true
					op = "sync"
				}
				return &t3RepairJournalFile{f, op}, e
			}}
			before, _ := t3Observe(c, spec)
			actions := []string{}
			got, e := t3Apply(c, spec, before, &actions)
			var rolled *RolledBack
			if !fired || e == nil || errors.As(e, &rolled) {
				t.Fatalf("persistence failure reported successful %s: %s %v fired=%t", phase, got, e, fired)
			}
			if !strings.Contains(e.Error(), "journal") {
				t.Fatalf("journal error not reported: %v", e)
			}
			if restore || phase == "activated" || strings.Contains(phase, "package-") || phase == "link-activated" || phase == "state-moved" || phase == "state-activated" {
				digest, _ := release.T3TreeDigest(filepath.Join(prefix, "lib/node_modules/t3"))
				if digest != old {
					t.Fatalf("files not restored at %s", phase)
				}
			}
		})
	}
}
