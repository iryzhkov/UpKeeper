package rollout

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Seed legitimate predecessor history through the production intent writer so
// these retained negative tests reach their reviewed fault rather than a missing
// prerequisite introduced by the inventory repair.
func reviewAcknowledged(t *testing.T, c Config, action, host string, now time.Time) {
	t.Helper()
	ack, err := beginAttempt(c, action, host, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = ack(); err != nil {
		t.Fatal(err)
	}
}
func reviewCompletedHost(t *testing.T, c Config, host string, now time.Time) {
	t.Helper()
	reviewAcknowledged(t, c, "run:"+host, host, now)
	reviewAcknowledged(t, c, "phase:observe:"+host, "", now)
	reviewAcknowledged(t, c, "phase:accept:"+host, "", now)
}
func TestReviewPreHostCompletionFailure(t *testing.T) {
	for _, phase := range []string{"fence", "prepare", "publish", "release-ci"} {
		t.Run(phase, func(t *testing.T) {
			c := rc122Fixture(t)
			r, _ := fakeRuntime()
			f := protocolBackend()
			injected := false
			r.Save = func(p string, s *State) error {
				if err := saveState(p, s); err != nil {
					return err
				}
				if s.Completed[phase] && s.InFlight == "" && !injected {
					injected = true
					return errors.New("rename succeeded; dirsync failed")
				}
				return nil
			}
			err := RunHostWithRuntime(context.Background(), c, f, c.Canary, true, io.Discard, r)
			if err == nil || !injected {
				t.Fatalf("fault not reached: %v", err)
			}
			f.actions = nil
			r.Save = saveState
			err = RunHostWithRuntime(context.Background(), c, f, c.Canary, true, io.Discard, r)
			t.Logf("restart err=%v host_dispatched=%v", err, contains(f.actions, "host:"+c.Canary))
			if err == nil || contains(f.actions, "host:"+c.Canary) {
				t.Fatal("UNSAFE: advanced after failed completion persistence")
			}
		})
	}
}
func TestReviewProductionMissingPerHostDigestFence(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	queriedFence := false
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == c.ActivityCommand[0] {
			no := false
			return jsonBytes(ActivityReceipt{Host: args[0], CoordinatorEpoch: args[1], CollectionDeferred: &no, RunningWithoutActivity: &no}), nil
		}
		if name == "git" && len(args) > 0 && args[0] == "status" {
			queriedFence = true
			return []byte(" M desired/release.json\n"), nil
		}
		if name == "upkeeper" && len(args) > 0 && args[0] == "fleet-plan" {
			return []byte(`{"release_sha256":"unreviewed"}`), nil
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, io.Discard, r)
	t.Logf("err=%v pulls=%d", err, productionPulls(f))
	if !queriedFence {
		t.Fatal("per-host checkout fence not reached")
	}
	if err == nil {
		t.Fatal("UNSAFE: no per-host manifest digest/dirty check")
	}
}
func TestReviewD5InterruptedRecovery(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	s.InFlight = "dropin:agent-b"
	s.Extra["dropin:agent-b"] = jsonBytes(dropinReceipt{Host: "agent-b", SHA256: strings.Repeat("a", 64), IntentAt: r.Now()})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := beginAttempt(c, "enroll:agent-b", "agent-b", r.Now()); err != nil {
		t.Fatal(err)
	}
	err = RecoverHost(context.Background(), c, b, "agent-b", true)
	t.Logf("recover=%v commands=%d", err, len(f.calls))
	if err != nil && strings.Contains(err.Error(), "differs from saved intent") {
		t.Fatal("UNRECOVERABLE: supported command rejects interrupted D5")
	}
}
func TestReviewHostConfigNotRevalidated(t *testing.T) {
	b, f, c, _ := productionHostFixture(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	s.BackupCreated = b.Now()
	s.BackupSnapshot = "snapshot"
	s.BackupScratch = "scratch"
	s.Extra["backup"] = jsonBytes(map[string]string{"restore_integrity": "ok", "restore_fk_violations": "0", "snapshot": s.BackupSnapshot, "scratch": s.BackupScratch})
	s.Completed["config-patch"] = true
	s.Extra["config-intent"] = jsonBytes(configReceipt{Before: c.CoordinatorConfig.BeforeSHA256, After: c.CoordinatorConfig.AfterSHA256, IntentAt: b.Now(), Done: true})
	reviewAcknowledged(t, c, "phase:config-patch", "", b.Now())
	seen := false
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if stdin == configTransactionScript {
			seen = true
			return nil, errors.New("live configuration changed")
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	_ = b.host(context.Background(), c, c.CoordinatorHost, s)
	t.Logf("configuration check=%v pulls=%d", seen, productionPulls(f))
	if !seen {
		t.Fatal("fresh completed-D2 verification not reached")
	}
	if productionPulls(f) > 0 {
		t.Fatal("UNSAFE: coordinator pull without checking installed after digest")
	}
}

var _ = os.Getuid

func TestReviewBackupShellLeafFailures(t *testing.T) {
	for _, fault := range []string{"none", "dirname", "grep", "stat", "compgen"} {
		t.Run(fault, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, ".local/bin")
			if err := os.MkdirAll(bin, 0700); err != nil {
				t.Fatal(err)
			}
			write := func(p, s string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(s), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(bin, "systemctl"), "#!/bin/bash\ncase \"$*\" in *show*) echo inactive;; esac\n")
			write(filepath.Join(bin, "python3"), `#!/bin/bash
code=$(/usr/bin/cat)
if [[ "$code" == *"uid, holders = os.getuid()"* ]]; then exit 0; fi
exec /usr/bin/python3 -c "$code" "${@:2}"
`)
			write(filepath.Join(bin, "t3-steward"), `#!/bin/bash
set -e
case "$3" in
 create) mkdir "$4"; echo '{"created":true}';;
 verify) echo '{"verified":true}';;
 restore)
 scratch="${6%/config.yaml}"
 mkdir "$scratch/artifacts"
 /usr/bin/python3 -c 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute("create table sample(x)"); c.commit(); c.close()' "$scratch/state.db"
 echo '{"restored":true}';;
 *) exit 88;;
esac
`)
			write(filepath.Join(home, ".config/t3-steward/config.yaml"), "state_path: production\nbacklog_v2:\n  mode: coordinator\n  storage:\n    artifacts: production\n    bundles: production\n    workspaces: production\nbacklog:\n  dir: production\narchive:\n  enabled: true\n  destination: production\n")
			db := filepath.Join(home, ".local/state/t3-steward/state.db")
			os.MkdirAll(filepath.Dir(db), 0700)
			out, err := exec.Command("/usr/bin/python3", "-c", "import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute('create table sample(x)'); c.commit(); c.close()", db).CombinedOutput()
			if err != nil {
				t.Fatal(err, string(out))
			}
			if fault == "dirname" {
				write(filepath.Join(bin, "dirname"), "#!/bin/bash\n/usr/bin/dirname \"$@\"\nexit 42\n")
			}
			if fault == "grep" {
				write(filepath.Join(bin, "grep"), "#!/bin/bash\nexit 42\n")
			}
			if fault == "stat" {
				write(filepath.Join(bin, "stat"), `#!/bin/bash
/usr/bin/stat "$@"
if [ -e "$HOME/stat-once" ]; then exit 42; fi
touch "$HOME/stat-once"
`)
			}
			if fault == "compgen" {
				write(filepath.Join(home, "bash-env"), "compgen() { builtin compgen \"$@\"; return 42; }\n")
				t.Setenv("BASH_ENV", filepath.Join(home, "bash-env"))
			}
			t.Setenv("HOME", home)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			b := NewProductionBackend()
			out, err = b.on(context.Background(), Config{LocalHost: "isolated"}, "isolated", legacyBackupScript, "backups/snapshot", "backups/scratch")
			t.Logf("fault=%s err=%v output=%s", fault, err, out)
			if fault == "none" && err != nil {
				t.Fatal("control failed")
			}
			if fault != "none" && err == nil {
				t.Fatal("UNSAFE: actual backup shell accepted failed leaf")
			}
		})
	}
}

func TestReviewMissingAttemptInventoryAccepted(t *testing.T) {
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	if err := RunHostWithRuntime(context.Background(), c, f, c.Canary, true, io.Discard, r); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(c.StatePath + ".attempts"); err != nil {
		t.Fatal(err)
	}
	f.actions = nil
	err := RunHostWithRuntime(context.Background(), c, f, "normandy", true, io.Discard, r)
	t.Logf("missing intent inventory: err=%v coordinator dispatched=%v", err, contains(f.actions, "host:normandy"))
	if err == nil {
		t.Fatal("UNSAFE: lost inventory treated as empty despite completed host journal")
	}
}
func TestReviewD5CatalogBeforeDropin(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range rc122Order {
		s.Completed["host:"+h] = true
		reviewCompletedHost(t, c, h, r.Now())
		s.Extra["observation:"+h] = jsonBytes(ObservationReceipt{Host: h, ConfigDigest: c.Digest(), ReleaseCommit: s.ReleaseCommit, Started: r.Now().Add(-time.Hour), Finished: r.Now(), Samples: 361, Identity: jsonBytes(map[string]string{"epoch": "4"})})
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	enrollment := false
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if strings.Contains(stdin, "worker enroll") {
			enrollment = true
		}
		if name == c.HaikuCheckCommand[0] {
			return []byte(`{"worker_id":"agent-b","coordinator_epoch":"1","model":"claudeAgent/claude-haiku-5-5"}`), nil
		}
		if name == "t3-steward" && len(args) > 0 && args[0] == "worker" {
			var ws []worker
			w := worker{}
			w.Requirement.WorkerID = "agent-b"
			w.Health = "catalog-drift"
			n := 0
			w.Snapshot.Inventory.Telemetry.RunningAttempts = &n
			ws = append(ws, w)
			for _, h := range c.DrainedHosts {
				w := worker{}
				w.Requirement.WorkerID = h
				w.State = "draining"
				ws = append(ws, w)
			}
			return jsonBytes(map[string]any{"Workers": ws}), nil
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	err = FinishWorker(context.Background(), c, b, "agent-b", true)
	t.Logf("D5 err=%v enrollment=%v", err, enrollment)
	if err != nil && !enrollment {
		t.Fatal("BROKEN ORDER: catalog drift blocked before named enrollment")
	}
}

func TestReviewProductionPreHostCompletionFailure(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	delete(s.Completed, "release-ci")
	// This fixture models a phase that has not yet run, so remove its seeded acknowledgement.
	if err := os.Remove(attemptPath(c, "phase:release-ci")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == c.ActivityCommand[0] {
			no := false
			return jsonBytes(ActivityReceipt{Host: args[0], CoordinatorEpoch: args[1], CollectionDeferred: &no, RunningWithoutActivity: &no}), nil
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	injected := false
	r.Save = func(p string, s *State) error {
		if err := saveState(p, s); err != nil {
			return err
		}
		if s.Completed["release-ci"] && s.InFlight == "" && !injected {
			injected = true
			return errors.New("dirsync after rename failed")
		}
		return nil
	}
	if err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, io.Discard, r); err == nil || !injected {
		t.Fatal("fault missed", err)
	}
	r.Save = saveState
	err = RunHostWithRuntime(context.Background(), c, b, c.Canary, true, io.Discard, r)
	t.Logf("ProductionBackend restart=%v pulls=%d", err, productionPulls(f))
	if err == nil || productionPulls(f) > 0 {
		t.Fatal("UNSAFE: actual host path continued after prehost persistence failure")
	}
}
func TestReviewFinalVerifyOmitsConfigurationStatus(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range rc122Order {
		s.Extra["host-pre:"+h] = jsonBytes(f.snapshot(h))
		f.upgraded[h] = true
		s.Completed["host:"+h] = true
		reviewCompletedHost(t, c, h, r.Now())
		s.Extra["observation:"+h] = jsonBytes(ObservationReceipt{Host: h, ConfigDigest: c.Digest(), ReleaseCommit: s.ReleaseCommit, Started: r.Now().Add(-time.Hour), Finished: r.Now(), Samples: 361, Identity: jsonBytes(map[string]string{"epoch": "2"})})
	}
	for _, h := range c.ActiveWorkers {
		s.Completed["enroll-worker:"+h] = true
		reviewAcknowledged(t, c, "enroll:"+h, h, r.Now())
		reviewAcknowledged(t, c, "phase:enroll-worker:"+h, "", r.Now())
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	queried := false
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == "upkeeper" && len(args) > 0 && (args[0] == "status" || args[0] == "pull") {
			queried = true
			return []byte(`{"hosts":[{"host":"agent-a","result":"failed","components":[{"name":"steward-fleet-configuration","state":"failed","error_class":"client-configuration"}]}]}`), errors.New("exit 20: unresolved configuration")
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	err = VerifyRollout(context.Background(), c, b, true)
	t.Logf("verify=%v full-status-queried=%v", err, queried)
	if !queried || err == nil {
		t.Fatal("UNSAFE: final complete without fresh full configuration status")
	}
}
func TestReviewBackupAgeStartsAtCompletion(t *testing.T) {
	b, f, c, s := backendFixture(t)
	fencedFake(f)
	now := b.Now()
	start := now
	b.Now = func() time.Time { return now }
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if stdin == legacyBackupScript {
			now = now.Add(2 * time.Hour)
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	err := b.backup(context.Background(), c, s)
	t.Logf("backup start=%v completion=%v declared-created=%v age=%v error=%v", start, now, s.BackupCreated, now.Sub(s.BackupCreated), err)
	if err == nil || !strings.Contains(err.Error(), "future-dated or stale") {
		t.Fatal("UNSAFE: over-age drill was accepted", err)
	}
	if !s.BackupCreated.IsZero() || s.Extra["backup"] != nil {
		t.Fatal("UNSAFE: stale backup published accepted receipt")
	}
}
