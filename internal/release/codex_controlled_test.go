package release

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func controlledSpec() map[string]any {
	asset := func(t string) map[string]any {
		n := "codex-controlled-0.155.1-rc.1-" + t + ".tar.zst"
		return map[string]any{"asset": n, "sha256": strings.Repeat("a", 64), "receipt_asset": strings.TrimSuffix(n, ".tar.zst") + ".receipt.json", "receipt_sha256": strings.Repeat("b", 64)}
	}
	return map[string]any{"kind": "github-release", "repository": "iryzhkov/codex", "tag": "controlled-v0.155.1-rc.1", "source_commit": strings.Repeat("c", 40), "source_tree": strings.Repeat("d", 40), "assets": map[string]any{ControlledCodexTargets[0]: asset(ControlledCodexTargets[0]), ControlledCodexTargets[1]: asset(ControlledCodexTargets[1])}, "hosts": []any{"laptop"}}
}

func controlledReceipt(s *ControlledCodex, target string) ControlledCodexReceipt {
	a := s.Assets[target]
	return ControlledCodexReceipt{Schema: "codex-controlled-runtime-receipt-v1", Repository: s.Repository, Tag: s.Tag, SourceCommit: s.SourceCommit, SourceTree: s.SourceTree, CargoVersion: "0.155.1", Target: target, VoiceTarget: "x86_64-unknown-linux-gnu", RunnerArch: "X64", Archive: a.Asset, ArchiveSHA256: a.SHA256, RustcVV: "rustc", CargoV: "cargo", VoiceHostLDD: "ok", RustyV8ArchiveSHA256: strings.Repeat("e", 64), RustyV8BindingSHA256: strings.Repeat("f", 64), QualificationWorkflow: "controlled-runtime-release", QualificationRunID: "123", QualificationRunAttempt: "1", FilesSHA256: map[string]string{"bin/codex": strings.Repeat("1", 64)}}
}

func TestControlledCodexClosedContract(t *testing.T) {
	if _, e := DecodeControlledCodex(controlledSpec()); e != nil {
		t.Fatal(e)
	}
	for _, f := range []func(map[string]any){func(m map[string]any) { m["extra"] = true }, func(m map[string]any) { m["tag"] = "v0.1.0" }, func(m map[string]any) { m["hosts"] = []any{} }, func(m map[string]any) { m["source_tree"] = "bad" }} {
		m := controlledSpec()
		f(m)
		if _, e := DecodeControlledCodex(m); e == nil {
			t.Fatal("accepted invalid contract")
		}
	}
}

func TestControlledCodexReceiptContract(t *testing.T) {
	s, _ := DecodeControlledCodex(controlledSpec())
	target := ControlledCodexTargets[0]
	r := controlledReceipt(s, target)
	body, _ := json.Marshal(r)
	if _, e := DecodeControlledCodexReceipt(body, s, target); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*ControlledCodexReceipt){
		func(r *ControlledCodexReceipt) { r.SourceTree = strings.Repeat("0", 40) },
		func(r *ControlledCodexReceipt) { r.CargoVersion = "0.155.2" },
		func(r *ControlledCodexReceipt) { r.VoiceTarget = "aarch64-unknown-linux-gnu" },
		func(r *ControlledCodexReceipt) { r.RunnerArch = "ARM64" },
		func(r *ControlledCodexReceipt) { r.QualificationWorkflow = "other" },
	} {
		r := controlledReceipt(s, target)
		mutate(&r)
		body, _ = json.Marshal(r)
		if _, e := DecodeControlledCodexReceipt(body, s, target); e == nil {
			t.Fatal("accepted mismatched receipt")
		}
	}
}

func TestControlledCodexCaptureRequiresInstalledIdentity(t *testing.T) {
	_, e := CaptureControlledCodex(t.TempDir(), func(procx.Command) procx.Result {
		t.Fatal("capture contacted release host before verifying installation")
		return procx.Result{}
	}, controlledSpec())
	if e == nil {
		t.Fatal("captured uninstalled controlled runtime")
	}
}
