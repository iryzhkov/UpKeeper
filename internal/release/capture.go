package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/pyfmt"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// InstructionRef records where a projected instruction file came from, so that
// a host can always trace an owned file back to a reviewed commit.
type InstructionRef struct {
	Repository string
	Commit     string
	Path       string
	SHA256     string
}

// EnvironmentCapturer reads this machine's owned agent environment. It is
// injected for the same reason the validator is: the manifest model does not
// depend on the projection that applies it.
var EnvironmentCapturer func(home string, provenance InstructionRef) (map[string]any, error)
var EnvironmentToolingNormalizer func(map[string]any) (map[string]any, error)

var stewardRangePattern = regexp.MustCompile(
	`tested with T3 ([0-9]+\.[0-9]+\.[0-9]+)\.\.([0-9]+\.[0-9]+\.[0-9]+)`)

// Capture reads the controller's own machine into a validated release manifest.
//
// Capture reads the whole machine, because the components are not independent:
// the T3 pin is derived from the installed steward, and the agent environment
// is a set of files rather than a version. When only is given, every component
// outside it is restored to exactly what the existing manifest published, so a
// capture asked for one component cannot carry another along.
//
// Without that, a controller whose local checkout of some unrelated component
// is ahead of the release proposes that newer commit on every capture, and the
// operator has to notice and revert it by hand each time.
//
// Before anything is observed, a capture that would publish the resident agent
// instructions proves that this host still holds what their source generates.
// See resident.go for why that refusal is the single enforcement point in the
// arrangement rather than one check among several.
func Capture(
	root, controller, home string, runner procx.Runner, now time.Time, only []string,
	resident ResidentPolicy,
) (Release, error) {
	if runner == nil {
		runner = procx.Run
	}
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return Release{}, failure.Capture("cannot resolve the home directory: %v", err)
		}
		home = resolved
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		return Release{}, failure.Capture("cannot resolve the home directory: %v", err)
	}
	home = absoluteHome

	var selected map[string]bool
	var published map[string]any
	if only != nil {
		selected = map[string]bool{}
		known := map[string]bool{}
		for _, name := range ComponentOrder {
			known[name] = true
		}
		unknown := []string{}
		for _, name := range only {
			selected[name] = true
			if !known[name] {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) > 0 {
			return Release{}, failure.Capture(
				"unknown component(s): %s", strings.Join(sortedUnique(unknown), ", "))
		}
		if selected[ToolingComponent] && !selected["agent-environment"] {
			return Release{}, failure.Capture("capture agent-tooling and agent-environment together so runtime bindings match")
		}
		if err := RefuseAuthoredCapture(only); err != nil {
			return Release{}, err
		}
		existing, err := ExistingOrDefault(root)
		if err != nil {
			return Release{}, err
		}
		components, _ := existing["components"].(map[string]any)
		copied, err := deepCopy(components)
		if err != nil {
			return Release{}, err
		}
		published, _ = copied.(map[string]any)
	}

	status, err := gitOutput(runner, root, "status", "--porcelain")
	if err != nil {
		return Release{}, err
	}
	if status != "" {
		return Release{}, failure.Capture(
			"release repository has uncommitted changes; commit before capture")
	}

	manifest, err := ExistingOrDefault(root)
	if err != nil {
		return Release{}, err
	}
	components, _ := manifest["components"].(map[string]any)
	// The resident agent instructions are gated here, inside Capture, rather
	// than in the one command that happens to call it today. Every future
	// caller of Capture publishes the same photograph of the same home
	// directory, so the refusal belongs where the photograph is taken.
	//
	// A selection that does not name agent-environment is not gated, because
	// the environment it captures below is discarded again at the end and the
	// release keeps what it already published. Gating it would refuse captures
	// that promote nothing, which on a fleet where every host is diverged today
	// would leave no way to publish anything at all.
	if selected == nil || selected["agent-environment"] {
		setup, _ := components["omarchy-setup"].(map[string]any)
		if err := guardResidentInstructions(runner, home, setup, resident); err != nil {
			return Release{}, err
		}
	}
	// Authored fleet intent is taken aside before anything is observed, and
	// put back untouched at the end. Capture may record observations in the
	// document's separate observed field; it may never infer authorization
	// from what this one host happens to have.
	authoredFleet, fleetPresent := components[fleet.Component]

	huyang, _ := components["huyang"].(map[string]any)
	huyang["ref"] = "main"
	for _, name := range []string{"omarchy-setup", "huyang"} {
		component, _ := components[name].(map[string]any)
		if err := captureCheckout(runner, home, name, component); err != nil {
			return Release{}, err
		}
	}
	steward, _ := components["t3-steward"].(map[string]any)
	if err := captureSteward(runner, home, "t3-steward", steward); err != nil {
		return Release{}, err
	}
	if hook, ok := steward["session_hook_sha256"].(string); ok && hook != "" {
		if err := verifySessionHook(home, hook); err != nil {
			return Release{}, err
		}
	}
	t3, err := captureT3(runner, home, steward)
	if err != nil {
		return Release{}, err
	}
	components["t3"] = t3
	steward["installer"] = "upkeeper"

	if EnvironmentCapturer == nil {
		return Release{}, failure.Capture(
			"agent-environment: no environment capturer is wired in")
	}
	headCommit, err := gitOutput(runner, root, "rev-parse", "HEAD")
	if err != nil {
		return Release{}, err
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		return Release{}, failure.Capture("agent-environment: cannot read README.md")
	}
	environment, err := EnvironmentCapturer(home, InstructionRef{
		Repository: "github.com/iryzhkov/UpKeeper",
		Commit:     headCommit,
		Path:       "README.md",
		SHA256:     digestOf(readme),
	})
	if err != nil {
		return Release{}, failure.Capture("agent-environment: %s", err.Error())
	}
	components["agent-environment"] = environment
	if components[JocastaComponent] != nil && (selected == nil || selected[JocastaComponent]) {
		jocasta, err := CaptureJocasta(home, runner, components[JocastaComponent].(map[string]any))
		if err != nil {
			return Release{}, failure.Capture("jocasta: %s", err)
		}
		components[JocastaComponent] = jocasta
	}
	// Tooling is opt-in for historical releases; selecting it bootstraps its
	// first reviewed capture. Once published it remains part of full captures.
	if components[ToolingComponent] != nil || selected[ToolingComponent] {
		tooling, err := CaptureTooling(home, runner)
		if err != nil {
			return Release{}, failure.Capture("agent-tooling: %s", err)
		}
		components[ToolingComponent] = tooling
		if EnvironmentToolingNormalizer == nil {
			return Release{}, failure.Capture("tooling registration normalizer unavailable")
		}
		environment, err = EnvironmentToolingNormalizer(environment)
		if err != nil {
			return Release{}, failure.Capture("tooling registration: %s", err)
		}
		components["agent-environment"] = environment
	}

	agentConfig, _ := components["agent-config"].(map[string]any)
	agentConfig["files"] = []any{}
	setup, _ := components["omarchy-setup"].(map[string]any)
	setup["modules"] = []any{"28-scripts"}
	if _, present := components["worker-configuration"]; !present {
		components["worker-configuration"] = map[string]any{
			"kind": "worker-configuration", "hosts": map[string]any{},
		}
	}

	if selected != nil {
		for _, name := range safety.SortedKeys(components) {
			if selected[name] {
				continue
			}
			if previous, present := published[name]; present {
				components[name] = previous
			} else {
				delete(components, name)
			}
		}
	}
	if err := preserveAuthoredFleetIntent(components, authoredFleet, fleetPresent); err != nil {
		return Release{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	manifest["captured_at"] = pytime.ISO8601Z(now)
	if _, err := safety.ValidateHostName(controller, nil); err != nil {
		return Release{}, err
	}
	manifest["captured_from"] = controller
	return Validate(manifest)
}

// captureCheckout records the commit a checkout is on, and refuses to publish a
// commit the fleet could not fetch later.
func captureCheckout(
	runner procx.Runner, home, name string, component map[string]any,
) error {
	path, _ := component["path"].(string)
	checkout := filepath.Join(home, path)
	if _, err := os.Stat(filepath.Join(checkout, ".git")); err != nil {
		return failure.Capture("%s: %s is not a git checkout", name, checkout)
	}
	status, err := gitOutput(runner, checkout, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return failure.Capture(
			"%s: %s has uncommitted changes; a release must be clean", name, checkout)
	}
	head, err := gitOutput(runner, checkout, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	commit, err := safety.ValidateGitCommit(head)
	if err != nil {
		return err
	}
	refValue, _ := component["ref"].(string)
	ref, err := ValidateRef(refValue, name)
	if err != nil {
		return err
	}
	// Ask the remote itself rather than trusting stale remote-tracking refs: a
	// target can only fetch what origin/<ref> actually reaches today.
	fetched := runner(procx.Command{
		Argv:    []string{"git", "-C", checkout, "fetch", "--quiet", "origin", ref},
		Timeout: 90 * time.Second,
	})
	if fetched.Err != nil || fetched.Code != 0 {
		return failure.Capture(
			"%s: cannot fetch origin/%s: %s", name, ref, tail(strings.TrimSpace(fetched.Stderr), 300))
	}
	reachable := runner(procx.Command{
		Argv:    []string{"git", "-C", checkout, "merge-base", "--is-ancestor", commit, "FETCH_HEAD"},
		Timeout: 30 * time.Second,
	})
	if reachable.Err != nil || reachable.Code != 0 {
		return failure.Capture(
			"%s: commit %s is not on origin/%s; push it there before publishing a release",
			name, shorten(commit, 12), ref)
	}
	component["commit"] = commit
	return nil
}

// captureSteward records the installed steward release, and refuses a build
// that was never published, because no target could reinstall it.
func captureSteward(runner procx.Runner, home, name string, component map[string]any) error {
	path, _ := component["path"].(string)
	binary := filepath.Join(home, path)
	completed := runner(procx.Command{Argv: []string{binary, "version"}, Timeout: 15 * time.Second})
	if completed.Err != nil || completed.Code != 0 {
		return failure.Capture("%s: %s version failed", name, binary)
	}
	words := strings.Fields(completed.Stdout)
	if len(words) < 2 {
		return failure.Capture(
			"%s: cannot parse version output %s", name, pyfmt.Repr(strings.TrimSpace(completed.Stdout)))
	}
	version := strings.TrimPrefix(words[1], "v")
	validated, err := ValidateVersion(version, name)
	if err != nil {
		return failure.Capture(
			"%s: installed build %s is not a published release; tag and release it first",
			name, pyfmt.Repr(words[1]))
	}
	component["version"] = validated
	return nil
}

// captureT3 derives the T3 pin from the steward that is actually installed, so
// the pair can never be captured out of the range the steward was tested with.
func captureT3(runner procx.Runner, home string, steward map[string]any) (map[string]any, error) {
	path, _ := steward["path"].(string)
	version := runner(procx.Command{
		Argv: []string{filepath.Join(home, path), "version"}, Timeout: 15 * time.Second,
	})
	match := stewardRangePattern.FindStringSubmatch(version.Stdout)
	if version.Err != nil || version.Code != 0 || match == nil {
		return nil, failure.Capture("steward does not report its tested T3 range")
	}
	installed := runner(procx.Command{
		Argv:    []string{"npm", "ls", "-g", "--depth=0", "--json", "t3"},
		Timeout: 30 * time.Second,
	})
	document, err := canonjson.DecodeObject([]byte(installed.Stdout))
	if err != nil {
		return nil, failure.Capture("cannot observe installed npm t3 package")
	}
	dependencies, _ := document["dependencies"].(map[string]any)
	entry, _ := dependencies["t3"].(map[string]any)
	packageVersion, ok := entry["version"].(string)
	if !ok {
		return nil, failure.Capture("cannot observe installed npm t3 package")
	}
	minimum, err := versionTuple(match[1])
	if err != nil {
		return nil, err
	}
	maximum, err := versionTuple(match[2])
	if err != nil {
		return nil, err
	}
	current, err := versionTuple(packageVersion)
	if err != nil {
		return nil, err
	}
	if installed.Err != nil || installed.Code != 0 ||
		compareVersions(minimum, current) > 0 || compareVersions(current, maximum) > 0 {
		return nil, failure.Capture("installed T3 is outside the steward tested range")
	}
	return map[string]any{
		"kind":    "npm-package",
		"package": "t3",
		"version": packageVersion,
		"compatibility": map[string]any{
			"min": match[1], "max": match[2], "steward_version": steward["version"],
		},
		"service": "t3code.service",
	}, nil
}

// verifySessionHook refuses to publish a steward pin whose reviewed OpenCode
// caller hook is not the one installed here.
func verifySessionHook(home, expected string) error {
	path := filepath.Join(home, ".config/opencode/plugins/steward-session.js")
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return failure.Capture("installed OpenCode session hook does not match the reviewed pin")
	}
	payload, err := os.ReadFile(path)
	if err != nil || digestOf(payload) != expected {
		return failure.Capture("installed OpenCode session hook does not match the reviewed pin")
	}
	return nil
}

func gitOutput(runner procx.Runner, directory string, arguments ...string) (string, error) {
	result := runner(procx.Command{
		Argv:    append([]string{"git", "-C", directory}, arguments...),
		Timeout: 30 * time.Second,
	})
	if result.Err != nil || result.Code != 0 {
		return "", failure.Capture("git %s failed in %s: %s",
			strings.Join(arguments, " "), directory, strings.TrimSpace(result.Stderr))
	}
	return strings.TrimSpace(result.Stdout), nil
}

func deepCopy(value any) (any, error) {
	encoded, err := canonjson.Compact(value)
	if err != nil {
		return nil, err
	}
	return canonjson.Decode(encoded)
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	for index := 1; index < len(unique); index++ {
		for position := index; position > 0 && unique[position-1] > unique[position]; position-- {
			unique[position-1], unique[position] = unique[position], unique[position-1]
		}
	}
	return unique
}

func tail(value string, size int) string {
	if len(value) <= size {
		return value
	}
	return value[len(value)-size:]
}
