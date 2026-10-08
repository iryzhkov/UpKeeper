package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintPullShowsCompletion(t *testing.T) {
	var out bytes.Buffer
	printPull(&out, map[string]any{
		"run_id": "20261007T040000Z-0a1b2c3d", "controller": "agent-a",
		"exit_code": 40, "completion": "partial",
		"hosts": []any{
			map[string]any{
				"host": "agent-a", "reachable": true, "result": "failed", "completion": "partial",
				"components": []any{
					map[string]any{"name": "agent-tooling", "state": "failed", "error": "health check failed"},
					map[string]any{"name": "jocasta", "state": "converged"},
					map[string]any{
						"name": "t3", "state": "skipped",
						"error":              "skipped: depends on failed t3-steward",
						"skipped_dependency": "t3-steward", "skipped_root": "t3-steward",
					},
				},
			},
			map[string]any{
				"host": "normandy", "reachable": true, "result": "converged", "completion": "complete",
				"components": []any{map[string]any{"name": "huyang", "state": "converged"}},
			},
		},
	})
	expected := strings.Join([]string{
		"run 20261007T040000Z-0a1b2c3d controller=agent-a exit=40 completion=partial",
		"agent-a: failed (completion partial)",
		"  agent-tooling: failed",
		"    ! health check failed",
		"  jocasta: converged",
		"  t3: skipped",
		"    ! skipped: depends on failed t3-steward",
		"normandy: converged",
		"  huyang: converged",
		"",
	}, "\n")
	if out.String() != expected {
		t.Errorf("produced\n%s\nexpected\n%s", out.String(), expected)
	}

	// A complete run reads exactly as it always has.
	out.Reset()
	printPull(&out, map[string]any{
		"run_id": "20261007T040000Z-0a1b2c3d", "controller": "normandy",
		"exit_code": 0, "completion": "complete",
		"hosts": []any{map[string]any{
			"host": "normandy", "reachable": true, "result": "converged", "completion": "complete",
		}},
	})
	if out.String() != "run 20261007T040000Z-0a1b2c3d controller=normandy exit=0\nnormandy: converged\n" {
		t.Errorf("a complete run produced %q", out.String())
	}
}

func TestScheduledSummaryNamesPartialHosts(t *testing.T) {
	var errs bytes.Buffer
	printScheduledSummary(&errs, "self-pull", map[string]any{
		"run_id": "20261007T040000Z-0a1b2c3d", "exit_code": 40, "completion": "partial",
		"hosts": []any{map[string]any{
			"host": "agent-a", "reachable": true, "result": "failed", "completion": "partial",
			"components": []any{
				map[string]any{"name": "agent-tooling", "state": "failed", "error": "health check failed"},
				map[string]any{"name": "jocasta", "state": "converged"},
				map[string]any{"name": "huyang", "state": "updated"},
				map[string]any{"name": "t3-steward", "state": "blocked", "error": "blocked: credential missing"},
				map[string]any{"name": "t3", "state": "skipped", "error": "skipped: depends on blocked t3-steward"},
			},
		}},
	}, "/runs/20261007T040000Z-0a1b2c3d.json", 40)
	expected := strings.Join([]string{
		"upkeeper self-pull: agent-a agent-tooling failed: health check failed",
		// The three counts partition the host's components.
		"upkeeper self-pull: agent-a partial: 2 succeeded, 2 not completed, 1 skipped",
		"upkeeper self-pull: run 20261007T040000Z-0a1b2c3d recorded at /runs/20261007T040000Z-0a1b2c3d.json; exit 40",
		"",
	}, "\n")
	if errs.String() != expected {
		t.Errorf("produced\n%s\nexpected\n%s", errs.String(), expected)
	}
}
