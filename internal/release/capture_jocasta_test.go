package release_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func jocastaCaptureFixture(t *testing.T) (string, string, map[string]any, procx.Runner) {
	t.Helper()
	acceptAnyEnvironment(t)
	root, home := t.TempDir(), t.TempDir()
	manifest := pairedManifestFixture(t)
	component(manifest, "agent-config")["files"] = []any{}
	delete(component(manifest, "t3-steward"), "session_hook_sha256")
	jocasta := map[string]any{
		"kind": "github-release", "repository": "iryzhkov/jocasta",
		"version": "0.1.6", "commit": strings.Repeat("b", 40),
		"url":    "https://jocasta.example.test",
		"assets": map[string]any{"linux_amd64": strings.Repeat("a", 64), "linux_arm64": strings.Repeat("b", 64)},
		"hosts":  map[string]any{"homelab": "secretref:jocasta/homelab", "normandy": "secretref:jocasta/normandy"},
	}
	components(manifest)[release.JocastaComponent] = jocasta
	for _, name := range []string{"omarchy-setup", "huyang"} {
		if err := os.MkdirAll(filepath.Join(home, component(manifest, name)["path"].(string), ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "README.md"), []byte("capture fixture\n"))
	generator := filepath.Join(home, release.ResidentGenerator)
	writeFile(t, generator, []byte("#!/bin/sh\n# --root --check\n"))
	if err := os.Chmod(generator, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := release.EnvironmentCapturer
	release.EnvironmentCapturer = func(_, _ string, _ release.InstructionRef, previous map[string]any) (map[string]any, error) {
		return previous, nil
	}
	t.Cleanup(func() { release.EnvironmentCapturer = previous })
	runner := func(command procx.Command) procx.Result {
		joined := strings.Join(command.Argv, " ")
		switch {
		case command.Argv[0] == generator:
			return procx.Result{}
		case strings.HasSuffix(joined, "status --porcelain"),
			strings.Contains(joined, "fetch --quiet origin "),
			strings.Contains(joined, "merge-base --is-ancestor "):
			return procx.Result{}
		case strings.HasSuffix(joined, "rev-parse HEAD"):
			return procx.Result{Stdout: strings.Repeat("a", 40)}
		case strings.HasSuffix(joined, "t3-steward version"):
			return procx.Result{Stdout: "t3-steward v0.10.1\ntested with T3 1.2.0..1.4.0\n"}
		case strings.HasPrefix(joined, "npm ls "):
			return procx.Result{Stdout: `{"dependencies":{"t3":{"version":"1.3.0","resolved":"https://registry.npmjs.org/t3/-/t3-1.3.0.tgz"}}}`}
		case joined == "npm prefix -g":
			return procx.Result{Code: 1}
		default:
			t.Errorf("unexpected command: %v", command.Argv)
			return procx.Result{Code: 1}
		}
	}
	return root, home, manifest, runner
}

func TestCaptureJocastaNonconsumerPreservesPublishedPin(t *testing.T) {
	for _, controller := range []string{"laptop", "omarchy-pc", "agent-a", "agent-b", "normandy-old"} {
		t.Run(controller, func(t *testing.T) {
			root, home, manifest, runner := jocastaCaptureFixture(t)
			// No CLI, config or credential exists in this home. Any CLI or gh
			// command fails the test through the fixture's unexpected-command case.
			if _, err := release.Write(root, release.Release{Manifest: manifest}); err != nil {
				t.Fatal(err)
			}
			got, err := release.Capture(root, controller, home, runner, time.Time{}, nil, release.ResidentPolicy{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Component(release.JocastaComponent), component(manifest, release.JocastaComponent)) {
				t.Fatal("default capture changed the nonconsumer's published Jocasta component")
			}
			if got.Manifest["captured_from"] != controller {
				t.Fatal("capture did not retain the controller identity")
			}
		})
	}
}

func TestCaptureJocastaExplicitNonconsumerRefuses(t *testing.T) {
	root, home, manifest, runner := jocastaCaptureFixture(t)
	if _, err := release.Write(root, release.Release{Manifest: manifest}); err != nil {
		t.Fatal(err)
	}
	_, err := release.Capture(root, "laptop", home, runner, time.Time{}, []string{release.JocastaComponent}, release.ResidentPolicy{})
	if err == nil || !strings.Contains(err.Error(), "not an intended host") {
		t.Fatalf("explicit nonconsumer capture: %v", err)
	}
}

func TestCaptureJocastaInvalidNonconsumerSpecRefuses(t *testing.T) {
	for _, only := range [][]string{nil, {release.JocastaComponent}} {
		t.Run(fmt.Sprint(only), func(t *testing.T) {
			root, home, manifest, runner := jocastaCaptureFixture(t)
			component(manifest, release.JocastaComponent)["hosts"] = map[string]any{"normandy": "secretref:jocasta/laptop"}
			if _, err := release.Write(root, release.Release{Manifest: manifest}); err != nil {
				t.Fatal(err)
			}
			_, err := release.Capture(root, "laptop", home, runner, time.Time{}, only, release.ResidentPolicy{})
			if err == nil || !strings.Contains(err.Error(), "own target-local credential") {
				t.Fatalf("invalid nonconsumer spec: %v", err)
			}
		})
	}
}

func TestCaptureJocastaConsumerRequiresPublishedCLI(t *testing.T) {
	for _, controller := range []string{"homelab", "normandy"} {
		for _, state := range []string{"missing", "mismatched", "matching"} {
			t.Run(controller+"/"+state, func(t *testing.T) {
				root, home, manifest, baseRunner := jocastaCaptureFixture(t)
				if _, err := release.Write(root, release.Release{Manifest: manifest}); err != nil {
					t.Fatal(err)
				}
				binary := filepath.Join(home, ".local/bin/jocasta")
				// SHA-256 of the empty file, used as the fake published artifact.
				const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
				if state != "missing" {
					payload := []byte{}
					if state == "mismatched" {
						payload = []byte("unpublished bytes")
					}
					writeFile(t, binary, payload)
				}
				cliCalls, ghCalls := 0, 0
				runner := func(command procx.Command) procx.Result {
					switch command.Argv[0] {
					case binary:
						cliCalls++
						if state == "missing" {
							return procx.Result{Code: 1, Err: os.ErrNotExist}
						}
						return procx.Result{Stdout: `{"version":"0.1.7","commit":"cccccccccccccccccccccccccccccccccccccccc"}`}
					case "gh":
						ghCalls++
						stage := command.Argv[len(command.Argv)-1]
						writeFile(t, filepath.Join(stage, "checksums.txt"), []byte(
							digest+"  jocasta_0.1.7_linux_amd64\n"+digest+"  jocasta_0.1.7_linux_arm64\n"))
						return procx.Result{}
					default:
						return baseRunner(command)
					}
				}
				got, err := release.Capture(root, controller, home, runner, time.Time{}, []string{release.JocastaComponent}, release.ResidentPolicy{})
				wantError := map[string]string{"missing": "cannot prove its version", "mismatched": "differs from published artifact"}[state]
				if state != "matching" {
					if err == nil || !strings.Contains(err.Error(), wantError) {
						t.Fatalf("%s CLI: %v", state, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					spec, err := release.DecodeJocasta(got.Component(release.JocastaComponent))
					if err != nil {
						t.Fatal(err)
					}
					if spec.Version != "0.1.7" || !reflect.DeepEqual(spec.Hosts, map[string]string{"homelab": "secretref:jocasta/homelab", "normandy": "secretref:jocasta/normandy"}) {
						t.Fatalf("consumer capture changed intended hosts or failed to capture: %#v", spec)
					}
				}
				if cliCalls != 1 || (state == "missing" && ghCalls != 0) || (state != "missing" && ghCalls != 1) {
					t.Fatalf("CLI calls %d, gh calls %d for %s", cliCalls, ghCalls, state)
				}
			})
		}
	}
}
