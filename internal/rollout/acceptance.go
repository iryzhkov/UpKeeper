package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Re-read the checkout that the actual pull loader consumes, immediately before dispatch.
func (b *ProductionBackend) checkoutFence(ctx context.Context, c Config, s *State) error {
	if s.ReleaseCommit == "" || s.ReleaseSHA256 == "" || s.PackageDigest == "" {
		return errors.New("missing immutable publication binding")
	}
	raw, err := os.ReadFile(c.StatePath + ".review-package.json")
	if err != nil {
		return err
	}
	if digest(raw) != s.PackageDigest {
		return errors.New("accepted package bytes changed")
	}
	var pkg struct{ Head, Release, ConfigDigest string }
	if err = decodeDiagnostic(raw, &pkg); err != nil {
		return err
	}
	if pkg.Head != s.ExpectHead || pkg.Release != s.ReleaseSHA256 || pkg.ConfigDigest != c.Digest() {
		return errors.New("accepted package binding mismatch")
	}
	p, err := b.command(ctx, c, "", "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(p)) != s.ReleaseCommit {
		return errors.New("checkout differs from published HEAD")
	}
	p, err = b.command(ctx, c, "", "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(p))) != 0 {
		return errors.New("published checkout index/worktree is dirty")
	}
	p, err = b.command(ctx, c, "", "upkeeper", "fleet-plan", "--json")
	if err != nil {
		return err
	}
	var plan struct {
		ReleaseSHA256 string `json:"release_sha256"`
	}
	if err = decodeDiagnostic(p, &plan); err != nil {
		return err
	}
	if plan.ReleaseSHA256 != s.ReleaseSHA256 {
		return errors.New("actual checkout canonical release digest differs")
	}
	return b.releaseCI(ctx, c, s)
}

func (b *ProductionBackend) configAtPull(ctx context.Context, c Config, s *State) error {
	if err := freshBackupAt(c, s, b.Now()); err != nil {
		return err
	}
	if c.CoordinatorConfig == nil || !s.Completed["config-patch"] {
		return errors.New("coordinator pull requires completed D2 phase")
	}
	if err := requireAttempt(c, "phase:config-patch", b.Now()); err != nil {
		return err
	}
	var receipt configReceipt
	if err := evidence(s, "config-intent", &receipt); err != nil {
		return err
	}
	p := c.CoordinatorConfig
	if !receipt.Done || receipt.Restored || receipt.Before != p.BeforeSHA256 || receipt.After != p.AfterSHA256 || receipt.IntentAt.IsZero() || receipt.IntentAt.After(b.Now()) {
		return errors.New("coordinator pull requires matching completed D2 receipt")
	}
	_, err := b.configCommand(ctx, c, "verify", p)
	if err != nil {
		return err
	}
	return freshBackupAt(c, s, b.Now())
}

func freshBackupAt(c Config, s *State, now time.Time) error {
	age := now.Sub(s.BackupCreated)
	if s.BackupCreated.IsZero() || age < 0 || age > time.Duration(c.BackupMaxAgeSeconds)*time.Second || s.BackupSnapshot == "" || s.BackupScratch == "" {
		return errors.New("backup snapshot absent, future or stale at effect boundary")
	}
	var kv map[string]string
	if err := evidence(s, "backup", &kv); err != nil {
		return err
	}
	if kv["snapshot"] != s.BackupSnapshot || kv["scratch"] != s.BackupScratch || kv["restore_integrity"] != "ok" || kv["restore_fk_violations"] != "0" {
		return errors.New("effect boundary backup evidence mismatch")
	}
	return nil
}

func (b *ProductionBackend) finalConfiguration(ctx context.Context, c Config, s *State) error {
	if err := b.checkoutFence(ctx, c, s); err != nil {
		return err
	}
	// No components selector or expectation file can hide a configuration failure.
	p, err := b.command(ctx, c, "", "upkeeper", "status", "--json")
	message := ""
	if err != nil {
		message = err.Error()
	}
	persistErr := record(s, "final-configuration-producer", struct{ Stdout, Error string }{string(p), message})
	if err != nil || persistErr != nil {
		return errors.Join(err, persistErr)
	}
	var report struct {
		Schema  int    `json:"schema_version"`
		Command string `json:"command"`
		Commit  string `json:"desired_manifest_commit"`
		Digest  string `json:"release_sha256"`
		Exit    *int   `json:"exit_code"`
		Result  string `json:"result"`
		Hosts   []struct {
			Host       string `json:"host"`
			Reachable  *bool  `json:"reachable"`
			Observe    *bool  `json:"observe_only"`
			Result     string `json:"result"`
			Error      string `json:"error"`
			Components []struct{ Name, State, Error string }
		} `json:"hosts"`
	}
	if err = decodeDiagnostic(p, &report); err != nil {
		return err
	}
	if report.Schema != 1 || report.Command != "status" || report.Commit != s.ReleaseCommit || report.Digest != s.ReleaseSHA256 || report.Exit == nil || *report.Exit != 0 || report.Result != "converged" || len(report.Hosts) == 0 {
		return errors.New("final full status candidate/schema/result mismatch")
	}
	seen := map[string]bool{}
	for _, h := range report.Hosts {
		if seen[h.Host] || h.Host == "" || h.Reachable == nil || !*h.Reachable || h.Observe == nil || !*h.Observe || h.Result != "converged" || h.Error != "" || len(h.Components) == 0 {
			return fmt.Errorf("unresolved full status host %s", h.Host)
		}
		seen[h.Host] = true
		for _, component := range h.Components {
			if component.Name == "" || component.Error != "" || (component.State != "converged" && component.State != "ahead") {
				return fmt.Errorf("unresolved configuration/drift %s/%s", h.Host, component.Name)
			}
		}
	}
	for h := range c.Targets {
		if h != "" && !seen[h] {
			return fmt.Errorf("full status omitted %s", h)
		}
	}
	return record(s, "final-configuration-status", json.RawMessage(p))
}
