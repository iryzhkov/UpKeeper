package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

// Runtime makes elapsed observation and persistence faults reproducible without
// sleeping or invoking production transport in tests.
type Runtime struct {
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
	Save  func(string, *State) error
}

func runtimeFor(b Backend) Runtime {
	r := Runtime{Now: time.Now, Save: saveState, Sleep: func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}}
	if p, ok := b.(*ProductionBackend); ok {
		r.Now = p.Now
		r.Sleep = p.Sleep
	}
	return r
}

var rc122Order = []string{"agent-b", "normandy", "homelab", "agent-a", "omarchy-pc"}

// ValidateWalk checks the complete production contract before read-only CLI planning.
func ValidateWalk(c Config) error { return validateWalk(c) }
func validateWalk(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !reflect.DeepEqual(append([]string{c.Canary}, c.FleetOrder...), rc122Order) ||
		c.CoordinatorHost != "normandy" || c.LastHost != "omarchy-pc" {
		return errors.New("rc122 requires agent-b, normandy, homelab, agent-a, omarchy-pc in fixed order")
	}
	if c.CoordinatorConfig == nil || !validArgv(c.QuietCommand) || (len(c.BackupCommand) > 0 && !validArgv(c.BackupCommand)) || !validArgv(c.ActivityCommand) || !validArgv(c.HaikuCheckCommand) || !validArgv(c.DropinTestCommand) {
		return errors.New("reviewed coordinator configuration and activity/collection predicate required")
	}
	return c.CoordinatorConfig.Validate()
}

// withJournal serializes all readers that authorize effects; a malformed,
// unreadable or conflicting journal never means an empty attempt inventory.
func withJournal(c Config, r Runtime, fn func(*State) error) error {
	if err := durableDirectory(filepath.Dir(c.StatePath)); err != nil {
		return err
	}
	for _, p := range []string{c.StatePath, c.StatePath + ".lock", filepath.Dir(c.StatePath)} {
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("journal paths must not be symlinks")
		}
		if st.Mode().Perm()&0022 != 0 {
			return errors.New("journal paths must not be writable by other users")
		}
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
	s.Save = func() error { return r.Save(c.StatePath, s) }
	if s.FleetApproval != "" && c.FleetApproval != "" && s.FleetApproval != c.FleetApproval {
		return errors.New("fleet approval differs from recorded authority")
	}
	return fn(s)
}
func terminal(s *State, cause error) error {
	s.Failed = cause.Error()
	return errors.Join(cause, s.Save())
}
func recoverHost(ctx context.Context, c Config, b Backend, s *State, host string, cause error) error {
	acknowledge, err := beginRecoveryFor(c, "rollback", host, runtimeFor(b).Now())
	if err != nil {
		return errors.Join(cause, err)
	}
	s.Failed = cause.Error()
	s.InFlight = "rollback:" + host
	if err := s.Save(); err != nil {
		return errors.Join(cause, err)
	}
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	if err := b.Execute(recovery, c, s.InFlight, s); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback recovery required: %w", err))
	}
	s.InFlight = ""
	if err := s.Save(); err != nil {
		return errors.Join(cause, err)
	}
	if err := acknowledge(); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("%w; rollback completed; attempt remains terminal", cause)
}
func guardAttempt(_ context.Context, c Config, _ Backend, s *State, r Runtime) error {
	if s.Failed != "" {
		return fmt.Errorf("rollout stopped: %s; named recover command only", s.Failed)
	}
	if err := attemptPending(c, r.Now()); err != nil {
		return err
	}
	if err := requiredAttempts(c, s, r.Now()); err != nil {
		return err
	}
	if s.InFlight != "" {
		return fmt.Errorf("unfinished %s; no continuation or next host; named recovery only", s.InFlight)
	}
	return nil
}
func executeSaved(ctx context.Context, c Config, b Backend, s *State, action string, now time.Time) error {
	complete, err := beginAttempt(c, "phase:"+action, "", now)
	if err != nil {
		return err
	}
	s.InFlight = action
	if err := s.Save(); err != nil {
		return err
	}
	if err := b.Execute(ctx, c, action, s); err != nil {
		return terminal(s, fmt.Errorf("%s: %w", action, err))
	}
	s.Completed[action] = true
	s.InFlight = ""
	if err := s.Save(); err != nil {
		return terminal(s, err)
	}
	return complete()
}

type ObservationReceipt struct {
	Host              string
	ConfigDigest      string
	ReleaseCommit     string
	Started, Finished time.Time
	Samples           int
	Identity          json.RawMessage
}

// observeHost begins a new continuously monitored interval. A persisted old
// timestamp cannot satisfy this gate after restart or a missed sample.
func observeHost(ctx context.Context, c Config, b Backend, s *State, host string, r Runtime) error {
	duration := 15 * time.Minute
	if host == c.Canary {
		duration = time.Duration(c.SoakSeconds) * time.Second
	}
	start := r.Now()
	complete, err := beginAttempt(c, "phase:observe:"+host, "", start)
	if err != nil {
		return err
	}
	last := start
	samples := 0
	s.InFlight = "observe:" + host
	if err := record(s, "observation-intent:"+host, start); err != nil {
		return err
	}
	for {
		now := r.Now()
		if now.Before(last) || now.Sub(last) > 60*time.Second {
			return errors.New("observation clock moved or monitoring gap exceeded")
		}
		if err := b.Execute(ctx, c, "observe:"+host, s); err != nil {
			return err
		}
		samples++
		last = r.Now()
		if last.Before(now) || last.Sub(now) > 60*time.Second {
			return errors.New("observation sample exceeded monitored interval")
		}
		if last.Sub(start) >= duration {
			break
		}
		if err := r.Sleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
	receipt := ObservationReceipt{Host: host, ConfigDigest: c.Digest(), ReleaseCommit: s.ReleaseCommit,
		Started: start, Finished: last, Samples: samples, Identity: s.Extra["identity-post:"+host]}
	if len(receipt.Identity) == 0 {
		return errors.New("observation lacks host identity/epoch evidence")
	}
	if err := record(s, "observation:"+host, receipt); err != nil {
		return err
	}
	s.Completed["host:"+host] = true
	if host == c.Canary {
		s.Completed["soak"] = true
	}
	s.InFlight = ""
	if err := s.Save(); err != nil {
		return terminal(s, err)
	}
	return complete()
}
func priorObservation(c Config, s *State, host string, r Runtime) error {
	for _, action := range []string{"run:" + host, "phase:observe:" + host} {
		if err := requireAttempt(c, action, r.Now()); err != nil {
			return err
		}
	}
	var receipt ObservationReceipt
	if err := evidence(s, "observation:"+host, &receipt); err != nil {
		return err
	}
	duration := 15 * time.Minute
	if host == c.Canary {
		duration = time.Duration(c.SoakSeconds) * time.Second
	}
	if !s.Completed["host:"+host] || receipt.Host != host || receipt.ConfigDigest != c.Digest() ||
		receipt.ReleaseCommit != s.ReleaseCommit || receipt.Started.IsZero() || receipt.Finished.After(r.Now()) ||
		receipt.Finished.Sub(receipt.Started) < duration || receipt.Samples < 1+int(duration/(60*time.Second)) ||
		len(receipt.Identity) == 0 {
		return errors.New("invalid, future or mismatched predecessor observation receipt")
	}
	return nil
}
func RunHost(ctx context.Context, c Config, b Backend, host string, yes bool, out io.Writer) error {
	return RunHostWithRuntime(ctx, c, b, host, yes, out, runtimeFor(b))
}
func RunHostWithRuntime(ctx context.Context, c Config, b Backend, host string, yes bool, out io.Writer, r Runtime) error {
	if err := validateWalk(c); err != nil {
		return err
	}
	if !contains(rc122Order, host) {
		return errors.New("explicit rollout host required")
	}
	if !yes {
		return errors.New("every mutation requires --yes")
	}
	if r.Now == nil || r.Sleep == nil || r.Save == nil {
		return errors.New("incomplete rollout runtime")
	}
	return withJournal(c, r, func(s *State) error {
		if err := guardAttempt(ctx, c, b, s, r); err != nil {
			return err
		}
		if s.Completed["host:"+host] {
			return errors.New("host already completed; no repeat or resume")
		}
		index := 0
		for rc122Order[index] != host {
			index++
		}
		if index > 0 {
			if err := priorObservation(c, s, rc122Order[index-1], r); err != nil {
				return err
			}
			if s.FleetApproval == "" && strings.TrimSpace(c.FleetApproval) == "" {
				return errors.New("parent release fleet approval required")
			}
		}
		complete, err := beginAttempt(c, "run:"+host, host, r.Now())
		if err != nil {
			return err
		}
		if index > 0 {
			if s.FleetApproval == "" {
				if strings.TrimSpace(c.FleetApproval) == "" {
					return errors.New("parent release fleet approval required")
				}
				s.FleetApproval = c.FleetApproval
				if err := s.Save(); err != nil {
					return err
				}
			}
		} else {
			if len(s.Completed) == 0 {
				if err := b.Check(ctx, c); err != nil {
					return err
				}
			}
			for _, action := range []string{"fence", "prepare", "publish", "release-ci"} {
				if !s.Completed[action] {
					if err := executeSaved(ctx, c, b, s, action, r.Now()); err != nil {
						return err
					}
				}
			}
		}
		accept, err := beginAttempt(c, "phase:accept:"+host, "", r.Now())
		if err != nil {
			return err
		}
		if err := b.Execute(ctx, c, "release-ci", s); err != nil {
			return terminal(s, err)
		}
		if err := accept(); err != nil {
			return err
		}
		if host == c.CoordinatorHost {
			for _, action := range []string{"backup", "config-patch"} {
				if err := executeSaved(ctx, c, b, s, action, r.Now()); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(out, "host %s: converge, verify and observe; rollback --to %s --hosts %s\n", host, c.PreviousRelease, host)
		s.InFlight = "host:" + host
		if err := s.Save(); err != nil {
			return err
		}
		if err := b.Execute(ctx, c, s.InFlight, s); err != nil {
			if len(s.Extra["host-pre:"+host]) == 0 {
				return terminal(s, err)
			}
			return recoverHost(ctx, c, b, s, host, err)
		}
		if err := observeHost(ctx, c, b, s, host, r); err != nil {
			// Even persistence failure cannot authorize another host. Saved InFlight
			// remains the uncertainty fence if recording the failure also fails.
			return recoverHost(ctx, c, b, s, host, err)
		}
		return complete()
	})
}

// FinishWorker is a separate named invocation after every host's receipt. It
// never undrains a worker, targets --all, or rolls back the entire fleet.
func FinishWorker(ctx context.Context, c Config, b Backend, host string, yes bool) error {
	if err := validateWalk(c); err != nil {
		return err
	}
	if !yes || !contains(c.ActiveWorkers, host) {
		return errors.New("named active worker and --yes required")
	}
	r := runtimeFor(b)
	return withJournal(c, r, func(s *State) error {
		if err := guardAttempt(ctx, c, b, s, r); err != nil {
			return err
		}
		for _, h := range rc122Order {
			if err := priorObservation(c, s, h, r); err != nil {
				return err
			}
		}
		if s.Completed["enroll-worker:"+host] {
			return errors.New("worker enrollment already completed")
		}
		if host == "agent-a" && !s.Completed["enroll-worker:agent-b"] {
			return errors.New("agent-b D5 must precede agent-a")
		}
		complete, err := beginAttempt(c, "enroll:"+host, host, r.Now())
		if err != nil {
			return err
		}
		if err := executeSaved(ctx, c, b, s, "enroll-worker:"+host, r.Now()); err != nil {
			return err
		}
		if host == "agent-a" || host == "agent-b" {
			if err := executeSaved(ctx, c, b, s, "dropin:"+host, r.Now()); err != nil {
				return err
			}
		}
		return complete()
	})
}
func VerifyRollout(ctx context.Context, c Config, b Backend, yes bool) error {
	if err := validateWalk(c); err != nil {
		return err
	}
	if !yes {
		return errors.New("verification receipt requires --yes")
	}
	r := runtimeFor(b)
	return withJournal(c, r, func(s *State) error {
		if err := guardAttempt(ctx, c, b, s, r); err != nil {
			return err
		}
		for _, h := range rc122Order {
			if err := priorObservation(c, s, h, r); err != nil {
				return err
			}
		}
		for _, h := range c.ActiveWorkers {
			if !s.Completed["enroll-worker:"+h] {
				return errors.New("named active enrollment incomplete")
			}
		}
		complete, err := beginAttempt(c, "verify", "", r.Now())
		if err != nil {
			return err
		}
		if err := executeSaved(ctx, c, b, s, "verify", r.Now()); err != nil {
			return err
		}
		return complete()
	})
}

// RecoverHost reconciles only the named uncertain host. It cannot complete an
// attempt, erase its fence, advance hosts or enroll workers.
func RecoverHost(ctx context.Context, c Config, b Backend, host string, yes bool) error {
	if err := validateWalk(c); err != nil {
		return err
	}
	if !yes || !contains(rc122Order, host) {
		return errors.New("named recovery host and --yes required")
	}
	return withJournal(c, runtimeFor(b), func(s *State) error {
		if s.InFlight == "dropin:"+host || s.InFlight == "dropin-restore:"+host || (s.Failed != "" && len(s.Extra["dropin:"+host]) > 0) {
			intent, done, err := readAttempt(c, attemptPath(c, "enroll:"+host), runtimeFor(b).Now())
			if err != nil || intent.Action != "enroll:"+host || done {
				return errors.New("D5 recovery requires unfinished matching enrollment intent")
			}
			var receipt dropinReceipt
			if err := evidence(s, "dropin:"+host, &receipt); err != nil {
				return err
			}
			if receipt.Host != host || receipt.Absent {
				return errors.New("D5 recovery requires matching original backup receipt")
			}
			acknowledge, err := beginRecovery(c, host, runtimeFor(b).Now())
			if err != nil {
				return err
			}
			if s.Failed == "" {
				s.Failed = "interrupted D5 attempt; recovery only"
			}
			s.InFlight = "dropin-restore:" + host
			if err := s.Save(); err != nil {
				return err
			}
			if err := b.Execute(ctx, c, s.InFlight, s); err != nil {
				return err
			}
			s.InFlight = ""
			if err := s.Save(); err != nil {
				return err
			}
			if err := acknowledge(); err != nil {
				return err
			}
			return errors.New("original drop-in restored; failed attempt remains terminal")
		}
		if strings.HasPrefix(s.InFlight, "rollback:") && s.InFlight != "rollback:"+host {
			return errors.New("another host owns rollback recovery")
		}
		if s.InFlight != "" && s.InFlight != "host:"+host && s.InFlight != "observe:"+host &&
			s.InFlight != "rollback:"+host && !(host == c.CoordinatorHost && (s.InFlight == "config-patch" || s.InFlight == "config-restore")) {
			return errors.New("recovery host differs from saved intent")
		}
		_, complete, err := readAttempt(c, attemptPath(c, "run:"+host), runtimeFor(b).Now())
		if err != nil {
			return fmt.Errorf("recovery requires valid saved invocation intent: %w", err)
		}
		if complete && s.Failed == "" && s.InFlight == "" {
			return errors.New("completed host does not authorize uncertain-effect recovery")
		}
		if len(s.Extra["host-pre:"+host]) > 0 {
			return recoverHost(ctx, c, b, s, host, errors.New("operator requested host rollback recovery"))
		}
		if host == c.CoordinatorHost && len(s.Extra["config-intent"]) > 0 {
			s.Failed = "coordinator configuration attempt failed; recovery only"
			s.InFlight = "config-restore"
			if err := s.Save(); err != nil {
				return err
			}
			if err := b.Execute(ctx, c, "config-restore", s); err != nil {
				return err
			}
			s.InFlight = ""
			return errors.Join(errors.New("configuration restored; attempt remains terminal"), s.Save())
		}
		return errors.New("no saved host effect to recover; retain attempt and obtain newly reviewed plan")
	})
}

// PrepareRollout stops after the unpublished package, so the parent can obtain
// its independent review before any publication or host convergence.
func PrepareRollout(ctx context.Context, c Config, b Backend, yes bool) error {
	if err := validateWalk(c); err != nil {
		return err
	}
	if !yes {
		return errors.New("preparation effects require --yes")
	}
	r := runtimeFor(b)
	return withJournal(c, r, func(s *State) error {
		if err := guardAttempt(ctx, c, b, s, r); err != nil {
			return err
		}
		if s.Completed["prepare"] {
			return errors.New("candidate already prepared; retain reviewed bytes")
		}
		if err := b.Check(ctx, c); err != nil {
			return err
		}
		for _, action := range []string{"fence", "prepare"} {
			if !s.Completed[action] {
				if err := executeSaved(ctx, c, b, s, action, r.Now()); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
