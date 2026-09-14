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

var historicalRevisionPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// Load reads and validates the published manifest in a checkout, and verifies
// that every companion file it names still matches the digest it was published
// with. A manifest whose companion files drifted is not a release.
func Load(root string) (Release, error) {
	path := filepath.Join(root, File)
	data, err := os.ReadFile(path)
	if err != nil {
		return Release{}, failure.Validation("cannot load %s: %v", File, err)
	}
	document, err := canonjson.Decode(data)
	if err != nil {
		return Release{}, failure.Validation("cannot load %s: %v", File, err)
	}
	manifest, ok := document.(map[string]any)
	if !ok {
		return Release{}, failure.Validation("%s must contain an object", File)
	}
	validated, err := Validate(manifest)
	if err != nil {
		return Release{}, err
	}
	for _, entry := range validated.Files() {
		store, _ := entry["store"].(string)
		expected, _ := entry["sha256"].(string)
		payload, err := os.ReadFile(filepath.Join(root, AgentConfigDir, store))
		if err != nil {
			return Release{}, failure.Validation("stored agent file missing: %s", store)
		}
		if digestOf(payload) != expected {
			return Release{}, failure.Validation(
				"stored agent file does not match its manifest digest: %s", store)
		}
	}
	return validated, nil
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
func LoadHistorical(
	root, revision string, runner procx.Runner,
) (Release, string, map[string]string, error) {
	if runner == nil {
		runner = procx.Run
	}
	if !historicalRevisionPattern.MatchString(revision) {
		return Release{}, "", nil, failure.Validation(
			"--to must be a release commit hash (7 to 40 hex characters)")
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
	commitOutput, err := git("rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return Release{}, "", nil, err
	}
	commit := strings.TrimSpace(commitOutput)
	if _, err := git("merge-base", "--is-ancestor", commit, "HEAD"); err != nil {
		return Release{}, "", nil, err
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
