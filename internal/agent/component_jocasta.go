package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const jocastaBinary = ".local/bin/jocasta"
const jocastaConfig = ".config/jocasta/config.json"

func init() {
	Register(release.JocastaComponent, Handler{Observe: jocastaObserve, Apply: jocastaApply})
}

// Recheck after external staging and immediately before each mutation. The
// target hierarchy may have changed since Observe or while gh was running.
func jocastaSafeTarget(c *Context, path string) error {
	relative, err := filepath.Rel(c.Home, path)
	if err != nil {
		return err
	}
	if _, err = agentenv.TargetPath(c.Home, relative); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("refusing non-regular Jocasta destination")
	}
	return nil
}

func jocastaProjection(c *Context, s *release.Jocasta) ([]byte, error) {
	ref, ok := s.Hosts[c.Host.Name]
	if !ok {
		return nil, nil
	}
	root := c.Path(workerSecretRoot)
	secret := filepath.Join(root, strings.TrimPrefix(ref, "secretref:"))
	if err := workerPathIsContained(root, secret); err != nil {
		return nil, err
	}
	info, err := os.Stat(secret)
	if err != nil {
		return nil, errors.New("Jocasta host credential has not been provisioned")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() == 0 || !ok || int(owner.Uid) != os.Getuid() {
		return nil, errors.New("Jocasta credential requires a nonempty owner-owned 0600 regular file")
	}
	return canonjson.MarshalFile(map[string]any{"url": s.URL, "token_file": secret})
}
func jocastaProve(c *Context, path string, s *release.Jocasta) error {
	result := c.Run([]string{path, "version", "--json"}, 15*time.Second, "")
	var v release.JocastaVersion
	if !result.OK || json.Unmarshal([]byte(result.Output), &v) != nil || v.Version != s.Version || v.Commit != s.Commit {
		return errors.New("Jocasta binary does not prove pinned version and source commit")
	}
	return nil
}
func jocastaObserve(c *Context, raw map[string]any) (map[string]any, error) {
	s, err := release.DecodeJocasta(raw)
	if err != nil {
		return nil, err
	}
	if _, ok := s.Hosts[c.Host.Name]; !ok {
		return map[string]any{"converged": true, "configured": false}, nil
	}
	config, err := jocastaProjection(c, s)
	if err != nil {
		return nil, err
	}
	platform := runtime.GOOS + "_" + runtime.GOARCH
	if s.Assets[platform] == "" {
		return nil, errors.New("unsupported Jocasta platform")
	}
	drift := []string{}
	for _, rel := range []string{jocastaBinary, jocastaConfig} {
		if _, err = agentenv.TargetPath(c.Home, rel); err != nil {
			return nil, err
		}
	}
	binary := c.Path(jocastaBinary)
	info, e := os.Lstat(binary)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0755 || c.SHA256File(binary) != s.Assets[platform] {
		drift = append(drift, "binary differs")
	} else if err = jocastaProve(c, binary, s); err != nil {
		drift = append(drift, err.Error())
	}
	installed, e := os.ReadFile(c.Path(jocastaConfig))
	info, ie := os.Lstat(c.Path(jocastaConfig))
	if e != nil || ie != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !bytes.Equal(installed, config) {
		drift = append(drift, "configuration differs")
	}
	return map[string]any{"converged": len(drift) == 0, "configured": true, "drift": drift, "version": s.Version}, nil
}
func jocastaApply(c *Context, raw, before map[string]any, actions *[]string) (string, error) {
	s, err := release.DecodeJocasta(raw)
	if err != nil {
		return "", err
	}
	if before["converged"] == true {
		return "converged", nil
	}
	if c.DryRun {
		*actions = append(*actions, "would install checksum-pinned Jocasta CLI and host credential reference")
		return "updated", nil
	}
	config, err := jocastaProjection(c, s)
	if err != nil {
		return "", err
	}
	if config == nil {
		return "converged", nil
	}
	for _, rel := range []string{jocastaBinary, jocastaConfig} {
		if _, err = agentenv.TargetPath(c.Home, rel); err != nil {
			return "", err
		}
	}
	stage, err := os.MkdirTemp("", "upkeeper-jocasta-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	platform := runtime.GOOS + "_" + runtime.GOARCH
	asset := release.JocastaAsset(s.Version, platform)
	// gh uses this host's existing GitHub authentication. Credentials are never
	// copied into the manifest or echoed into the convergence result.
	result := c.Run([]string{"gh", "release", "download", "v" + s.Version, "--repo", s.Repository, "--pattern", asset, "--dir", stage}, 2*time.Minute, c.Home)
	if !result.OK {
		return "", errors.New("Jocasta artifact download failed; verify host GitHub access")
	}
	candidate := filepath.Join(stage, asset)
	info, err := os.Lstat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 || c.SHA256File(candidate) != s.Assets[platform] {
		return "", errors.New("Jocasta artifact checksum or size mismatch")
	}
	if err = os.Chmod(candidate, 0755); err != nil {
		return "", err
	}
	if err = jocastaProve(c, candidate, s); err != nil {
		return "", err
	}
	binary, err := os.ReadFile(candidate)
	if err != nil {
		return "", err
	}
	type priorFile struct {
		path   string
		data   []byte
		mode   os.FileMode
		exists bool
	}
	prior := []priorFile{}
	backup := c.Path(".local/state/dev-fleet/jocasta", c.RunID)
	if _, err = agentenv.TargetPath(c.Home, filepath.Join(".local/state/dev-fleet/jocasta", c.RunID)); err != nil {
		return "", err
	}
	if err = os.MkdirAll(backup, 0700); err != nil {
		return "", err
	}
	for i, rel := range []string{jocastaBinary, jocastaConfig} {
		p := priorFile{path: c.Path(rel)}
		if err = jocastaSafeTarget(c, p.path); err != nil {
			return "", err
		}
		st, e := os.Lstat(p.path)
		if e == nil {
			if !st.Mode().IsRegular() {
				return "", errors.New("refusing non-regular Jocasta destination")
			}
			p.exists = true
			p.mode = st.Mode().Perm()
			p.data, e = os.ReadFile(p.path)
			if e != nil {
				return "", e
			}
			if e = agentenv.AtomicWrite(filepath.Join(backup, fmt.Sprintf("%d.previous", i)), p.data, 0600); e != nil {
				return "", e
			}
		} else if !os.IsNotExist(e) {
			return "", e
		}
		prior = append(prior, p)
	}
	desired := [][]byte{binary, config}
	modes := []os.FileMode{0755, 0600}
	activated := 0
	rollback := func(cause error) (string, error) {
		for i := activated - 1; i >= 0; i-- {
			p := prior[i]
			if e := jocastaSafeTarget(c, p.path); e != nil {
				return "", Failf("Jocasta rollback target changed; preserve recovery at %s", backup)
			}
			current, e := os.ReadFile(p.path)
			if e != nil || !bytes.Equal(current, desired[i]) {
				return "", Failf("Jocasta activation failed; concurrent state preserved for recovery at %s", backup)
			}
			if p.exists {
				e = agentenv.AtomicWrite(p.path, p.data, p.mode)
			} else {
				e = os.Remove(p.path)
			}
			if e != nil {
				return "", Failf("Jocasta rollback failed; inspect %s", backup)
			}
		}
		return "", RolledBackf("Jocasta activation reverted: %s", cause)
	}
	for i, p := range prior {
		if e := jocastaSafeTarget(c, p.path); e != nil {
			return rollback(e)
		}
		current, e := os.ReadFile(p.path)
		if p.exists {
			if e != nil || !bytes.Equal(current, p.data) {
				return rollback(errors.New("destination changed during staging"))
			}
		} else if !os.IsNotExist(e) {
			return rollback(errors.New("destination appeared during staging"))
		}
		if e = agentenv.AtomicWrite(p.path, desired[i], modes[i]); e != nil {
			return rollback(e)
		}
		activated++
	}
	after, err := jocastaObserve(c, raw)
	if err != nil {
		return rollback(err)
	}
	if after["converged"] != true {
		return rollback(errors.New("installed Jocasta verification failed"))
	}
	*actions = append(*actions, "installed verified Jocasta CLI and private host configuration")
	return "updated", nil
}
