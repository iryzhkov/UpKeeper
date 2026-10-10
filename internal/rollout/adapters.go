package rollout

import (
	"context"
	"errors"
	"time"
)

// Fence is the U1 integration seam. A hold implementation replaces both setting
// and verification; the rollout never assumes that a hold stops a timer.
type Fence interface {
	Check(context.Context, Config, bool) error
	Set(context.Context, Config, *State) error
}

// CoordinatorBackup is the Steward-owned backup integration seam. CreateAndVerify
// must attest a stopped local snapshot plus a scratch restore drill by populating
// BackupSnapshot, BackupScratch, BackupCreated and Extra["backup"]. An exit-zero
// verify-only command is insufficient. The rollout still gates quiet, fences,
// readiness and the evidence's integrity, foreign keys and age.
type CoordinatorBackup interface {
	CreateAndVerify(context.Context, Config, *State) error
}

func (b *ProductionBackend) timers(ctx context.Context, c Config, fenced bool) error {
	if b.Fence != nil {
		if _, err := b.laptop(ctx, c); err != nil {
			return err
		}
		return b.Fence.Check(ctx, c, fenced)
	}
	return b.timerCheck(ctx, c, fenced)
}
func (b *ProductionBackend) fence(ctx context.Context, c Config, s *State) error {
	if b.Fence != nil {
		if err := b.Fence.Set(ctx, c, s); err != nil {
			return err
		}
		return b.timers(ctx, c, true)
	}
	return b.setTimerFence(ctx, c, s)
}
func (b *ProductionBackend) createBackup(ctx context.Context, c Config, s *State) error {
	if b.CoordinatorBackup == nil {
		return b.backup(ctx, c, s)
	}
	if err := b.quiet(ctx, c); err != nil {
		return err
	}
	if err := b.timers(ctx, c, true); err != nil {
		return err
	}
	if err := b.CoordinatorBackup.CreateAndVerify(ctx, c, s); err != nil {
		return err
	}
	var kv map[string]string
	if err := evidence(s, "backup", &kv); err != nil {
		return err
	}
	if kv["restore_integrity"] != "ok" || kv["restore_fk_violations"] != "0" || s.BackupSnapshot == "" || s.BackupScratch == "" || s.BackupCreated.IsZero() || b.Now().Before(s.BackupCreated) || b.Now().Sub(s.BackupCreated) > time.Duration(c.BackupMaxAgeSeconds)*time.Second {
		return errors.New("backup adapter lacks a successful stopped snapshot, scratch drill or healthy coordinator")
	}
	if err := b.wait(ctx, c, func() error {
		id, err := b.identity(ctx, c)
		if err != nil {
			return err
		}
		if id.Health != "healthy" {
			return errors.New("backup adapter lacks a successful stopped snapshot, scratch drill or healthy coordinator")
		}
		return nil
	}); err != nil {
		return err
	}
	if s.Save != nil {
		return s.Save()
	}
	return nil
}
