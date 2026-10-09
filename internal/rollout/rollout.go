// Package rollout implements a journaled binary-only maintenance walk.
package rollout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Version             string
	PreviousRelease     string
	StewardRepo         string
	UpKeeperRepo        string
	Checkout            string
	SetupRepository     string
	StatePath           string
	LocalHost           string
	CoordinatorHost     string
	Canary              string
	LastHost            string
	LaptopHost          string
	ReviewFile          string
	FleetApproval       string
	Remotes             []string
	FleetOrder          []string
	TimerHosts          []string
	DrainedHosts        []string
	ActiveWorkers       []string
	Targets             map[string]string
	Assets              map[string]string
	QuietCommand        []string
	BackupCommand       []string
	BackupMaxAgeSeconds int
	ReadinessSeconds    int
	SoakSeconds         int
}

type State struct {
	ConfigDigest   string
	FleetApproval  string
	Completed      map[string]bool
	InFlight       string
	Failed         string
	ReleaseCommit  string
	ExpectHead     string
	ReleaseSHA256  string
	PackageDigest  string
	TimerPrior     map[string]string
	BackupCreated  time.Time
	BackupSnapshot string
	BackupScratch  string
	Extra          map[string]json.RawMessage
	Save           func() error `json:"-"`
}

// Backend is the effect boundary; Check must never mutate. Execute must persist
// evidence via State.Save before an effect that needs prior-state recovery.
type Backend interface {
	Check(context.Context, Config) error
	Execute(context.Context, Config, string, *State) error
}

type Step struct {
	Action      string
	Description string
	Rollback    string
}

func (c Config) Validate() error {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`).MatchString(c.Version) {
		return errors.New("version must be an exact release version")
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(c.PreviousRelease) {
		return errors.New("previous release must be an immutable full commit")
	}
	if !filepath.IsAbs(c.Checkout) || !filepath.IsAbs(c.StatePath) || c.Checkout == "" || c.StatePath == "" || c.CoordinatorHost == "" || c.Canary == "" || c.LastHost == "" || len(c.FleetOrder) == 0 || len(c.Remotes) < 2 {
		return errors.New("checkout, state, coordinator, canary, last host, fleet order and two remotes are required")
	}
	if c.Canary == c.CoordinatorHost || c.FleetOrder[len(c.FleetOrder)-1] != c.LastHost {
		return errors.New("canary must differ from coordinator; configured last host must be last")
	}
	if c.BackupMaxAgeSeconds <= 0 || c.BackupMaxAgeSeconds > 3600 || c.ReadinessSeconds < 60 || c.ReadinessSeconds > 180 || c.SoakSeconds < 1800 {
		return errors.New("backup age must be 1..3600s, readiness 60..180s, soak at least 1800s")
	}
	seen := map[string]bool{}
	for _, h := range append([]string{c.Canary}, c.FleetOrder...) {
		if h == c.LaptopHost || seen[h] {
			return fmt.Errorf("excluded or duplicate rollout host %q", h)
		}
		seen[h] = true
	}
	if !seen[c.CoordinatorHost] {
		return errors.New("coordinator missing from fleet order")
	}
	for _, list := range [][]string{append([]string{c.LocalHost, c.CoordinatorHost, c.Canary, c.LastHost, c.LaptopHost}, c.FleetOrder...), c.TimerHosts, c.DrainedHosts, c.ActiveWorkers} {
		unique := map[string]bool{}
		for _, h := range list {
			if h == "" {
				continue
			}
			if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`).MatchString(h) || c.Targets[h] == "" {
				return fmt.Errorf("host %q needs a valid name and transport target", h)
			}
			unique[h] = true
		}
	}
	if len(c.TimerHosts) == 0 || !contains(c.TimerHosts, c.LocalHost) || !contains(c.TimerHosts, c.CoordinatorHost) || (c.LaptopHost != "" && !contains(c.TimerHosts, c.LaptopHost)) {
		return errors.New("timer hosts must include controller, coordinator and excluded laptop")
	}
	for _, h := range c.ActiveWorkers {
		if contains(c.DrainedHosts, h) || h == c.LaptopHost {
			return fmt.Errorf("worker %s is both active and excluded/drained", h)
		}
	}
	for _, h := range append(append([]string{}, c.ActiveWorkers...), c.DrainedHosts...) {
		if !seen[h] {
			return fmt.Errorf("worker %s missing from rollout targets", h)
		}
	}
	if !contains(c.ActiveWorkers, c.Canary) {
		return errors.New("canary must be an active worker")
	}
	if len(c.QuietCommand) == 0 {
		return errors.New("a coordinator quiet predicate is required")
	}
	return nil
}
func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
func (c Config) Digest() string {
	// Fleet authorization is obtained after the canary and journaled separately.
	c.FleetApproval = ""
	b, _ := json.Marshal(c)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Plan only builds values: no files, commands or remote effects.
func Plan(c Config) ([]Step, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rollback := "upkeeper rollback --to " + c.PreviousRelease + " --components t3-steward"
	steps := []Step{
		{"fence", "record prior timer states; stop every reachable configured self timer; excluded laptop stays masked", "retain fences; timer release requires separate reviewed full-effect disposition"},
		{"prepare", "author binary-only candidate; verify assets, scope, full/scoped/rollback previews; freeze review package", "retain unpublished candidate and evidence"},
		{"publish", "require ACCEPT naming both fences and PACKAGE digest; recheck fences; authored push to configured remotes", "review and publish a forward release; never force-push"},
		{"release-ci", "require exact release commit on every remote with successful CI", "keep timers fenced"},
		{"host:" + c.Canary, "idle single-host canary; verify binary, both new service generations, unchanged t3code, drains and epoch", rollback + " --hosts " + c.Canary},
		{"soak", "observe canary for the configured soak before any fleet host", "rollback canary to immutable previous release"},
	}
	for _, h := range c.FleetOrder {
		if h == c.CoordinatorHost {
			steps = append(steps, Step{"backup", "quiet coordinator; stopped local snapshot, verify and scratch restore drill; restart both DB holders", "restart prior units; preserve snapshot and failed database for supervised restore"})
		}
		steps = append(steps, Step{"host:" + h, "component-scoped convergence and per-host readiness gates; coordinator reconnect wait bounded", rollback + " --hosts " + h})
	}
	steps = append(steps, Step{"enroll", "enroll only configured active workers whose accepted catalog differs; preserve drains", "forward per-worker --current-catalog enrollment after rollback"}, Step{"verify", "verify coordinator and every worker, masks, drains and scheduled holds; retain timer fences", "immutable release rollback or supervised coordinator snapshot restore"})
	return steps, nil
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return c, err
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return c, errors.New("config contains trailing JSON")
	}
	return c, c.Validate()
}

func loadState(c Config) (*State, error) {
	s := &State{ConfigDigest: c.Digest(), Completed: map[string]bool{}, TimerPrior: map[string]string{}, Extra: map[string]json.RawMessage{}}
	b, err := os.ReadFile(c.StatePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(b, s); err != nil {
			return nil, err
		}
		if s.ConfigDigest != c.Digest() {
			return nil, errors.New("configuration differs from the frozen rollout; use the original config")
		}
	}
	if s.Completed == nil || s.TimerPrior == nil || s.Extra == nil {
		return nil, errors.New("incomplete rollout journal")
	}
	s.Save = func() error { return saveState(c.StatePath, s) }
	return s, nil
}
func saveState(path string, s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".rollout-state-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Run requires explicit authority even on resume. A host interrupted between
// mutation and its success checkpoint is rolled back before further advancement.
// Failed rollouts are terminal journals, never silently retried.
func Run(ctx context.Context, c Config, b Backend, yes bool, out io.Writer) error {
	steps, err := Plan(c)
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
		return failHost(ctx, c, b, s, s.InFlight, errors.New("interrupted host effect; reconciling by immutable rollback"))
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
		if strings.HasPrefix(step.Action, "host:") {
			// Recheck CI and fences before every host, including after process restart.
			if err = b.Execute(ctx, c, "release-ci", s); err != nil {
				return err
			}
			if strings.TrimPrefix(step.Action, "host:") != c.Canary && s.FleetApproval == "" {
				if strings.TrimSpace(c.FleetApproval) == "" {
					return errors.New("fleet approval required after canary and soak; record it in FleetApproval and resume with --yes")
				}
				// Authorization is separate from the frozen operational configuration.
				// Persist it before dispatching the first fleet host.
				s.FleetApproval = c.FleetApproval
				if err = s.Save(); err != nil {
					return err
				}
			}
		}
		s.InFlight = step.Action
		if err = s.Save(); err != nil {
			return err
		}
		if step.Action == "soak" {
			err = soak(ctx, c, b, s)
		} else {
			err = b.Execute(ctx, c, step.Action, s)
		}
		if err != nil {
			if strings.HasPrefix(step.Action, "host:") {
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
	}
	return nil
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
