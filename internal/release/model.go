// Package release is the release manifest: what `upkeeper push` captures and
// what `upkeeper pull` applies.
//
// A release is the exact state of the controller's own machine, taken from what
// is running there rather than typed by hand: the Huyang checkout commit, the
// omarchy-setup checkout commit, the installed t3-steward release, the T3
// package it is compatible with, and the agent instruction files that no other
// mechanism distributes. Every value is validated on the way in, so nothing in
// a manifest can later become a shell fragment on a target.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// SchemaVersion is the only release schema this binary understands.
const SchemaVersion = 1

// File and AgentConfigDir are the repository-relative locations of the
// published manifest and of the companion files it names.
const (
	File           = "desired/release.json"
	AgentConfigDir = "desired/agent-config"
)

// HomeRoots are the home subtrees a release may touch. It is wider than the
// components manifest's set because the harness instruction files live in
// ~/.claude and ~/.codex.
var HomeRoots = map[string]bool{
	".config": true, ".local": true, ".claude": true, ".codex": true,
}

var (
	refPattern              = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	versionPattern          = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.]+)?$`)
	localBuildPattern       = regexp.MustCompile(`-[0-9]+-g[0-9a-f]{7,}|dirty`)
	sha256Pattern           = regexp.MustCompile(`^[0-9a-f]{64}$`)
	modulePattern           = regexp.MustCompile(`^[0-9]{2}-[a-z][a-z0-9-]*$`)
	githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	storePattern            = regexp.MustCompile(
		`^[A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)
	numericVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	unitNamePattern       = regexp.MustCompile(`^[a-z][a-z0-9-]*\.service$`)
	buildWordPattern      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// LegacyComponentOrder is the component set of releases captured before the
// T3 pair, the agent environment and the worker configuration existed. Those
// releases stay loadable so that a rollback to them still works.
var LegacyComponentOrder = []string{"omarchy-setup", "agent-config", "huyang", "t3-steward"}

// ComponentOrder is both the set of components a current release carries and
// the order in which they are converged. The order is a dependency order, not
// an alphabetical one.
var ComponentOrder = []string{
	"omarchy-setup",
	"agent-config",
	ToolingComponent,
	JocastaComponent,
	"agent-environment",
	"huyang",
	"t3-steward",
	"t3",
	"worker-configuration",
	fleet.Component,
}

// OptionalComponents may be absent from a current release. The Steward fleet
// intent is optional because every release captured before it existed has to
// keep loading, converging and rolling back exactly as it does now: adding a
// component may not invalidate the releases already published.
var OptionalComponents = map[string]bool{fleet.Component: true, ToolingComponent: true, JocastaComponent: true}

// defaultManifest is the shape of a fresh manifest. Everything push captures is
// null here, so that a manifest which was never captured cannot be mistaken for
// one that was.
const defaultManifest = `{
  "schema_version": 1,
  "captured_at": null,
  "captured_from": null,
  "components": {
    "omarchy-setup": {
      "kind": "git-checkout",
      "source": "https://github.com/iryzhkov/omarchy-setup.git",
      "ref": "main",
      "commit": null,
      "path": ".local/share/omarchy-setup",
      "modules": ["28-scripts", "45-agents"]
    },
    "agent-config": {
      "kind": "files",
      "files": [{"path": ".codex/AGENTS.md", "store": "codex/AGENTS.md", "sha256": null}]
    },
    "huyang": {
      "kind": "git-checkout",
      "source": "https://github.com/iryzhkov/huyang.git",
      "ref": "main",
      "commit": null,
      "path": ".local/share/huyang",
      "build": ["make", "build"],
      "service": "huyang.service",
      "probe": {"kind": "huyang-mcp", "tools": 19}
    },
    "t3-steward": {
      "kind": "github-release",
      "repository": "iryzhkov/t3-steward",
      "version": null,
      "path": ".local/bin/t3-steward",
      "service": "t3-steward.service",
      "installer": "t3-update"
    }
  }
}`

// DefaultManifest returns a fresh copy of the empty manifest shape.
func DefaultManifest() (map[string]any, error) {
	return canonjson.DecodeObject([]byte(defaultManifest))
}

// Release is a validated manifest.
type Release struct {
	Manifest map[string]any
}

// Components returns the component map of the manifest.
func (r Release) Components() map[string]any {
	components, _ := r.Manifest["components"].(map[string]any)
	return components
}

// Component returns one component entry, or nil when the release omits it.
func (r Release) Component(name string) map[string]any {
	component, _ := r.Components()[name].(map[string]any)
	return component
}

// CanonicalBytes are the published bytes of the manifest. Their digest is the
// release identity, so this encoding may never change.
func (r Release) CanonicalBytes() ([]byte, error) {
	return canonjson.MarshalFile(r.Manifest)
}

// SHA256 is the published release identity.
func (r Release) SHA256() (string, error) {
	bytes, err := r.CanonicalBytes()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(bytes)
	return hex.EncodeToString(digest[:]), nil
}

// Digest is SHA256 without the error, for report fields that cannot fail
// meaningfully because the manifest was already validated.
func (r Release) Digest() string {
	value, err := r.SHA256()
	if err != nil {
		return ""
	}
	return value
}

// Files returns the companion file entries of the agent-config component.
func (r Release) Files() []map[string]any {
	component := r.Component("agent-config")
	raw, _ := component["files"].([]any)
	entries := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if entry, ok := item.(map[string]any); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// Describe renders the human summary a capture commit message carries.
func Describe(value Release) []string {
	components := value.Components()
	setup, _ := components["omarchy-setup"].(map[string]any)
	huyang, _ := components["huyang"].(map[string]any)
	steward, _ := components["t3-steward"].(map[string]any)

	descriptions := make([]string, 0, len(value.Files()))
	for _, entry := range value.Files() {
		path, _ := entry["path"].(string)
		digest, _ := entry["sha256"].(string)
		descriptions = append(descriptions, path+"@"+shorten(digest, 12))
	}
	lines := []string{
		"omarchy-setup " + shorten(text(setup["commit"]), 12) + " (" + text(setup["ref"]) + ")",
		"agent-config " + strings.Join(descriptions, ", "),
		"huyang " + shorten(text(huyang["commit"]), 12) + " (" + text(huyang["ref"]) + ")",
		"t3-steward " + text(steward["version"]),
	}
	t3, present := components["t3"].(map[string]any)
	if !present {
		return lines
	}
	environment, _ := components["agent-environment"].(map[string]any)
	environmentFiles, _ := environment["files"].([]any)
	workers, _ := components["worker-configuration"].(map[string]any)
	workerHosts, _ := workers["hosts"].(map[string]any)
	lines = append(lines,
		"t3 "+text(t3["version"]),
		"agent-environment "+itoa(len(environmentFiles))+" owned files",
		"worker-configuration "+itoa(len(workerHosts))+" hosts",
	)
	// The fleet intent is described only when the release carries it, so a
	// release captured before it existed keeps the summary it always had.
	intent, present := components[fleet.Component].(map[string]any)
	if !present {
		return lines
	}
	fleetHosts, _ := intent["hosts"].(map[string]any)
	fleetProjects, _ := intent["projects"].(map[string]any)
	return append(lines, fleet.Component+" "+itoa(len(fleetHosts))+" hosts, "+
		itoa(len(fleetProjects))+" projects")
}

func shorten(value string, size int) string {
	if len(value) <= size {
		return value
	}
	return value[:size]
}

func text(value any) string {
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
