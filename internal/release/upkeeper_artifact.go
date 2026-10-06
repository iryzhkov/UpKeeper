package release

import (
	"encoding/json"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// ProgramComponent is the optional component that pins UpKeeper's own released
// binary: the program a host must run to apply this release.
//
// It is deliberately not in ComponentOrder. The agent does not converge it; a
// self-pull applies it before any component is dispatched, because the program
// that applies a release has to be the program that was gated with it.
const ProgramComponent = "upkeeper"

// ProgramRepository is the only repository a program pin may name.
const ProgramRepository = "iryzhkov/UpKeeper"

// ProgramAssetLimit bounds a pinned binary. The released binaries are a few
// megabytes; the bound only stops a pin from asking a host to download a disk.
const ProgramAssetLimit int64 = 256 << 20

// ProgramPaths are the repository paths a change to which changes the program
// itself, as opposed to the release intent the program applies. go.sum is
// included because a dependency-only change rebuilds a different binary.
var ProgramPaths = []string{"go.mod", "go.sum", "cmd", "internal"}

// ProgramTargets maps a Go architecture to the asset label a pin carries.
var ProgramTargets = map[string]string{"amd64": "linux-amd64", "arm64": "linux-arm64"}

var (
	programTagPattern       = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.]+)?$`)
	programToolchainPattern = regexp.MustCompile(`^go[0-9]+\.[0-9]+(?:\.[0-9]+)?$`)
	programCommitPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ProgramAsset is one architecture's released binary.
type ProgramAsset struct {
	Asset  string
	SHA256 string
	Size   int64
}

// ProgramPin is a validated upkeeper component.
type ProgramPin struct {
	Repository   string
	Tag          string
	SourceCommit string
	Toolchain    string
	Assets       map[string]ProgramAsset
}

// ProgramTarget is this machine's asset label, or "" when no binary is
// released for it.
func ProgramTarget() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	return ProgramTargets[runtime.GOARCH]
}

// VersionLine is exactly what the pinned binary's `version` prints.
func (p *ProgramPin) VersionLine() string {
	return "upkeeper " + p.Tag + " (commit " + p.SourceCommit + ")"
}

// URL is where a target's asset is published.
func (p *ProgramPin) URL(base, target string) string {
	if base == "" {
		base = "https://github.com"
	}
	return strings.TrimRight(base, "/") + "/" + p.Repository + "/releases/download/" +
		p.Tag + "/" + p.Assets[target].Asset
}

// DecodeProgramPin validates an upkeeper component. Every field is exact: the
// pin is what authorizes a host to replace its own program, so nothing in it
// may be guessed, defaulted or carried along unchecked.
func DecodeProgramPin(component map[string]any) (*ProgramPin, error) {
	if !hasExactKeys(component, "kind", "repository", "tag", "source_commit", "toolchain", "assets") {
		return nil, failure.Validation(
			"upkeeper must name exactly kind, repository, tag, source_commit, toolchain and assets")
	}
	if component["kind"] != "upkeeper-binary" {
		return nil, failure.Validation("upkeeper.kind must be upkeeper-binary")
	}
	if component["repository"] != ProgramRepository {
		return nil, failure.Validation("upkeeper.repository must be %s", ProgramRepository)
	}
	pin := &ProgramPin{
		Repository:   ProgramRepository,
		Tag:          scalarString(component["tag"]),
		SourceCommit: scalarString(component["source_commit"]),
		Toolchain:    scalarString(component["toolchain"]),
		Assets:       map[string]ProgramAsset{},
	}
	if _, ok := component["tag"].(string); !ok || !programTagPattern.MatchString(pin.Tag) {
		return nil, failure.Validation("upkeeper.tag is not a release tag")
	}
	if _, ok := component["source_commit"].(string); !ok ||
		!programCommitPattern.MatchString(pin.SourceCommit) {
		return nil, failure.Validation("upkeeper.source_commit must be a full lowercase commit")
	}
	if _, ok := component["toolchain"].(string); !ok ||
		!programToolchainPattern.MatchString(pin.Toolchain) {
		return nil, failure.Validation("upkeeper.toolchain must name a Go release such as go1.24.13")
	}
	assets, ok := component["assets"].(map[string]any)
	if !ok || !hasExactKeys(assets, "linux-amd64", "linux-arm64") {
		return nil, failure.Validation("upkeeper.assets must name exactly linux-amd64 and linux-arm64")
	}
	for _, target := range []string{"linux-amd64", "linux-arm64"} {
		entry, ok := assets[target].(map[string]any)
		if !ok || !hasExactKeys(entry, "asset", "sha256", "size") {
			return nil, failure.Validation("upkeeper.assets.%s must name asset, sha256 and size", target)
		}
		if entry["asset"] != "upkeeper-"+target {
			return nil, failure.Validation("upkeeper.assets.%s.asset must be upkeeper-%s", target, target)
		}
		digest, _ := entry["sha256"].(string)
		if !sha256Pattern.MatchString(digest) {
			return nil, failure.Validation("upkeeper.assets.%s.sha256 is invalid", target)
		}
		size, ok := programSize(entry["size"])
		if !ok {
			return nil, failure.Validation(
				"upkeeper.assets.%s.size must be a positive integer of at most %d bytes",
				target, ProgramAssetLimit)
		}
		pin.Assets[target] = ProgramAsset{Asset: "upkeeper-" + target, SHA256: digest, Size: size}
	}
	return pin, nil
}

func programSize(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok || strings.ContainsAny(number.String(), ".eE+-") {
		return 0, false
	}
	parsed, err := number.Int64()
	if err != nil || parsed <= 0 || parsed > ProgramAssetLimit {
		return 0, false
	}
	return parsed, true
}

// ProgramPin returns the release's program pin, or nil when the release was
// published before UpKeeper pinned its own binary.
func (r Release) ProgramPin() (*ProgramPin, error) {
	component := r.Component(ProgramComponent)
	if component == nil {
		return nil, nil
	}
	return DecodeProgramPin(component)
}

// CheckProgramPin proves that the pinned binary was built from the program a
// commit holds: no path in ProgramPaths may differ between the pin's source
// commit and the commit under test. A release whose pin names another program
// is stale: hosts refuse it rather than install it. Code-repository CI does
// not run this, because a commit that changes the program can only be pinned
// by a later commit; it is for the configuration repository's CI (M2b) and
// for authoring a pin.
func CheckProgramPin(root, sourceCommit, commit string, runner procx.Runner) error {
	if runner == nil {
		runner = procx.Run
	}
	if !programCommitPattern.MatchString(sourceCommit) {
		return failure.Validation("upkeeper.source_commit must be a full lowercase commit")
	}
	git := func(arguments ...string) procx.Result {
		return runner(procx.Command{
			Argv: append([]string{"git", "-C", root}, arguments...), Timeout: 60 * time.Second})
	}
	if result := git("cat-file", "-e", sourceCommit+"^{commit}"); result.Err != nil || result.Code != 0 {
		return failure.Validation(
			"upkeeper.source_commit %s is not in this repository's history", sourceCommit)
	}
	diff := git(append([]string{"diff", "--quiet", sourceCommit, commit, "--"}, ProgramPaths...)...)
	if diff.Err != nil || (diff.Code != 0 && diff.Code != 1) {
		return failure.Validation("cannot compare the pinned program with %s", commit)
	}
	if diff.Code != 0 {
		return failure.Validation(
			"the upkeeper pin was built from %s, whose program (%s) differs from %s; "+
				"pin the binary released from this program",
			sourceCommit, strings.Join(ProgramPaths, ", "), commit)
	}
	return nil
}
