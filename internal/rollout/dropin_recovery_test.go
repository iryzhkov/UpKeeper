package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNamedD5RecoveryRealRestoreAfterInterruptionAndFirstFailure(t *testing.T) {
	for _, fault := range []string{"kill-point", "first-restart42", "unreadable-backup", "metadata-drift"} {
		t.Run(fault, func(t *testing.T) {
			c := rc122Fixture(t)
			c.LocalHost = "agent-b"
			c.TimerHosts = append(c.TimerHosts, "agent-b")
			home := t.TempDir()
			path := filepath.Join(home, ".config/systemd/user/t3-steward-worker.service.d/tmpdir-resolved.conf")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte("[Service]\nEnvironment=TMPDIR=/old\n")
			if err := os.WriteFile(path, original, 0640); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(home, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			shim := filepath.Join(bin, "systemctl")
			if err := os.WriteFile(shim, []byte("#!/bin/bash\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			shell := &configShellRunner{home: home}
			f := &backendFake{c: c, upgraded: map[string]bool{"normandy": true, "agent-b": true}, fenced: map[string]bool{}}
			b := NewProductionBackend()
			b.Now = time.Now
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				if stdin == dropinScript {
					return shell.Run(ctx, dir, stdin, name, args...)
				}
				return f.Run(ctx, dir, stdin, name, args...)
			})
			if err := durableDirectory(filepath.Dir(c.StatePath)); err != nil {
				t.Fatal(err)
			}
			if _, err := beginAttempt(c, "enroll:agent-b", "agent-b", b.Now()); err != nil {
				t.Fatal(err)
			}
			raw, err := b.on(context.Background(), c, "agent-b", dropinScript, "inspect")
			if err != nil {
				t.Fatal(err)
			}
			var receipt dropinReceipt
			if err = json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			receipt.Host = "agent-b"
			receipt.IntentAt = b.Now()
			s := &State{ConfigDigest: c.Digest(), Completed: map[string]bool{}, TimerPrior: map[string]string{}, Extra: map[string]json.RawMessage{}, InFlight: "dropin:agent-b"}
			s.Extra["dropin:agent-b"] = jsonBytes(receipt)
			if err = saveState(c.StatePath, s); err != nil {
				t.Fatal(err)
			}
			if _, err = b.on(context.Background(), c, "agent-b", dropinScript, "remove", receipt.SHA256, fmt.Sprint(receipt.Mode), fmt.Sprint(receipt.UID), fmt.Sprint(receipt.GID)); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(home, ".local/state/t3-steward-rollout-backups/rc122-tmpdir-dropin.conf")
			if fault == "first-restart42" {
				if err = os.WriteFile(shim, []byte("#!/bin/bash\nif [[ $2 == restart ]]; then echo effect; exit 42; fi\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "unreadable-backup" {
				if os.Getuid() == 0 {
					t.Skip("requires real nonroot permissions")
				}
				if err = os.Chmod(backup, 0); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "metadata-drift" {
				if err = os.Chmod(backup, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = RecoverHost(context.Background(), c, b, "agent-b", true)
			if err == nil {
				t.Fatal("recovery completed a failed attempt")
			}
			if fault != "kill-point" {
				failed, _ := loadState(c)
				if failed.Failed == "" || failed.Completed["enroll-worker:agent-b"] {
					t.Fatal("failed restore lost permanent failure")
				}
				if err = os.Chmod(backup, 0640); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(shim, []byte("#!/bin/bash\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
				err = RecoverHost(context.Background(), c, b, "agent-b", true)
			}
			if err == nil || !strings.Contains(err.Error(), "restored") {
				t.Fatalf("recovery boundary: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(original) {
				t.Fatal("original bytes lost", err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0640 {
				t.Fatal("original metadata lost", err)
			}
			loaded, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Failed == "" || loaded.InFlight != "" || loaded.Completed["enroll-worker:agent-b"] {
				t.Fatal("restore advanced attempt")
			}
			var recovered dropinReceipt
			if err = evidence(loaded, "dropin:agent-b", &recovered); err != nil || !recovered.Restored || recovered.Done {
				t.Fatal("restore receipt", err)
			}
			if err = FinishWorker(context.Background(), c, b, "agent-b", true); err == nil {
				t.Fatal("failed D5 forwarded enrollment")
			}
			if err = attemptPending(c, b.Now()); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatal("original unfinished enrollment evidence erased", err)
			}
		})
	}
}
