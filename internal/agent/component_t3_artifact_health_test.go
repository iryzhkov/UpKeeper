package agent

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func TestT3ControlledHealthIsRequiredDespiteMatchingIdentity(t *testing.T) {
	c, spec, runner, _ := t3ArtifactHost(t, false, false, 0, 0)
	before, _ := t3Observe(c, spec)
	actions := []string{}
	if _, e := t3Apply(c, spec, before, &actions); e != nil {
		t.Fatal(e)
	}
	original := runner.handle
	runner.handle = func(cmd procx.Command) procx.Result {
		result := original(cmd)
		if cmd.Argv[len(cmd.Argv)-1] == "check" {
			result.Code = 1
		}
		return result
	}
	before, e := t3Observe(c, spec)
	if e != nil {
		t.Fatal(e)
	}
	if before["artifact_installed"] != true || before["artifact_served"] != true || before["check_ok"] != false {
		t.Fatalf("fixture does not isolate health: %v", before)
	}
	got, e := t3Apply(c, spec, before, &actions)
	if e != nil || got != "deferred" {
		t.Fatalf("failed health converged or mutated: %s %v", got, e)
	}
}
func TestT3ControlledPostRestartHealthFailureKeepsBackup(t *testing.T) {
	c, spec, runner, _ := t3ArtifactHost(t, false, false, 0, 0)
	original := runner.handle
	restarted := false
	restarts := 0
	runner.handle = func(cmd procx.Command) procx.Result {
		result := original(cmd)
		if len(cmd.Argv) > 2 && cmd.Argv[0] == "systemctl" && cmd.Argv[2] == "restart" {
			restarted = true
			restarts++
		}
		if restarted && cmd.Argv[len(cmd.Argv)-1] == "check" {
			result.Code = 1
		}
		return result
	}
	before, _ := t3Observe(c, spec)
	actions := []string{}
	got, e := t3Apply(c, spec, before, &actions)
	if e == nil || !strings.Contains(e.Error(), "rollback incomplete") || got == "updated" || got == "converged" {
		t.Fatalf("failed post-restart health accepted: %s %v", got, e)
	}
	if restarts != 1 {
		t.Fatal("unguarded restoration restart")
	}
	after, _ := t3Observe(c, spec)
	if after["artifact_installed"] != true {
		t.Fatal("unsafe restoration despite missing idle proof")
	}
}
