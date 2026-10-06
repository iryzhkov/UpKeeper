package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func TestT3RepairUnitOwnedMissingAndLegacy(t *testing.T) {
	c, tx, _, _ := t3TxPrepare(t, true)
	runner := &adapterTestRunner{}
	c.Runner = runner.run
	runner.handle = func(cmd procx.Command) procx.Result {
		return procx.Result{Stdout: "{ path=" + tx.link + " ; argv[]=" + tx.link + " start ; ignore_errors=no ; }"}
	}
	adapterTestWrite(t, filepath.Join(tx.packagePath, "dist/bin.mjs"), "legacy")
	if e := t3SwapLink("../lib/node_modules/t3/dist/bin.mjs", tx.link); e != nil {
		t.Fatal(e)
	}
	if _, e := t3UnitBinding(c, "t3code.service", tx.prefix); e != nil {
		t.Fatalf("supported legacy link refused: %v", e)
	}
	if e := os.RemoveAll(tx.packagePath); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(tx.link); e != nil {
		t.Fatal(e)
	}
	if _, e := t3UnitBinding(c, "t3code.service", tx.prefix); e != nil {
		t.Fatalf("owned absent installation refused: %v", e)
	}
	adapterTestWrite(t, filepath.Join(tx.packagePath, "foreign"), "ambiguous")
	if _, e := t3UnitBinding(c, "t3code.service", tx.prefix); e == nil {
		t.Fatal("ambiguous missing launcher accepted")
	}
}

func TestT3RepairSeedStartGuard(t *testing.T) {
	runner := &adapterTestRunner{}
	c := adapterTestContext(t, runner)
	runner.handle = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "LoadState") {
			return procx.Result{Stdout: "LoadState=not-found"}
		}
		return procx.Result{}
	}
	actions := []string{}
	checked := false
	_, e := seedT3Service(c, &actions, func() error {
		checked = true
		return errors.New("injected changed mapping/identity/idle")
	})
	if e == nil || !checked || runner.ran("enable", "--now") {
		t.Fatalf("start guard bypass: %v", e)
	}
	if !runner.ran("daemon-reload") {
		t.Fatal("guard did not run after staging unit")
	}
}
