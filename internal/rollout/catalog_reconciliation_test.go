package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSupportedDrainingCatalogDriftReconcilesNamedWorkerBeforeDropin(t *testing.T) {
	b, f, _, s := backendFixture(t)
	c := rc122Fixture(t)
	f.c = c
	for _, h := range rc122Order {
		f.upgraded[h] = true
	}
	reconciled := false
	named := 0
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if stdin != "" && strings.Contains(stdin, "worker enroll 'agent-b' --current-catalog") {
			named++
			reconciled = true
			return []byte("enrolled"), nil
		}
		if stdin == dropinScript {
			return nil, errors.New("dropin effect must belong to later phase")
		}
		if name == c.HaikuCheckCommand[0] {
			return jsonBytes(map[string]string{"worker_id": "agent-b", "coordinator_epoch": "2", "model": "claudeAgent/claude-haiku-5-5"}), nil
		}
		raw, err := f.Run(ctx, dir, stdin, name, args...)
		if name == "t3-steward" && len(args) > 0 && args[0] == "worker" && !reconciled {
			var doc map[string]any
			if e := json.Unmarshal(raw, &doc); e != nil {
				return nil, e
			}
			for _, entry := range doc["workers"].([]any) {
				row := entry.(map[string]any)
				if row["requirement"].(map[string]any)["workerId"] == "agent-b" {
					// Exact Steward API uses draining/ready for catalog drift even with
					// requirement.draining false; it does not mean an operator drain.
					row["state"] = "draining"
					row["enrollment"].(map[string]any)["request"].(map[string]any)["catalogRevision"] = 0
				}
			}
			raw = jsonBytes(doc)
		}
		return raw, err
	})
	if err := b.Execute(context.Background(), c, "enroll-worker:agent-b", s); err != nil {
		t.Fatal(err)
	}
	if named != 1 || !reconciled {
		t.Fatal("catalog drift was not reconciled by named enrollment")
	}
}
