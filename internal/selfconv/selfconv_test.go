package selfconv_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
)

const (
	before    = "1111111111111111111111111111111111111111"
	candidate = "2222222222222222222222222222222222222222"
)

// gitScript answers the plumbing a checkout inspection runs. Anything it was
// not told about fails, so a test can never accidentally reach a real remote.
type gitScript struct {
	root      string
	toplevel  string
	status    string
	head      string
	branch    string
	origin    string
	gate      string
	mergeFail bool
	calls     []string
}

func (g *gitScript) run(command procx.Command) procx.Result {
	joined := strings.Join(command.Argv, " ")
	g.calls = append(g.calls, joined)
	switch {
	case strings.HasSuffix(joined, "rev-parse --show-toplevel"):
		return procx.Result{Stdout: g.toplevel + "\n"}
	case strings.Contains(joined, "status --porcelain"):
		return procx.Result{Stdout: g.status}
	case strings.HasSuffix(joined, "rev-parse --verify HEAD"):
		return procx.Result{Stdout: g.head + "\n"}
	case strings.HasSuffix(joined, "branch --show-current"):
		return procx.Result{Stdout: g.branch + "\n"}
	case strings.HasSuffix(joined, "remote get-url origin"):
		return procx.Result{Stdout: g.origin + "\n"}
	case strings.Contains(joined, "fetch --quiet --no-tags"):
		return procx.Result{}
	case strings.HasSuffix(joined, "rev-parse FETCH_HEAD"):
		return procx.Result{Stdout: candidate + "\n"}
	case strings.Contains(joined, "merge-base --is-ancestor"):
		if g.mergeFail {
			return procx.Result{Code: 1}
		}
		return procx.Result{}
	case strings.HasPrefix(joined, "gh run list"):
		return procx.Result{Stdout: g.gate}
	case strings.Contains(joined, "merge --ff-only"):
		g.head = candidate
		return procx.Result{}
	default:
		return procx.Result{Code: 1, Stderr: "unscripted: " + joined}
	}
}

func newScript(t *testing.T) *gitScript {
	t.Helper()
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolved = root
	}
	return &gitScript{
		root: root, toplevel: resolved, head: before, branch: "main",
		origin: "https://github.com/iryzhkov/UpKeeper.git",
		gate:   `[{"headSha":"` + candidate + `","status":"completed","conclusion":"success"}]`,
	}
}

func TestCheckoutRevisionRefusesADirtyOrForeignTree(t *testing.T) {
	script := newScript(t)
	revision, err := selfconv.CheckoutRevision(script.root, script.run)
	if err != nil {
		t.Fatalf("CheckoutRevision: %v", err)
	}
	if revision != before {
		t.Errorf("revision is %q", revision)
	}

	// Work in progress is preserved rather than converged over.
	script.status = " M src/dev_fleet/pull.py\n"
	_, err = selfconv.CheckoutRevision(script.root, script.run)
	if err == nil ||
		err.Error() != "--self refuses a dirty checkout; commit or preserve work first" {
		t.Fatalf("produced %v", err)
	}

	script.status = ""
	script.toplevel = filepath.Join(script.root, "nested")
	_, err = selfconv.CheckoutRevision(script.root, script.run)
	if err == nil || err.Error() != "--self must use the dev-fleet checkout root" {
		t.Fatalf("produced %v", err)
	}
}

func TestLocalHostNeedsExactlyOneInventoryMatch(t *testing.T) {
	config, err := inventory.Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	// No machine identity of this fleet is likely to match a synthetic
	// inventory, so the refusal is what a mismatched host must see.
	empty := &inventory.Config{Root: config.Root, Hosts: map[string]inventory.HostSpec{}}
	_, err = selfconv.LocalHost(empty)
	if err == nil ||
		err.Error() != "--self requires exactly one inventory match for this machine-id" {
		t.Fatalf("produced %v", err)
	}
}

func TestRefreshRequiresMainTheCanonicalOriginAndAPassedGate(t *testing.T) {
	cases := []struct {
		name     string
		breakIt  func(*gitScript)
		expected string
	}{
		{
			"a branch other than main",
			func(s *gitScript) { s.branch = "migration/go-binary" },
			"self refresh requires main",
		},
		{
			"a remote that is not the canonical one",
			func(s *gitScript) { s.origin = "https://github.com/someone/UpKeeper.git" },
			"self refresh requires the canonical UpKeeper GitHub origin",
		},
		{
			"a gate that has not finished",
			func(s *gitScript) {
				s.gate = `[{"headSha":"` + candidate +
					`","status":"in_progress","conclusion":null}]`
			},
			"candidate main has no successful GitHub push gate; checkout unchanged",
		},
		{
			"a gate for a different commit",
			func(s *gitScript) {
				s.gate = `[{"headSha":"` + before +
					`","status":"completed","conclusion":"success"}]`
			},
			"candidate main has no successful GitHub push gate; checkout unchanged",
		},
		{
			"no gate at all",
			func(s *gitScript) { s.gate = `[]` },
			"candidate main has no successful GitHub push gate; checkout unchanged",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			script := newScript(t)
			testCase.breakIt(script)
			_, err := selfconv.Refresh(script.root, script.run)
			if err == nil || err.Error() != testCase.expected {
				t.Fatalf("produced %v, expected %q", err, testCase.expected)
			}
			for _, call := range script.calls {
				if strings.Contains(call, "merge --ff-only") {
					t.Error("a refused refresh still moved the checkout")
				}
			}
		})
	}
}

func TestRefreshFastForwardsAGatedCandidate(t *testing.T) {
	script := newScript(t)
	moved, err := selfconv.Refresh(script.root, script.run)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if moved != candidate {
		t.Errorf("refreshed to %q", moved)
	}
}

func TestTheHoldMarkerSurvivesAnUnderstoodFailureAndClearsOnSuccess(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "scheduled-holds")
	spec := inventory.HostSpec{Name: "omarchy-pc"}
	marker := filepath.Join(directory, "omarchy-pc.hold")

	holds := &selfconv.HoldRunner{
		Directory: directory,
		Inner: func(
			inventory.HostSpec, agent.Payload, time.Duration, bool,
		) map[string]any {
			return map[string]any{"reachable": true, "result": "updated"}
		},
	}
	if result := holds.Run(spec, agent.Payload{}, time.Minute, true); result["result"] != "updated" {
		t.Fatalf("result is %#v", result)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("an understood result left a hold behind")
	}

	// A result nobody can interpret leaves the marker, and the next run
	// refuses to dispatch until a human has looked at the host.
	holds.Inner = func(inventory.HostSpec, agent.Payload, time.Duration, bool) map[string]any {
		return map[string]any{"reachable": false, "result": "failed"}
	}
	holds.Run(spec, agent.Payload{}, time.Minute, true)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the hold was not kept: %v", err)
	}
	dispatched := false
	holds.Inner = func(inventory.HostSpec, agent.Payload, time.Duration, bool) map[string]any {
		dispatched = true
		return nil
	}
	result := holds.Run(spec, agent.Payload{}, time.Minute, true)
	if dispatched {
		t.Error("a held host was dispatched to again")
	}
	if !strings.Contains(result["error"].(string), "scheduled hold requires operator observation") {
		t.Errorf("result is %#v", result)
	}
}

func TestNotificationOnlySpeaksWhenSomethingHappened(t *testing.T) {
	cases := []struct {
		name     string
		report   map[string]any
		wanted   bool
		severity string
	}{
		{
			"a quiet run says nothing",
			map[string]any{"exit_code": 0, "hosts": []any{
				map[string]any{"result": "converged"},
			}},
			false, "",
		},
		{
			"a changed host is news",
			map[string]any{"exit_code": 0, "hosts": []any{
				map[string]any{"result": "updated"},
			}},
			true, "info",
		},
		{
			"a retired legacy updater is news even when the host converged",
			map[string]any{"exit_code": 0, "hosts": []any{map[string]any{
				"result":         "converged",
				"legacy_updater": map[string]any{"state": "updated"},
			}}},
			true, "info",
		},
		{
			"a failure is always news",
			map[string]any{"exit_code": 40, "hosts": []any{
				map[string]any{"result": "failed"},
			}},
			true, "fail",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			severity, title, wanted := selfconv.Notification(testCase.report)
			if wanted != testCase.wanted {
				t.Fatalf("wanted %v, expected %v", wanted, testCase.wanted)
			}
			if !wanted {
				return
			}
			if severity != testCase.severity || title == "" {
				t.Errorf("severity %q title %q", severity, title)
			}
		})
	}
}

func TestPostReportMakesExactlyOneAttempt(t *testing.T) {
	attempts := 0
	runner := func(command procx.Command) procx.Result {
		attempts++
		if command.Argv[0] != "feed" || command.Argv[1] != "post" {
			t.Errorf("unexpected command %v", command.Argv)
		}
		if strings.Contains(command.Input, "ghp_") {
			t.Error("a secret reached the fleet feed")
		}
		return procx.Result{Code: 1, Stderr: "feed unavailable"}
	}
	report := map[string]any{
		"exit_code": 40, "run_id": "20260914T090000Z-0a1b2c3d",
		"hosts": []any{map[string]any{
			"host": "omarchy-pc", "result": "failed",
			"error": "token ghp_abcdefghijklmnopqrstuvwxyz0123",
			"components": []any{
				map[string]any{"name": "huyang", "state": "failed", "error": "build"},
				map[string]any{"name": "t3", "state": "converged"},
			},
		}},
	}
	err := selfconv.PostReport(report, runner)
	if err == nil {
		t.Fatal("a failed post must be reported")
	}
	if attempts != 1 {
		// A lost feed response does not justify a duplicate event in the
		// fleet's shared history.
		t.Errorf("made %d attempts", attempts)
	}
}
