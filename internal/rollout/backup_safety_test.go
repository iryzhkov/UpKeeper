package rollout

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the production Bash adapter through OSRunner. Only the process inventory
// is stubbed here so every late shell leaf is reachable; real UID/inode/permission
// tests remain in backend_leaf_test.go.
func stoppedBackupHome(t *testing.T, fault string) string {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, ".local/bin")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".config/t3-steward/config.yaml"), "state_path: production\nbacklog_v2:\n  mode: coordinator\n  storage:\n    artifacts: production\n    bundles: production\n    workspaces: production\nbacklog:\n  dir: production\narchive:\n  enabled: true\n  destination: production\n")
	db := filepath.Join(home, ".local/state/t3-steward/state.db")
	if err := os.MkdirAll(filepath.Dir(db), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/python3", "-c", "import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute('create table sample(x)'); c.commit(); c.close()", db).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	common := `#!/bin/bash
set -u
name="${0##*/}"
n=0
if [ -f "$HOME/count-$name" ]; then read -r n < "$HOME/count-$name"; fi
n=$((n+1)); printf '%s\n' "$n" > "$HOME/count-$name"
case "$name" in
 systemctl) case "$*" in *show*) echo inactive ;; esac; rc=0 ;;
 python3)
  code=$(/usr/bin/cat)
  case "$code" in
   *"uid, holders = os.getuid()"*) exit 0 ;;
   *"wal_checkpoint(TRUNCATE)"*) stage=checkpoint ;;
   *"seen, stack, out"*) stage=config ;;
   *"PRAGMA integrity_check"*) stage=integrity ;;
   *) exit 89 ;;
  esac
  /usr/bin/python3 -c "$code" "${@:2}"; rc=$?
  if [ "$BACKUP_FAULT" = "python-$stage" ]; then exit 42; fi ;;
 t3-steward)
  case "$3" in
   create) /usr/bin/mkdir "$4"; echo '{"created":true}'; rc=0 ;;
   verify) echo '{"verified":true}'; rc=0 ;;
   restore)
    scratch="${6%/config.yaml}"
    /usr/bin/mkdir "$scratch/artifacts"
    /usr/bin/python3 -c 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute("create table sample(x)"); c.commit(); c.close()' "$scratch/state.db"
    echo '{"restored":true}'; rc=0 ;;
   *) exit 88 ;;
  esac
  if [ "$BACKUP_FAULT" = "backup-$3" ]; then exit 42; fi ;;
 *) "/usr/bin/$name" "$@"; rc=$? ;;
esac
if [ "$BACKUP_FAULT" = "$name" ] || [ "$BACKUP_FAULT" = "$name-$n" ]; then exit 42; fi
exit "$rc"
`
	for _, name := range []string{"systemctl", "python3", "t3-steward", "dirname", "grep", "stat", "chmod", "mkdir", "sha256sum", "sed", "head", "rm"} {
		write(filepath.Join(bin, name), common)
	}
	if fault == "compgen" {
		write(filepath.Join(home, "bash-env"), "compgen() { builtin compgen \"$@\"; return 42; }\n")
		t.Setenv("BASH_ENV", filepath.Join(home, "bash-env"))
	}
	if fault == "unset" {
		write(filepath.Join(home, "bash-env"), "unset() { builtin unset \"$@\"; return 42; }\n")
		t.Setenv("BASH_ENV", filepath.Join(home, "bash-env"))
		t.Setenv("T3_STEWARD_TEST_OVERRIDE", "must-be-unset")
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("BACKUP_FAULT", fault)
	return home
}

func TestStoppedBackupAllSafetyLeavesChecked(t *testing.T) {
	faults := []string{"none", "dirname", "grep-1", "grep-2", "stat-1", "stat-2", "compgen", "unset", "python-checkpoint", "python-config", "python-integrity", "backup-create", "backup-verify", "backup-restore", "chmod-1", "chmod-2", "chmod-3", "mkdir-1", "mkdir-2", "sha256sum", "sed", "head", "systemctl-1", "systemctl-2", "systemctl-3", "systemctl-4", "rm"}
	for _, fault := range faults {
		t.Run(fault, func(t *testing.T) {
			home := stoppedBackupHome(t, fault)
			b := NewProductionBackend()
			out, err := b.on(context.Background(), Config{LocalHost: "isolated"}, "isolated", legacyBackupScript, "backups/snapshot", "backups/scratch")
			if fault == "none" {
				if err != nil {
					t.Fatalf("control: %v %s", err, out)
				}
				kv := keyValues(out)
				if kv["restore_integrity"] != "ok" || kv["restore_fk_violations"] != "0" || kv["snapshot"] == "" || kv["scratch"] == "" {
					t.Fatalf("missing accepted control evidence: %s", out)
				}
			} else if err == nil {
				t.Fatalf("failed leaf accepted: %s", out)
			}
			if fault == "dirname" {
				if _, err := os.Stat(filepath.Join(home, "backups")); !os.IsNotExist(err) {
					t.Fatal("consumed failed dirname before mkdir")
				}
			}
			if fault == "compgen" || fault == "unset" {
				if _, err := os.Stat(filepath.Join(home, "backups/scratch/restore.json")); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(home, "backups/scratch/state.db")); !os.IsNotExist(err) {
					t.Fatal("restore ran after failed environment enumeration/removal")
				}
			}
		})
	}
}

func TestBackupFreshnessUsesPreEffectTime(t *testing.T) {
	for _, elapsed := range []time.Duration{time.Second, 2 * time.Hour, -time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			b, f, c, s := backendFixture(t)
			fencedFake(f)
			start := b.Now()
			now := start
			b.Now = func() time.Time { return now }
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				if stdin == legacyBackupScript {
					now = start.Add(elapsed)
				}
				return f.Run(ctx, dir, stdin, name, args...)
			})
			err := b.backup(context.Background(), c, s)
			stale := elapsed < 0 || elapsed > time.Duration(c.BackupMaxAgeSeconds)*time.Second
			if stale {
				if err == nil || !strings.Contains(err.Error(), "future-dated or stale") {
					t.Fatalf("stale/future backup accepted: %v", err)
				}
				if !s.BackupCreated.IsZero() || s.BackupSnapshot != "" || s.Extra["backup"] != nil {
					t.Fatal("failed backup published accepted receipt")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !s.BackupCreated.Equal(start) {
					t.Fatalf("creation = %v, want conservative pre-effect %v", s.BackupCreated, start)
				}
			}
		})
	}
}
