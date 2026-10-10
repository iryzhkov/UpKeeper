package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type dropinCommandRunner struct {
	shell                *configShellRunner
	advertisementFailure bool
}

func (r *dropinCommandRunner) Run(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
	if name == "haiku-adapter" {
		host, epoch := args[len(args)-2], args[len(args)-1]
		raw, _ := json.Marshal(map[string]string{"worker_id": host, "coordinator_epoch": epoch, "model": "claudeAgent/claude-haiku-5-5"})
		if r.advertisementFailure {
			return raw, errors.New("valid adapter output then 42")
		}
		return raw, nil
	}
	if name == "t3-steward" {
		if len(args) > 1 && args[0] == "coordinator" {
			return []byte("{\"Health\":\"healthy\",\"Release\":\"rc122\",\"Epoch\":4}"), nil
		}
		return []byte("{\"Workers\":[{\"Requirement\":{\"workerId\":\"agent-b\"},\"Health\":\"ready\",\"Snapshot\":{\"CoordinatorEpoch\":4,\"Inventory\":{\"Runtime\":{\"Release\":\"rc122\"},\"Telemetry\":{\"running_attempts\":0}}}}]}"), nil
	}
	return r.shell.Run(ctx, dir, stdin, name, args...)
}

func TestDropinRepairGateFailureRestoresAndFencesNextHost(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config/systemd/user/t3-steward-worker.service.d/tmpdir-resolved.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("[Service]\nEnvironment=TMPDIR=/old\n")
	if err := os.WriteFile(path, original, 0640); err != nil {
		t.Fatal(err)
	}
	bindir := filepath.Join(home, "bin")
	if err := os.Mkdir(bindir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bindir, "systemctl"), []byte("#!/bin/bash\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+":"+os.Getenv("PATH"))
	runner := &dropinCommandRunner{shell: &configShellRunner{home: home}}
	b := &ProductionBackend{Runner: runner, Now: time.Now}
	c := Config{LocalHost: "agent-b", Version: "rc122", DropinTestCommand: []string{"bash", "-c", "echo valid; exit 42"}, HaikuCheckCommand: []string{"haiku-adapter"}}
	s := &State{Extra: map[string]json.RawMessage{}, Save: func() error { return nil }}
	if err := b.dropinRepair(context.Background(), c, "agent-b", s); err == nil {
		t.Fatal("test failure accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatal("drop-in not restored")
	}
	var r dropinReceipt
	if err = evidence(s, "dropin:agent-b", &r); err != nil {
		t.Fatal(err)
	}
	if !r.Restored || r.Done {
		t.Fatal("recovery became success")
	}
	calls := runner.shell.calls
	if err = b.dropinRepair(context.Background(), c, "agent-a", s); err == nil {
		t.Fatal("next host after uncertainty")
	}
	if runner.shell.calls != calls {
		t.Fatal("next-host effect")
	}
	if err = b.dropinRepair(context.Background(), c, "agent-b", s); err == nil {
		t.Fatal("forbidden resume")
	}
}
func TestDropinRepairReviewedAbsentDoesNotInventBackup(t *testing.T) {
	home := t.TempDir()
	runner := &dropinCommandRunner{shell: &configShellRunner{home: home}}
	b := &ProductionBackend{Runner: runner, Now: time.Now}
	c := Config{LocalHost: "agent-b", Version: "rc122", DropinAbsentHosts: []string{"agent-b"}, DropinTestCommand: []string{"bash", "-c", "exit 0"}, HaikuCheckCommand: []string{"haiku-adapter"}}
	s := &State{Extra: map[string]json.RawMessage{}, Save: func() error { return nil }}
	if err := b.dropinRepair(context.Background(), c, "agent-b", s); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(home, ".local/state/t3-steward-rollout-backups/rc122-tmpdir-dropin.conf")
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatal("invented original")
	}
	var receipt dropinReceipt
	if err := evidence(s, "dropin:agent-b", &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Absent || !receipt.Done || receipt.SHA256 != "" {
		t.Fatal("absent receipt incorrect")
	}
}

func TestHaikuAdvertisementProducerStatus(t *testing.T) {
	b := &ProductionBackend{Runner: &dropinCommandRunner{advertisementFailure: true}}
	if err := b.haikuAdvertisement(context.Background(), Config{HaikuCheckCommand: []string{"haiku-adapter"}}, "agent-b", "4"); err == nil {
		t.Fatal("producer output bypassed exit42")
	}
}

type configShellRunner struct {
	home      string
	calls     int
	ambiguous bool
}

func (r *configShellRunner) Run(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
	r.calls++
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "HOME="+r.home)
	cmd.Stdin = strings.NewReader(stdin)
	raw, err := cmd.CombinedOutput()
	if err == nil && r.ambiguous {
		return raw, errors.New("transport took effect then returned 42")
	}
	return raw, err
}
func configFixture(t *testing.T) (*ProductionBackend, Config, *State, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, ".config", "t3-steward", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	before, after := "before: original\n", "after: reviewed\n"
	if err := os.WriteFile(path, []byte(before), 0640); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := &configShellRunner{home: home}
	b := &ProductionBackend{Runner: r, Now: func() time.Time { return now }}
	c := Config{LocalHost: "normandy", CoordinatorHost: "normandy", BackupMaxAgeSeconds: 3600, CoordinatorConfig: &ConfigChange{BeforeSHA256: digest([]byte(before)), AfterSHA256: digest([]byte(after)), Content: after, ValidateCommand: []string{"python3", "-c", "import pathlib,sys,json,hashlib; data=pathlib.Path(sys.argv[1]).read_bytes(); assert data == b'after: reviewed\\n'; print(json.dumps(dict(schema_version=1,kind='config-validation',valid=True,file_sha256=hashlib.sha256(data).hexdigest())))", "{config}"}}}
	s := &State{Extra: map[string]json.RawMessage{}, BackupCreated: now, BackupSnapshot: "verified-snapshot", BackupScratch: "verified-scratch", Save: func() error { return nil }}
	if err := record(s, "backup", map[string]string{"restore_integrity": "ok", "restore_fk_violations": "0", "snapshot": s.BackupSnapshot, "scratch": s.BackupScratch}); err != nil {
		t.Fatal(err)
	}
	return b, c, s, path
}
func TestConfigTransactionRealNonRootPermissionsRefuse(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("requires non-root permission semantics")
	}
	for _, kind := range []string{"unreadable", "unwritable-directory"} {
		t.Run(kind, func(t *testing.T) {
			b, c, s, path := configFixture(t)
			if kind == "unreadable" {
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.configPatch(context.Background(), c, s); err == nil {
				t.Fatal("permission fault accepted")
			}
			if kind == "unreadable" {
				if err := os.Chmod(path, 0640); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if digest(got) != c.CoordinatorConfig.BeforeSHA256 {
				t.Fatal("permission refusal mutated config")
			}
			if _, err = os.Stat(path + ".bak-rc122-pre"); !os.IsNotExist(err) {
				t.Fatal("permission refusal mutated backup")
			}
		})
	}
}
func TestConfigTransactionActualPersistenceFaults(t *testing.T) {
	for _, kind := range []string{"file-fsync", "directory-fsync", "rename-after-effect"} {
		t.Run(kind, func(t *testing.T) {
			b, c, s, path := configFixture(t)
			injection := t.TempDir()
			code := "import os,stat\n_real_sync=os.fsync\n_real_replace=os.replace\n"
			if kind == "rename-after-effect" {
				code += "def fault_replace(src,dst):\n _real_replace(src,dst)\n raise OSError('rename effected then returned 42')\nos.replace=fault_replace\n"
			} else {
				check := "not stat.S_ISDIR(os.fstat(fd).st_mode)"
				if kind == "directory-fsync" {
					check = "stat.S_ISDIR(os.fstat(fd).st_mode)"
				}
				code += "def fault_sync(fd):\n if " + check + ": raise OSError('injected persistence failure')\n return _real_sync(fd)\nos.fsync=fault_sync\n"
			}
			if err := os.WriteFile(filepath.Join(injection, "sitecustomize.py"), []byte(code), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PYTHONPATH", injection)
			t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
			if err := b.configPatch(context.Background(), c, s); err == nil {
				t.Fatal("persistence fault accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := c.CoordinatorConfig.BeforeSHA256
			if kind == "rename-after-effect" {
				want = c.CoordinatorConfig.AfterSHA256
			}
			if digest(got) != want {
				t.Fatal("unexpected persistence effect")
			}
			calls := b.Runner.(*configShellRunner).calls
			if err := b.configPatch(context.Background(), c, s); err == nil {
				t.Fatal("unfinished persistence resumed")
			}
			if b.Runner.(*configShellRunner).calls != calls {
				t.Fatal("second ambiguous dispatch")
			}
		})
	}
}

func TestConfigTransactionBeforeAndCorruptBackupEvidence(t *testing.T) {
	b, c, s, path := configFixture(t)
	if err := b.configBefore(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".bak-rc122-pre"); !os.IsNotExist(err) {
		t.Fatal("before check mutated backup")
	}
	s.Extra["backup"] = json.RawMessage("{")
	if err := b.configPatch(context.Background(), c, s); err == nil {
		t.Fatal("corrupt backup evidence accepted")
	}
	if b.Runner.(*configShellRunner).calls != 1 {
		t.Fatal("corruption caused remote effect")
	}
	if err := os.WriteFile(path, []byte(c.CoordinatorConfig.Content), 0640); err != nil {
		t.Fatal(err)
	}
	if err := b.configBefore(context.Background(), c); err == nil {
		t.Fatal("old coordinator backup accepted after config")
	}
}

func TestConfigTransactionInstallRestoreMetadata(t *testing.T) {
	b, c, s, path := configFixture(t)
	if err := b.configPatch(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest(got) != c.CoordinatorConfig.AfterSHA256 {
		t.Fatal("after digest")
	}
	backup := path + ".bak-rc122-pre"
	original, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if digest(original) != c.CoordinatorConfig.BeforeSHA256 {
		t.Fatal("backup digest")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatal("mode not preserved")
	}
	if err = b.configPatch(context.Background(), c, s); err != nil {
		t.Fatal("completed retry verification:", err)
	}
	if err = b.configRestore(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest(got) != c.CoordinatorConfig.BeforeSHA256 {
		t.Fatal("restore digest")
	}
}
func TestConfigTransactionIntentFailureZeroEffects(t *testing.T) {
	b, c, s, path := configFixture(t)
	s.Save = func() error { return errors.New("journal fsync") }
	if err := b.configPatch(context.Background(), c, s); err == nil {
		t.Fatal("accepted unsaved intent")
	}
	if b.Runner.(*configShellRunner).calls != 0 {
		t.Fatal("remote before saved intent")
	}
	if _, err := os.Stat(path + ".bak-rc122-pre"); !os.IsNotExist(err) {
		t.Fatal("backup effect")
	}
}
func TestConfigTransactionAmbiguousEffectNoRetryRestore(t *testing.T) {
	b, c, s, path := configFixture(t)
	r := b.Runner.(*configShellRunner)
	r.ambiguous = true
	if err := b.configPatch(context.Background(), c, s); err == nil {
		t.Fatal("accepted ambiguous effect")
	}
	calls := r.calls
	if err := b.configPatch(context.Background(), c, s); err == nil {
		t.Fatal("retried ambiguous effect")
	}
	if r.calls != calls {
		t.Fatal("second dispatch")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest(got) != c.CoordinatorConfig.AfterSHA256 {
		t.Fatal("effect fault missing")
	}
	r.ambiguous = false
	if err = b.configRestore(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
}
func TestConfigTransactionConflictingBackupValidatorAndFutureRefuse(t *testing.T) {
	for _, kind := range []string{"backup", "validator", "future", "save-completion"} {
		t.Run(kind, func(t *testing.T) {
			b, c, s, path := configFixture(t)
			if kind == "backup" {
				if err := os.WriteFile(path+".bak-rc122-pre", []byte("wrong"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "validator" {
				c.CoordinatorConfig.ValidateCommand = []string{"bash", "-c", "echo valid; exit 42", "{config}"}
			}
			if kind == "future" {
				s.BackupCreated = b.Now().Add(time.Hour)
			}
			saves := 0
			if kind == "save-completion" {
				s.Save = func() error {
					saves++
					if saves > 1 {
						return errors.New("completion rename")
					}
					return nil
				}
			}
			if err := b.configPatch(context.Background(), c, s); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := c.CoordinatorConfig.BeforeSHA256
			if kind == "save-completion" {
				want = c.CoordinatorConfig.AfterSHA256
			}
			if digest(got) != want {
				t.Fatal("unexpected live bytes")
			}
			if kind == "save-completion" {
				// The persisted receipt remains unfinished. Simulate a process reload of
				// that durable state rather than accepting the mutated in-memory value.
				var receipt configReceipt
				if err := evidence(s, "config-intent", &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.Done = false
				s.Save = func() error { return nil }
				if err := record(s, "config-intent", receipt); err != nil {
					t.Fatal(err)
				}
				calls := b.Runner.(*configShellRunner).calls
				if err := b.configPatch(context.Background(), c, s); err == nil {
					t.Fatal("accepted unfinished completion")
				}
				if b.Runner.(*configShellRunner).calls != calls {
					t.Fatal("repeated ambiguous install")
				}
			}
		})
	}
}
func TestDropinShellPreservesOriginalAndAmbiguousRestart(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "systemd", "user", "t3-steward-worker.service.d", "tmpdir-resolved.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("[Service]\nEnvironment=TMPDIR=/old\n")
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
	bindir := filepath.Join(home, "bin")
	if err := os.Mkdir(bindir, 0700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(bindir, "systemctl")
	if err := os.WriteFile(shim, []byte("#!/bin/bash\nif [[ $2 == restart ]]; then echo took-effect; exit 42; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+":"+os.Getenv("PATH"))
	r := &configShellRunner{home: home}
	b := &ProductionBackend{Runner: r}
	c := Config{LocalHost: "agent-b"}
	if _, err := b.on(context.Background(), c, "agent-b", dropinScript, "remove", digest(data), "416", fmt.Sprint(os.Getuid()), fmt.Sprint(os.Getgid())); err == nil {
		t.Fatal("valid output nonzero restart accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("remove effect absent")
	}
	backup := filepath.Join(home, ".local", "state", "t3-steward-rollout-backups", "rc122-tmpdir-dropin.conf")
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatal("lost original")
	}
	if err = os.WriteFile(shim, []byte("#!/bin/bash\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = b.on(context.Background(), c, "agent-b", dropinScript, "restore", digest(data), "416", fmt.Sprint(os.Getuid()), fmt.Sprint(os.Getgid())); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatal("restored content")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatal("restored mode")
	}
}
