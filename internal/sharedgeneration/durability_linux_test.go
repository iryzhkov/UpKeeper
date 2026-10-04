//go:build linux

package sharedgeneration

import (
	"errors"
	"testing"
)

func TestIdempotenceCannotHidePriorDurabilityUncertainty(t *testing.T) {
	repo, commit := fixture(t, "approved")
	s := mustOpen(t)
	hit := false
	s.h.fault = func(p string) error {
		if p == "fsync:selection" {
			hit = true
			return errors.New("uncertain selector durability")
		}
		return nil
	}
	if i, e := s.Stage(repo, commit); !hit || !errors.Is(e, ErrRecoveryRequired) || i.GenerationID != "" {
		t.Fatal("false commit success", e)
	}
	hit = false
	s.h.fault = func(p string) error {
		if p == "fsync:selection-idempotent" {
			hit = true
			return errors.New("still uncertain")
		}
		return nil
	}
	if i, e := s.Stage(repo, commit); !hit || !errors.Is(e, ErrRecoveryRequired) || i.GenerationID != "" {
		t.Fatal("idempotence bypassed durability", e)
	}
	s.h = hooks{}
	if i, e := s.Recover(); e != nil || i.GenerationID == "" {
		t.Fatal("recovery could not confirm valid candidate", e)
	}
}
