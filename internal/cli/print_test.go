package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The text reports are what an operator reads at a terminal. Their exact line
// shapes are ported from the Python implementation and compared literally.
func TestDoctorText(t *testing.T) {
	var out bytes.Buffer
	printDoctor(&out, map[string]any{
		"run_id": "20260914T090000Z-0a1b2c3d", "controller": "normandy",
		"result": "partial", "exit_code": 20,
		"hosts": []any{
			map[string]any{"name": "omarchy-pc", "reachable": true, "result": "healthy"},
			map[string]any{
				"name": "normandy", "reachable": true, "result": "failed",
				"failed_checks": []any{"linger", "os"},
			},
			map[string]any{"name": "homelab", "reachable": false, "error": "probe-failed"},
			map[string]any{"name": "laptop", "reachable": false},
		},
	})
	expected := strings.Join([]string{
		"run 20260914T090000Z-0a1b2c3d controller=normandy doctor=partial exit=20",
		"omarchy-pc: healthy",
		"normandy: failed failed=linger,os",
		"homelab: offline (probe-failed)",
		"laptop: offline (unreachable)",
		"",
	}, "\n")
	if out.String() != expected {
		t.Errorf("produced\n%s\nexpected\n%s", out.String(), expected)
	}
}

func TestStatusText(t *testing.T) {
	var out bytes.Buffer
	printStatus(&out, map[string]any{
		"run_id": "20260914T090000Z-0a1b2c3d", "controller": "unknown",
		"result": "drift", "exit_code": 10,
		"hosts": []any{map[string]any{
			"name": "omarchy-pc", "reachable": true, "result": "drift",
			"components": []any{
				map[string]any{"name": "agent99", "state": "decision-required"},
				map[string]any{"name": "t3-steward", "state": "converged"},
			},
		}},
	})
	expected := "run 20260914T090000Z-0a1b2c3d controller=unknown status=drift exit=10\n" +
		"omarchy-pc: drift [agent99=decision-required, t3-steward=converged]\n"
	if out.String() != expected {
		t.Errorf("produced\n%s\nexpected\n%s", out.String(), expected)
	}
}

// A drifting component's reason is printed under its host so an operator does
// not have to open the JSON to learn why the scheduled pull has not converged it.
// A host that reports its binary gets one line naming it; one that does not
// prints exactly what it always did.
func TestStatusTextNamesTheBinaryAHostReports(t *testing.T) {
	var out bytes.Buffer
	printStatus(&out, map[string]any{
		"run_id": "20260914T090000Z-0a1b2c3d", "controller": "normandy",
		"result": "converged", "exit_code": 0,
		"hosts": []any{
			map[string]any{"name": "homelab", "reachable": true, "result": "converged", "components": []any{},
				"upkeeper": map[string]any{"binary": map[string]any{
					"version": "v0.1.31", "commit": strings.Repeat("b", 40), "generation": strings.Repeat("1", 64)}}},
			map[string]any{"name": "laptop", "reachable": true, "result": "converged", "components": []any{},
				"upkeeper": map[string]any{"binary": map[string]any{
					"version": "v0.1.30", "commit": strings.Repeat("a", 40), "generation": nil}}},
			map[string]any{"name": "normandy", "reachable": true, "result": "converged", "components": []any{}},
		},
	})
	expected := strings.Join([]string{
		"run 20260914T090000Z-0a1b2c3d controller=normandy status=converged exit=0",
		"homelab: converged []",
		"  upkeeper v0.1.31 commit bbbbbbbbbbbb generation 111111111111",
		"laptop: converged []",
		"  upkeeper v0.1.30 commit aaaaaaaaaaaa generation unmanaged",
		"normandy: converged []",
		"",
	}, "\n")
	if out.String() != expected {
		t.Errorf("got\n%s\nwant\n%s", out.String(), expected)
	}
}

func TestStatusTextShowsDriftReasons(t *testing.T) {
	var out bytes.Buffer
	printStatus(&out, map[string]any{
		"run_id": "20260914T090000Z-0a1b2c3d", "controller": "normandy",
		"result": "drift", "exit_code": 10,
		"hosts": []any{map[string]any{
			"name": "laptop", "reachable": true, "result": "drift",
			"components": []any{
				map[string]any{"name": "huyang", "state": "converged"},
				map[string]any{"name": "t3-steward", "state": "drift",
					"reason": "scheduled hold marker present for 26h0m0s; clear it with: upkeeper hold clear laptop --reason TEXT"},
			},
		}},
	})
	expected := "run 20260914T090000Z-0a1b2c3d controller=normandy status=drift exit=10\n" +
		"laptop: drift [huyang=converged, t3-steward=drift]\n" +
		"  t3-steward: scheduled hold marker present for 26h0m0s; clear it with: upkeeper hold clear laptop --reason TEXT\n"
	if out.String() != expected {
		t.Errorf("produced\n%s\nexpected\n%s", out.String(), expected)
	}
}

func TestPushTextSaysNothingAboutCommitsAManifestOnlyRunNeverConsidered(t *testing.T) {
	var out bytes.Buffer
	printPush(&out, map[string]any{
		"summary":   []any{"huyang abcdef123456 (main)"},
		"committed": nil,
		"pushed":    []any{},
	})
	if out.String() != "  huyang abcdef123456 (main)\n" {
		t.Errorf("produced %q", out.String())
	}

	out.Reset()
	printPush(&out, map[string]any{
		"summary": []any{"huyang abcdef123456 (main)"},
		// A committing run that found nothing to record does report that.
		"committed": nil, "head": "1111111111111111111111111111111111111111",
		"pushed": []any{},
	})
	if !strings.Contains(out.String(), "nothing new to commit") {
		t.Errorf("produced %q", out.String())
	}

	out.Reset()
	printPush(&out, map[string]any{
		"summary":   []any{},
		"committed": "1234567890abcdef1234567890abcdef12345678",
		"head":      "1234567890abcdef1234567890abcdef12345678",
		"pushed":    []any{"origin main"},
	})
	if out.String() != "committed 1234567890ab\npushed origin main\n" {
		t.Errorf("produced %q", out.String())
	}
}

func TestPullText(t *testing.T) {
	var out bytes.Buffer
	printPull(&out, map[string]any{
		"run_id": "20260914T090000Z-0a1b2c3d", "controller": "normandy",
		"exit_code": 40, "dry_run": true,
		"hosts": []any{
			map[string]any{"host": "laptop", "self_managed": true, "result": "skipped"},
			map[string]any{
				"host": "homelab", "reachable": false, "error": "unreachable before dispatch",
				"stderr": "ssh: connect to host homelab port 22: No route to host\n",
			},
			map[string]any{
				"host": "omarchy-pc", "reachable": true, "result": "failed",
				"legacy_updater": map[string]any{
					"state": "updated", "actions": []any{"disabled and deleted legacy updater units"},
				},
				"components": []any{map[string]any{
					"name": "huyang", "state": "failed",
					"actions": []any{"fetched origin main"},
					"error":   "build failed",
				}},
				"harness": map[string]any{"claude": true, "codex": false, "opencode": nil},
			},
		},
	})
	expected := strings.Join([]string{
		"run 20260914T090000Z-0a1b2c3d controller=normandy exit=40 (dry run)",
		"laptop: self-managed (local timer; no controller SSH)",
		"homelab: offline (unreachable before dispatch)",
		"  ssh: connect to host homelab port 22: No route to host",
		"omarchy-pc: failed",
		"  legacy updater: updated",
		"    - disabled and deleted legacy updater units",
		"  huyang: failed",
		"    - fetched origin main",
		"    ! build failed",
		"  harness without huyang registration: codex, opencode",
		"",
	}, "\n")
	if out.String() != expected {
		t.Errorf("produced\n%s\nexpected\n%s", out.String(), expected)
	}
}
