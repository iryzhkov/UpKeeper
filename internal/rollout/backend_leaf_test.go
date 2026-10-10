package rollout

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func leafHome(t *testing.T, fault string) string {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/bash
case "${0##*/}" in
 uname) echo x86_64 ;;
 t3-steward) echo 't3-steward 0.11.0-rc.122' ;;
 readlink) case "$*" in *-f*) echo "$HOME/.local/bin/t3-steward" ;; *) echo /bin/t3-steward ;; esac ;;
 sha256sum) echo 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  binary' ;;
 systemctl)
  case "$*" in
   *UnitFileState*--value*) echo masked ;;
   *upkeeper-self.timer*) printf 'ActiveState=inactive\nUnitFileState=enabled\n' ;;
   *MainPID*) echo 0 ;;
   *ExecMainStartTimestamp*) echo before ;;
   *NRestarts*) echo 0 ;;
   *t3-steward-worker.service*ActiveState*) echo "${LEAF_WORKER_STATE:-inactive}" ;;
   *ActiveState*) echo inactive ;;
  esac ;;
esac
if [[ "${0##*/}" == "$LEAF_FAULT" ]]; then
 if [[ "$LEAF_FAULT" != systemctl || "$*" == *show* ]]; then exit 42; fi
fi
`
	for _, name := range []string{"uname", "t3-steward", "readlink", "sha256sum", "systemctl", "sleep"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("LEAF_FAULT", fault)
	return home
}

func TestLeafSnapshotRejectsValidOutputThenFailure(t *testing.T) {
	for _, producer := range []string{"uname", "t3-steward", "readlink", "sha256sum", "systemctl"} {
		t.Run(producer, func(t *testing.T) {
			leafHome(t, producer)
			b := NewProductionBackend()
			_, err := b.on(context.Background(), Config{LocalHost: "isolated"}, "isolated", snapshotScript)
			if err == nil {
				t.Fatal("accepted a failed snapshot producer")
			}
		})
	}
}
func TestLeafTimerAndLaptopRejectValidOutputThenFailure(t *testing.T) {
	leafHome(t, "systemctl")
	b := NewProductionBackend()
	c := Config{LocalHost: "isolated", LaptopHost: "isolated", TimerHosts: []string{"isolated"}}
	if _, err := b.laptop(context.Background(), c); err == nil {
		t.Fatal("accepted failed mask producer")
	}
	c.LaptopHost = ""
	if err := b.timerCheck(context.Background(), c, true); err == nil {
		t.Fatal("accepted failed timer producer")
	}
}
func TestLeafTimerRejectsUnknownStates(t *testing.T) {
	for _, key := range []string{"ActiveState", "SelfServiceActive", "ControllerTimer", "UnitFileState"} {
		t.Run(key, func(t *testing.T) {
			b, runner, c, _ := backendFixture(t)
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				p, err := runner.Run(ctx, dir, stdin, name, args...)
				if stdin == timerScript {
					kv := keyValues(p)
					kv[key] = "unknown"
					p = encodeKV(kv)
				}
				return p, err
			})
			if err := b.timerCheck(context.Background(), c, false); err == nil {
				t.Fatal("accepted unknown " + key)
			}
		})
	}
}

func TestLeafDatabaseHolderRealInodeAndNonDumpable(t *testing.T) {
	for _, kind := range []string{"hardlink", "non-dumpable"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			db := filepath.Join(dir, "state.db")
			if err := os.WriteFile(db, []byte("database"), 0600); err != nil {
				t.Fatal(err)
			}
			held := db
			if kind == "hardlink" {
				held = filepath.Join(dir, "alias")
				if err := os.Link(db, held); err != nil {
					t.Fatal(err)
				}
			}
			code := `import ctypes,sys
f=open(sys.argv[1])
if sys.argv[2]=="non-dumpable":
 if ctypes.CDLL(None).prctl(4,0,0,0,0)!=0: sys.exit(2)
print("ready",flush=True)
sys.stdin.read()
`
			child := exec.Command("python3", "-c", code, held, kind)
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := child.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stdin.Close(); _ = child.Wait() })
			ready := make([]byte, 6)
			if _, err = io.ReadFull(stdout, ready); err != nil {
				t.Fatal(err)
			}
			b := NewProductionBackend()
			_, err = b.on(context.Background(), Config{LocalHost: "isolated"}, "isolated", "python3 -c "+quote(holderFixtureProgram())+" "+quote(db)+" "+fmt.Sprint(child.Process.Pid))
			if err == nil {
				t.Fatal("accepted live same-uid holder")
			}
			if kind == "non-dumpable" && os.Geteuid() != 0 && !strings.Contains(err.Error(), "cannot inspect") {
				t.Fatalf("expected inaccessible inventory refusal: %v", err)
			}
			if kind == "hardlink" && !strings.Contains(err.Error(), "held by") {
				t.Fatalf("expected inode holder refusal: %v", err)
			}
		})
	}
}

func TestLeafDatabaseHolderVanishedFDRefusesLivePID(t *testing.T) {
	// Exercise the actual Python adapter with a deterministic proc race. A vanished
	// FD on a still-live PID must not turn a partial inventory into an empty scan.
	start := strings.Index(databaseHolderScript, "import os, sys")
	end := strings.LastIndex(databaseHolderScript, "\nPY")
	source := databaseHolderScript[start:end]
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if err := os.WriteFile(db, []byte("db"), 0600); err != nil {
		t.Fatal(err)
	}
	prelude := `import os,sys,io,builtins
realstat,realopen=os.stat,builtins.open
def listing(path):
 if path=="/proc": return ["99999999"]
 if path=="/proc/99999999/fd": return ["7","8"]
 raise RuntimeError(path)
def stat(path,*args,**kwargs):
 if path=="/proc/99999999/fd/7": raise FileNotFoundError(path)
 if path=="/proc/99999999": return realstat(sys.argv[1])
 return realstat(path,*args,**kwargs)
def opening(path,*args,**kwargs):
 if path=="/proc/99999999/status": return io.StringIO("Uid: %d %d %d %d\n"%((os.getuid(),)*4))
 return realopen(path,*args,**kwargs)
os.listdir,os.stat,builtins.open=listing,stat,opening
`
	out, err := exec.Command("python3", "-c", prelude+source, db).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "descriptor disappeared") {
		t.Fatalf("race must refuse: %v %s", err, out)
	}
}

func TestLeafStoppedBackupRejectsActiveWorkerBeforeCheckpoint(t *testing.T) {
	home := leafHome(t, "")
	t.Setenv("LEAF_WORKER_STATE", "active")
	b := NewProductionBackend()
	_, err := b.on(context.Background(), Config{LocalHost: "isolated"}, "isolated", legacyBackupScript, "backup", "scratch")
	if err == nil || !strings.Contains(err.Error(), "coordinator or worker did not stop") {
		t.Fatalf("expected still-active worker refusal: %v", err)
	}
	if _, err = os.Stat(filepath.Join(home, "backup.create.json")); !os.IsNotExist(err) {
		t.Fatal("backup effect before both units stopped")
	}
}

func TestLeafStoppedBackupQueriesBothUnitsBeforeCheckpoint(t *testing.T) {
	home := leafHome(t, "systemctl")
	b := NewProductionBackend()
	_, err := b.on(context.Background(), Config{LocalHost: "isolated"}, "isolated", legacyBackupScript, "backup", "scratch")
	if err == nil {
		t.Fatal("accepted failed stopped-unit producer")
	}
	if _, err = os.Stat(filepath.Join(home, "backup.create.json")); !os.IsNotExist(err) {
		t.Fatal("backup effect before stopped-state evidence")
	}
}
