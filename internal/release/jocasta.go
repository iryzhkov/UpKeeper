package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

const JocastaComponent = "jocasta"

type Jocasta struct {
	Kind       string            `json:"kind"`
	Repository string            `json:"repository"`
	Version    string            `json:"version"`
	Commit     string            `json:"commit"`
	URL        string            `json:"url"`
	Assets     map[string]string `json:"assets"`
	Hosts      map[string]string `json:"hosts"`
}
type JocastaVersion struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func DecodeJocasta(raw map[string]any) (*Jocasta, error) {
	if !hasExactKeys(raw, "kind", "repository", "version", "commit", "url", "assets", "hosts") {
		return nil, errors.New("unknown Jocasta component fields")
	}
	b, err := canonjson.Compact(raw)
	if err != nil {
		return nil, err
	}
	var spec Jocasta
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&spec); err != nil {
		return nil, err
	}
	if spec.Kind != "github-release" || spec.Repository != "iryzhkov/jocasta" {
		return nil, errors.New("invalid Jocasta release source")
	}
	if _, err = ValidateVersion(spec.Version, "jocasta"); err != nil {
		return nil, err
	}
	if _, err = safety.ValidateGitCommit(spec.Commit); err != nil {
		return nil, err
	}
	u, err := url.Parse(spec.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Jocasta URL must be an HTTPS origin without credentials")
	}
	if len(spec.Assets) != 2 {
		return nil, errors.New("Jocasta requires pinned linux amd64 and arm64 digests")
	}
	for _, p := range []string{"linux_amd64", "linux_arm64"} {
		if !sha256Pattern.MatchString(spec.Assets[p]) {
			return nil, errors.New("invalid Jocasta artifact digest")
		}
	}
	if len(spec.Hosts) == 0 {
		return nil, errors.New("Jocasta needs explicit host credential references")
	}
	for host, ref := range spec.Hosts {
		if _, err = safety.ValidateHostName(host, nil); err != nil {
			return nil, err
		}
		if ref != "secretref:jocasta/"+host {
			return nil, errors.New("Jocasta host must reference its own target-local credential")
		}
	}
	return &spec, nil
}
func JocastaAsset(version, platform string) string { return "jocasta_" + version + "_" + platform }
func CaptureJocasta(home string, runner procx.Runner, raw map[string]any) (map[string]any, error) {
	spec, err := DecodeJocasta(raw)
	if err != nil {
		return nil, err
	}
	result := runner(procx.Command{Argv: []string{filepath.Join(home, ".local/bin/jocasta"), "version", "--json"}, Timeout: 15 * time.Second})
	var version JocastaVersion
	if !result.OK() || json.Unmarshal([]byte(result.Stdout), &version) != nil {
		return nil, errors.New("installed Jocasta cannot prove its version")
	}
	if _, err = ValidateVersion(version.Version, "jocasta"); err != nil {
		return nil, err
	}
	if _, err = safety.ValidateGitCommit(version.Commit); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp("", "upkeeper-jocasta-capture-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	result = runner(procx.Command{Argv: []string{"gh", "release", "download", "v" + version.Version, "--repo", spec.Repository, "--pattern", "checksums.txt", "--dir", stage}, Timeout: 90 * time.Second})
	if !result.OK() {
		return nil, errors.New("cannot fetch published Jocasta checksums")
	}
	checksums, err := os.ReadFile(filepath.Join(stage, "checksums.txt"))
	if err != nil {
		return nil, err
	}
	assets := map[string]string{}
	for _, platform := range []string{"linux_amd64", "linux_arm64"} {
		name := JocastaAsset(version.Version, platform)
		for _, line := range strings.Split(string(checksums), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
				if assets[platform] != "" {
					return nil, errors.New("duplicate Jocasta checksum")
				}
				assets[platform] = f[0]
			}
		}
	}
	installed, err := os.ReadFile(filepath.Join(home, ".local/bin/jocasta"))
	if err != nil {
		return nil, err
	}
	if digestOf(installed) != assets[runtime.GOOS+"_"+runtime.GOARCH] {
		return nil, errors.New("installed Jocasta differs from published artifact")
	}
	spec.Version = version.Version
	spec.Commit = version.Commit
	spec.Assets = assets
	encoded, err := canonjson.Compact(spec)
	if err != nil {
		return nil, err
	}
	out, err := canonjson.DecodeObject(encoded)
	if err != nil {
		return nil, err
	}
	if _, err = DecodeJocasta(out); err != nil {
		return nil, fmt.Errorf("captured Jocasta: %w", err)
	}
	return out, nil
}
