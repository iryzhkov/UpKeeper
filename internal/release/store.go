package release

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// historicalRevisionPattern is the shape a revision must have before it is
// handed to git: no option-like prefix, no whitespace, no control characters.
// Which revisions exist is git's question, not a regular expression's.
var historicalRevisionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/@{}^~-]*$`)

// Load reads and validates the published manifest in a checkout, and verifies
// that every companion file it names still matches the digest it was published
// with. A manifest whose companion files drifted is not a release.
func Load(root string) (Release, error) {
	value, _, err := LoadWithBytes(root)
	return value, err
}

// LoadWithBytes validates the same manifest snapshot whose original bytes it
// returns. Authored publication uses these bytes to fence staging without
// imposing canonical serialization or reading the manifest a second time.
func LoadWithBytes(root string) (Release, []byte, error) {
	path := filepath.Join(root, File)
	data, err := os.ReadFile(path)
	if err != nil {
		return Release{}, nil, failure.Validation("cannot load %s: %v", File, err)
	}
	document, err := canonjson.Decode(data)
	if err != nil {
		return Release{}, nil, failure.Validation("cannot load %s: %v", File, err)
	}
	manifest, ok := document.(map[string]any)
	if !ok {
		return Release{}, nil, failure.Validation("%s must contain an object", File)
	}
	validated, err := Validate(manifest)
	if err != nil {
		return Release{}, nil, err
	}
	for _, entry := range validated.Files() {
		store, _ := entry["store"].(string)
		expected, _ := entry["sha256"].(string)
		payload, err := os.ReadFile(filepath.Join(root, AgentConfigDir, store))
		if err != nil {
			return Release{}, nil, failure.Validation("stored agent file missing: %s", store)
		}
		if digestOf(payload) != expected {
			return Release{}, nil, failure.Validation(
				"stored agent file does not match its manifest digest: %s", store)
		}
	}
	return validated, data, nil
}

// Write publishes the manifest's canonical bytes into a checkout.
func Write(root string, value Release) (string, error) {
	path := filepath.Join(root, File)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	payload, err := value.CanonicalBytes()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// StoredFiles reads the companion files a release names out of a checkout.
func StoredFiles(root string, value Release) (map[string]string, error) {
	stored := map[string]string{}
	for _, entry := range value.Files() {
		store, _ := entry["store"].(string)
		payload, err := os.ReadFile(filepath.Join(root, AgentConfigDir, store))
		if err != nil {
			return nil, failure.Validation("stored agent file missing: %s", store)
		}
		stored[store] = string(payload)
	}
	return stored, nil
}

// ExistingOrDefault returns the published manifest of a checkout, or the empty
// shape when nothing has been published yet.
func ExistingOrDefault(root string) (map[string]any, error) {
	if _, err := os.Stat(filepath.Join(root, File)); err == nil {
		value, err := Load(root)
		if err != nil {
			return nil, err
		}
		return value.Manifest, nil
	}
	return DefaultManifest()
}

// LoadHistorical reads immutable release intent, and the companion files that
// go with it, straight out of Git history.
//
// Nothing about the checkout moves: a rollback must be able to read an older
// release without disturbing whatever the working tree currently holds.
//
// revision is anything git resolves to a commit: a hash, a tag, a branch or a
// symbolic ref such as HEAD. flag names the option the operator typed it
// after ("--to", "--previous"), so a refusal names that option; an empty flag
// means the revision was the checkout's own and the message says so.
func LoadHistorical(
	root, revision, flag string, runner procx.Runner,
) (Release, string, map[string]string, error) {
	if runner == nil {
		runner = procx.Run
	}
	label := flag
	if label == "" {
		label = "the checkout revision"
	}
	if !historicalRevisionPattern.MatchString(revision) {
		return Release{}, "", nil, failure.Validation(
			"%s must name a commit: a hash, tag, branch or symbolic ref such as HEAD", label)
	}
	unavailable := failure.Validation("historical release or companion file is unavailable")
	git := func(arguments ...string) (string, error) {
		result := runner(procx.Command{
			Argv:    append([]string{"git", "-C", root}, arguments...),
			Timeout: 30 * time.Second,
		})
		if result.Err != nil || result.Code != 0 {
			return "", unavailable
		}
		return result.Stdout, nil
	}
	commitOutput, err := git("rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return Release{}, "", nil, failure.Validation(
			"%s %s does not resolve to a commit in this checkout", label, revision)
	}
	commit := strings.TrimSpace(commitOutput)
	if _, err := git("merge-base", "--is-ancestor", commit, "HEAD"); err != nil {
		return Release{}, "", nil, failure.Validation(
			"%s %s (%s) is not in this checkout's history", label, revision, commit)
	}
	manifestBytes, err := git("show", commit+":"+File)
	if err != nil {
		return Release{}, "", nil, err
	}
	document, err := canonjson.Decode([]byte(manifestBytes))
	if err != nil {
		return Release{}, "", nil, failure.Validation("historical release is not valid JSON")
	}
	manifest, ok := document.(map[string]any)
	if !ok {
		return Release{}, "", nil, failure.Validation("historical release must be an object")
	}
	value, err := Validate(manifest)
	if err != nil {
		return Release{}, "", nil, err
	}
	stored := map[string]string{}
	for _, entry := range value.Files() {
		store, _ := entry["store"].(string)
		expected, _ := entry["sha256"].(string)
		payload, err := git("show", commit+":"+AgentConfigDir+"/"+store)
		if err != nil {
			return Release{}, "", nil, err
		}
		if digestOf([]byte(payload)) != expected {
			return Release{}, "", nil, failure.Validation(
				"historical companion file digest mismatch: %s", store)
		}
		if !utf8.ValidString(payload) {
			return Release{}, "", nil, failure.Validation(
				"historical companion file is not UTF-8")
		}
		stored[store] = payload
	}
	return value, commit, stored, nil
}

func digestOf(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
