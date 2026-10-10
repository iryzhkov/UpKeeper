package rollout

// Historical orchestration is a test-only harness for inherited backend
// qualification. The rc122 command uses RunHost and its protocol tests.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Run requires explicit authority even on resume. A host interrupted between
// mutation and its success checkpoint is rolled back before further advancement.
// Failed rollouts are terminal journals, never silently retried.
func Run(ctx context.Context, c Config, b Backend, yes bool, out io.Writer) error {
	steps, err := Plan(c)
	// Historical accepted fixtures predate D2; they qualify the inherited backend.
	// rc122 configuration transactions are exercised by the production-path tests.
	if c.CoordinatorConfig == nil {
		var historical []Step
		for _, step := range steps {
			if step.Action != "config-patch" {
				historical = append(historical, step)
			}
		}
		steps = historical
	}
	if err != nil {
		return err
	}
	for _, step := range steps {
		fmt.Fprintf(out, "%s: %s\n  rollback: %s\n", step.Action, step.Description, step.Rollback)
	}
	if !yes {
		return errors.New("every mutation requires --yes")
	}
	if err = os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(c.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another rollout owns the journal")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	s, err := loadState(c)
	if err != nil {
		return err
	}
	if s.FleetApproval != "" && c.FleetApproval != "" && s.FleetApproval != c.FleetApproval {
		return errors.New("fleet approval differs from the recorded authorization")
	}
	if strings.HasPrefix(s.InFlight, "rollback:") {
		return resumeRollback(ctx, c, b, s)
	}
	if s.Failed != "" {
		return fmt.Errorf("rollout stopped: %s; retain fences and recovery evidence", s.Failed)
	}
	if strings.HasPrefix(s.InFlight, "host:") {
		host := strings.TrimPrefix(s.InFlight, "host:")
		if _, recorded := s.Extra["host-pre:"+host]; recorded {
			return failHost(ctx, c, b, s, s.InFlight, errors.New("interrupted host effect; reconciling by immutable rollback"))
		}
		// No host effect can precede its durable prior snapshot. Retry preflight
		// after a hard interruption just as after a pre-convergence refusal.
		s.InFlight = ""
	}
	if s.InFlight == "publish" && len(s.Extra["publication-started"]) > 0 {
		return errors.New("publication was interrupted; reconcile all remote receipts before resuming")
	}
	if err = s.Save(); err != nil {
		return err
	}
	if len(s.Completed) == 0 {
		if err = b.Check(ctx, c); err != nil {
			return err
		}
	}
	for _, step := range steps {
		if s.Completed[step.Action] {
			continue
		}
		if (step.Action == "backup" || (strings.HasPrefix(step.Action, "host:") && step.Action != "host:"+c.Canary)) && s.FleetApproval == "" {
			if strings.TrimSpace(c.FleetApproval) == "" {
				return errors.New("fleet approval required after canary and soak; record it in FleetApproval and resume with --yes")
			}
			// Persist authority before the binding backup or any fleet host.
			s.FleetApproval = c.FleetApproval
			if err = s.Save(); err != nil {
				return err
			}
		}
		if strings.HasPrefix(step.Action, "host:") {
			// Recheck CI and fences before every host, including after process restart.
			if err = b.Execute(ctx, c, "release-ci", s); err != nil {
				return err
			}
		}
		s.InFlight = step.Action
		if err = s.Save(); err != nil {
			return err
		}
		if step.Action == "backup" && s.Completed["config-patch"] {
			if production, ok := b.(*ProductionBackend); ok {
				if err = production.configRestore(ctx, c, s); err != nil {
					return err
				}
				// The historical retry harness retains the restored receipt and
				// acknowledgement before opening a new configuration transaction.
				if err = record(s, "legacy-config-restored", json.RawMessage(s.Extra["config-intent"])); err != nil {
					return err
				}
				if err = os.Rename(attemptPath(c, "phase:config-patch"), c.StatePath+".legacy-config-restored-attempt"); err != nil {
					return err
				}
				delete(s.Extra, "config-intent")
				delete(s.Completed, "config-patch")
				if err = s.Save(); err != nil {
					return err
				}
			}
		}
		var configAck func() error
		if step.Action == "config-patch" {
			if production, ok := b.(*ProductionBackend); ok {
				configAck, err = beginAttempt(c, "phase:config-patch", "", production.Now())
				if err != nil {
					return err
				}
			}
		}
		if step.Action == "soak" {
			err = soak(ctx, c, b, s)
		} else {
			err = b.Execute(ctx, c, step.Action, s)
		}
		if err != nil {
			if strings.HasPrefix(step.Action, "host:") {
				var refusal *hostPreflightError
				if errors.As(err, &refusal) {
					s.InFlight = ""
					if refusal.refreshBackup {
						delete(s.Completed, "backup")
					}
					return errors.Join(err, s.Save())
				}
				return failHost(ctx, c, b, s, step.Action, err)
			}
			if step.Action == "verify" || step.Action == "enroll" {
				return failFleet(ctx, c, b, s, step.Action, err)
			}
			// CI/review refusal is resumable; backend checkpoints retain prior effects.
			return fmt.Errorf("%s: %w", step.Action, err)
		}
		s.Completed[step.Action] = true
		s.InFlight = ""
		if err = s.Save(); err != nil {
			return err
		}
		if configAck != nil {
			if err = configAck(); err != nil {
				return err
			}
		}
	}
	return nil
}

// attachLegacyCoordinatorConfig supplies actual configuration bytes and a staged
// validator to inherited backend tests. It never uses an operator configuration.
func attachLegacyCoordinatorConfig(t *testing.T, c *Config) string {
	t.Helper()
	before, after := "before: original\n", "after: reviewed\n"
	home := t.TempDir()
	path := filepath.Join(home, ".config", "t3-steward", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(before), 0640); err != nil {
		t.Fatal(err)
	}
	c.CoordinatorConfig = &ConfigChange{
		BeforeSHA256: digest([]byte(before)), AfterSHA256: digest([]byte(after)), Content: after,
		ValidateCommand: []string{"python3", "-c", "import pathlib,sys,json,hashlib; data=pathlib.Path(sys.argv[1]).read_bytes(); assert data == b'after: reviewed\\n'; print(json.dumps(dict(schema_version=1,kind='config-validation',valid=True,file_sha256=hashlib.sha256(data).hexdigest())))", "{config}"},
	}
	return home
}

func runLegacyConfigCommand(ctx context.Context, home, stdin, name string, args ...string) ([]byte, error) {
	var cmd *exec.Cmd
	if name == "ssh" {
		// Run the exact remote Bash argv locally in the isolated fake host.
		cmd = exec.CommandContext(ctx, "bash", "-c", args[len(args)-1])
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
	}
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.CombinedOutput()
}

func soak(ctx context.Context, c Config, b Backend, s *State) error {
	var start time.Time
	if raw := s.Extra["soak-start"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &start); err != nil {
			return err
		}
	} else {
		start = time.Now()
		raw, _ := json.Marshal(start)
		s.Extra["soak-start"] = raw
		if err := s.Save(); err != nil {
			return err
		}
	}
	for time.Since(start) < time.Duration(c.SoakSeconds)*time.Second {
		if err := b.Execute(ctx, c, "canary-observe", s); err != nil {
			return failHost(ctx, c, b, s, "host:"+c.Canary, err)
		}
		t := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	if err := b.Execute(ctx, c, "canary-observe", s); err != nil {
		return failHost(ctx, c, b, s, "host:"+c.Canary, err)
	}
	return nil
}
func failHost(ctx context.Context, c Config, b Backend, s *State, action string, cause error) error {
	s.Failed = action + ": " + cause.Error()
	s.InFlight = "rollback:" + strings.TrimPrefix(action, "host:")
	if err := s.Save(); err != nil {
		return errors.Join(cause, err)
	}
	// Recovery has a separate bounded context so interruption does not cancel rollback.
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	if err := b.Execute(recovery, c, s.InFlight, s); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback failed (resume required): %w", err))
	}
	s.InFlight = ""
	if err := s.Save(); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("%w; immutable rollback completed; rollout stopped", cause)
}
func failFleet(ctx context.Context, c Config, b Backend, s *State, action string, cause error) error {
	hosts := append([]string{c.Canary}, c.FleetOrder...)
	var queue []string
	for i := len(hosts) - 1; i >= 0; i-- {
		if s.Completed["host:"+hosts[i]] {
			queue = append(queue, "rollback:"+hosts[i])
		}
	}
	s.Failed = action + ": " + cause.Error()
	if len(queue) == 0 {
		s.InFlight = ""
		if err := s.Save(); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	s.InFlight = queue[0]
	raw, _ := json.Marshal(queue)
	s.Extra["rollback-queue"] = raw
	if err := s.Save(); err != nil {
		return errors.Join(cause, err)
	}
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	return errors.Join(cause, resumeRollback(recovery, c, b, s))
}
func resumeRollback(ctx context.Context, c Config, b Backend, s *State) error {
	queue := []string{s.InFlight}
	if raw := s.Extra["rollback-queue"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &queue); err != nil {
			return err
		}
		if len(queue) == 0 || queue[0] != s.InFlight {
			return errors.New("rollback queue and journal differ")
		}
	}
	for len(queue) > 0 {
		if err := b.Execute(ctx, c, queue[0], s); err != nil {
			return err
		}
		queue = queue[1:]
		raw, _ := json.Marshal(queue)
		s.Extra["rollback-queue"] = raw
		s.InFlight = ""
		if len(queue) > 0 {
			s.InFlight = queue[0]
		}
		if err := s.Save(); err != nil {
			return err
		}
	}
	return fmt.Errorf("rollback completed; rollout stopped: %s", s.Failed)
}
