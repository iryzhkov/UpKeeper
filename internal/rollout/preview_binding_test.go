package rollout

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

var previewBindingFaults = []string{
	"positive", "wrong-release", "stale-release", "missing-release", "empty-release", "noncanonical-release", "invalid-release",
	"wrong-parent", "stale-parent", "missing-parent", "empty-parent", "noncanonical-parent", "invalid-parent",
	"missing-schema", "wrong-schema", "wrong-command", "missing-dry-run", "not-dry-run", "missing-exit-code", "nonzero-exit-code",
}

func faultFullPreview(t *testing.T, raw []byte, fault string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	switch fault {
	case "wrong-release":
		doc["release_sha256"] = strings.Repeat("9", 64)
	case "stale-release":
		doc["release_sha256"] = strings.Repeat("d", 64)
	case "missing-release":
		delete(doc, "release_sha256")
	case "empty-release":
		doc["release_sha256"] = ""
	case "noncanonical-release":
		doc["release_sha256"] = strings.ToUpper(doc["release_sha256"].(string))
	case "invalid-release":
		doc["release_sha256"] = strings.Repeat("z", 64)
	case "wrong-parent":
		doc["desired_manifest_commit"] = strings.Repeat("8", 40)
	case "stale-parent":
		doc["desired_manifest_commit"] = strings.Repeat("a", 40)
	case "missing-parent":
		delete(doc, "desired_manifest_commit")
	case "empty-parent":
		doc["desired_manifest_commit"] = ""
	case "noncanonical-parent":
		doc["desired_manifest_commit"] = strings.ToUpper(doc["desired_manifest_commit"].(string))
	case "invalid-parent":
		doc["desired_manifest_commit"] = strings.Repeat("z", 40)
	case "missing-schema":
		delete(doc, "schema_version")
	case "wrong-schema":
		doc["schema_version"] = 2
	case "wrong-command":
		doc["command"] = "status"
	case "missing-dry-run":
		delete(doc, "dry_run")
	case "not-dry-run":
		doc["dry_run"] = false
	case "missing-exit-code":
		delete(doc, "exit_code")
	case "nonzero-exit-code":
		doc["exit_code"] = 40
	}
	return jsonBytes(doc)
}

// Run actual production preparation and publication with complete per-host
// inventories. Only the producer envelope differs; no live transport is used.
func TestAstraFullPreviewPublicationBinding(t *testing.T) {
	for _, fault := range previewBindingFaults {
		t.Run(fault, func(t *testing.T) {
			b, f, c, s := backendFixture(t)
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				p, err := f.Run(ctx, dir, stdin, name, args...)
				if name == "upkeeper" && len(args) > 0 && args[0] == "pull" &&
					contains(args, "--all") && contains(args, "--dry-run") && err == nil {
					return faultFullPreview(t, p, fault), nil
				}
				return p, err
			})
			if err := b.fence(context.Background(), c, s); err != nil {
				t.Fatal(err)
			}
			err := b.prepare(context.Background(), c, s)
			if err == nil {
				writeReview(t, &c, s)
				err = b.publish(context.Background(), c, s)
			}
			if fault == "positive" {
				if err != nil || f.pushes != 1 {
					t.Fatal("exact producer identity refused", err, f.pushes)
				}
			} else {
				if err == nil || f.pushes != 0 || s.PackageDigest != "" || len(s.Extra["preview-full"]) != 0 {
					t.Fatal("contradictory producer authorized preparation/publication", err, f.pushes)
				}
			}
		})
	}
}

// A self-consistent immutable package and exact review cannot make the preview
// for a different parent or release authoritative at the publication boundary.
func TestRepair3PublicationRevalidatesFullPreviewBinding(t *testing.T) {
	for _, fault := range previewBindingFaults {
		t.Run(fault, func(t *testing.T) {
			b, f, c, s := backendFixture(t)
			if err := b.fence(context.Background(), c, s); err != nil {
				t.Fatal(err)
			}
			if err := b.prepare(context.Background(), c, s); err != nil {
				t.Fatal(err)
			}
			s.Extra["preview-full"] = faultFullPreview(t, s.Extra["preview-full"], fault)
			raw := jsonBytes(reviewedPackage{s.ExpectHead, s.ReleaseSHA256, s.ConfigDigest, s.TimerPrior, s.Extra})
			s.PackageDigest = digest(raw)
			if err := os.WriteFile(c.StatePath+".review-package.json", raw, 0600); err != nil {
				t.Fatal(err)
			}
			writeReview(t, &c, s)
			err := b.publish(context.Background(), c, s)
			if fault == "positive" {
				if err != nil || f.pushes != 1 {
					t.Fatal("exact package refused", err, f.pushes)
				}
			} else if err == nil || f.pushes != 0 {
				t.Fatal("exact review authorized contradictory preview", err, f.pushes)
			}
		})
	}
}

func TestRepair3PreparationParentHEADFence(t *testing.T) {
	b, f, c, s := backendFixture(t)
	if err := b.fence(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	headReads := 0
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "rev-parse" {
			headReads++
			if headReads == 2 {
				// Another checkout becomes the full-preview preparation parent.
				f.head = strings.Repeat("8", 40)
			}
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	err := b.prepare(context.Background(), c, s)
	if err == nil || !strings.Contains(err.Error(), "preparation parent HEAD") ||
		s.PackageDigest != "" || f.pushes != 0 {
		t.Fatal("moved preparation parent authorized package", err)
	}
}
