// Package rollout executes the reviewed binary-only maintenance procedure.
package rollout

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// CommandRunner keeps all host, GitHub, git and Steward effects fakeable.
type CommandRunner interface {
	Run(context.Context, string, string, string, ...string) ([]byte, error)
}
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Every UpKeeper helper must consume the fenced checkout, even when the
	// operator shell has an override for its ordinary installed configuration.
	if name == "upkeeper" && dir != "" {
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, "DEV_FLEET_CONFIG_ROOT="+dir)
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	return out, nil
}

type ProductionBackend struct {
	Runner            CommandRunner
	Fence             Fence
	CoordinatorBackup CoordinatorBackup
	Now               func() time.Time
	Sleep             func(context.Context, time.Duration) error
	inspectBinary     func([]byte, string, string) error
}

func NewProductionBackend() *ProductionBackend {
	return &ProductionBackend{Runner: OSRunner{}, Now: time.Now, Sleep: func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}}
}
func (b *ProductionBackend) command(ctx context.Context, c Config, stdin, name string, args ...string) ([]byte, error) {
	return b.Runner.Run(ctx, c.Checkout, stdin, name, args...)
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func (b *ProductionBackend) on(ctx context.Context, c Config, host, script string, args ...string) ([]byte, error) {
	if host == c.LocalHost {
		return b.Runner.Run(ctx, "", script, "bash", append([]string{"-s", "--"}, args...)...)
	}
	target, ok := c.Targets[host]
	if !ok || target == "" {
		return nil, fmt.Errorf("missing SSH target for %s", host)
	}
	line := "bash -s --"
	for _, arg := range args {
		line += " " + quote(arg)
	}
	return b.Runner.Run(ctx, "", script, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", target, line)
}
func keyValues(p []byte) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(string(p), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}
func digest(p []byte) string { x := sha256.Sum256(p); return hex.EncodeToString(x[:]) }
func record(s *State, key string, v any) error {
	if s.Extra == nil {
		s.Extra = map[string]json.RawMessage{}
	}
	p, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.Extra[key] = p
	if s.Save != nil {
		return s.Save()
	}
	return nil
}
func evidence(s *State, key string, v any) error {
	p, ok := s.Extra[key]
	if !ok {
		return fmt.Errorf("missing evidence %s", key)
	}
	return json.Unmarshal(p, v)
}

type identity struct {
	Health, Release string
	Epoch           json.Number
}
type worker struct {
	Requirement struct {
		WorkerID        string `json:"workerId"`
		Draining        bool
		CatalogRevision json.RawMessage
	}
	State, Health   string
	Enrolled, Stale bool
	Enrollment      struct {
		Request struct{ CatalogRevision json.RawMessage }
	}
	Snapshot struct {
		CoordinatorEpoch json.Number
		Inventory        struct {
			Runtime   struct{ Release string }
			Telemetry struct {
				RunningAttempts *int `json:"running_attempts"`
			}
		}
	}
}

func (b *ProductionBackend) identity(ctx context.Context, c Config) (identity, error) {
	p, err := b.command(ctx, c, "", "t3-steward", "coordinator", "identity", "--json")
	var v identity
	if err == nil {
		err = json.Unmarshal(p, &v)
	}
	return v, err
}
func (b *ProductionBackend) workers(ctx context.Context, c Config) ([]worker, error) {
	p, err := b.command(ctx, c, "", "t3-steward", "worker", "list", "--json")
	var v struct{ Workers []worker }
	if err == nil {
		err = json.Unmarshal(p, &v)
	}
	return v.Workers, err
}
func (b *ProductionBackend) green(ctx context.Context, c Config, repo, sha string) error {
	p, err := b.command(ctx, c, "", "gh", "run", "list", "-R", repo, "--commit", sha, "--json", "status,conclusion")
	if err != nil {
		return err
	}
	var runs []struct{ Status, Conclusion string }
	if err = json.Unmarshal(p, &runs); err != nil {
		return err
	}
	if len(runs) == 0 {
		return errors.New("no CI runs")
	}
	for _, r := range runs {
		if r.Status != "completed" || r.Conclusion != "success" {
			return fmt.Errorf("CI for %s is %s/%s", sha, r.Status, r.Conclusion)
		}
	}
	return nil
}
func (b *ProductionBackend) remoteHeads(ctx context.Context, c Config, want string) (string, error) {
	var head string
	for _, remote := range c.Remotes {
		p, err := b.command(ctx, c, "", "git", "ls-remote", remote, "refs/heads/main")
		if err != nil {
			return "", err
		}
		f := strings.Fields(string(p))
		if len(f) != 2 || f[1] != "refs/heads/main" {
			return "", errors.New("remote main missing")
		}
		if head != "" && head != f[0] {
			return "", errors.New("UpKeeper remotes disagree")
		}
		head = f[0]
		if want != "" && head != want {
			return "", errors.New("remote main moved")
		}
	}
	if head == "" {
		return "", errors.New("no release remotes")
	}
	return head, nil
}

type assetEvidence struct {
	Tag, Commit, ChecksumsSHA256 string
	Binaries                     map[string]string
}

// assets validates all release archives in memory: check creates no directories or files.
func (b *ProductionBackend) assets(ctx context.Context, c Config, sha string) (assetEvidence, error) {
	v := assetEvidence{Tag: "v" + c.Version, Commit: sha, Binaries: map[string]string{}}
	p, err := b.command(ctx, c, "", "gh", "api", "repos/"+c.StewardRepo+"/releases/tags/"+v.Tag)
	if err != nil {
		return v, err
	}
	var release struct {
		Assets []struct {
			Name string
			ID   json.Number
		}
	}
	if err = json.Unmarshal(p, &release); err != nil {
		return v, err
	}
	ids := map[string]string{}
	for _, a := range release.Assets {
		ids[a.Name] = a.ID.String()
	}
	download := func(name string) ([]byte, error) {
		id := ids[name]
		if id == "" {
			return nil, fmt.Errorf("missing release asset %s", name)
		}
		return b.command(ctx, c, "", "gh", "api", "repos/"+c.StewardRepo+"/releases/assets/"+id, "-H", "Accept: application/octet-stream")
	}
	sums, err := download("checksums.txt")
	if err != nil {
		return v, err
	}
	v.ChecksumsSHA256 = digest(sums)
	checksums := map[string]string{}
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			checksums[strings.TrimPrefix(f[1], "*")] = f[0]
		}
	}
	for _, platform := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		name := "t3-steward_" + c.Version + "_" + platform + ".tar.gz"
		archive, err := download(name)
		if err != nil {
			return v, err
		}
		if checksums[name] == "" || digest(archive) != checksums[name] {
			return v, fmt.Errorf("%s archive checksum mismatch", platform)
		}
		binary, err := archiveBinary(archive)
		if err != nil {
			return v, err
		}
		inspect := b.inspectBinary
		if inspect == nil {
			inspect = validateBuildInfo
		}
		if err = inspect(binary, v.Tag, sha); err != nil {
			return v, fmt.Errorf("%s build metadata: %w", platform, err)
		}
		v.Binaries[platform] = digest(binary)
	}
	return v, nil
}
func validateBuildInfo(binary []byte, tag, sha string) error {
	info, err := buildinfo.Read(bytes.NewReader(binary))
	if err != nil {
		return err
	}
	if info.Main.Path != "github.com/iryzhkov/t3-steward" || info.Main.Version != tag {
		return errors.New("build module does not name release tag")
	}
	commitProved := false
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			if setting.Value != sha {
				return errors.New("binary tagged commit differs")
			}
			commitProved = true
		}
		if setting.Key == "-ldflags" && len(sha) >= 12 && strings.Contains(setting.Value, sha[:12]) {
			commitProved = true
		}
	}
	if !commitProved {
		return errors.New("binary build metadata does not prove tagged commit")
	}
	return nil
}
func archiveBinary(p []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(p))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var binary []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(h.Name) == "t3-steward" && (h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA) {
			if binary != nil {
				return nil, errors.New("duplicate binary in archive")
			}
			if h.Size > 512<<20 {
				return nil, errors.New("oversized binary archive")
			}
			binary, err = io.ReadAll(io.LimitReader(tr, 512<<20))
			if err != nil {
				return nil, err
			}
		}
	}
	if len(binary) == 0 {
		return nil, errors.New("archive has no binary")
	}
	return binary, nil
}

const timerScript = `systemctl --user show upkeeper-self.timer -p ActiveState -p UnitFileState
printf 'SelfServiceActive=%s\n' "$(systemctl --user is-active upkeeper-self.service || true)"
printf 'ControllerTimer=%s\n' "$(systemctl --user is-active upkeeper-pull.timer || true)"
`

func (b *ProductionBackend) laptop(ctx context.Context, c Config) (bool, error) {
	if c.LaptopHost == "" {
		return false, nil
	}
	p, err := b.on(ctx, c, c.LaptopHost, "systemctl --user is-enabled t3-steward-worker.service 2>&1 || true")
	if err != nil {
		// Only a transport exit 255 proves this optional host is unreachable.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 255 {
			// Authentication, host-key and name-resolution errors do not prove an
			// excluded host is off. Only an explicit connection failure permits skip.
			detail := err.Error()
			if strings.Contains(detail, "Connection refused") || strings.Contains(detail, "Connection timed out") || strings.Contains(detail, "No route to host") {
				return false, nil
			}
		}
		return false, fmt.Errorf("cannot establish laptop safety: %w", err)
	}
	if strings.TrimSpace(string(p)) != "masked" {
		return true, errors.New("reachable laptop worker is not masked")
	}
	return true, nil
}
func (b *ProductionBackend) timerCheck(ctx context.Context, c Config, fenced bool) error {
	reachable, err := b.laptop(ctx, c)
	if err != nil {
		return err
	}
	for _, h := range c.TimerHosts {
		if h == c.LaptopHost && !reachable {
			continue
		}
		p, err := b.on(ctx, c, h, timerScript)
		if err != nil {
			return err
		}
		kv := keyValues(p)
		if strings.HasPrefix(kv["SelfServiceActive"], "activ") {
			return fmt.Errorf("%s self updater running", h)
		}
		if kv["ControllerTimer"] == "active" {
			return fmt.Errorf("%s has controller pull timer", h)
		}
		if kv["ActiveState"] == "" {
			return fmt.Errorf("%s timer state unavailable", h)
		}
		if fenced && kv["ActiveState"] != "inactive" {
			return fmt.Errorf("%s timer rearmed", h)
		}
	}
	return nil
}
func drains(ws []worker, c Config) error {
	for _, h := range c.DrainedHosts {
		found := false
		for _, w := range ws {
			if w.Requirement.WorkerID == h {
				found = true
				if w.State != "draining" {
					return fmt.Errorf("%s drain changed", h)
				}
			}
		}
		if !found {
			return fmt.Errorf("missing drained worker %s", h)
		}
	}
	return nil
}
func (b *ProductionBackend) Check(ctx context.Context, c Config) error {
	p, err := b.command(ctx, c, "", "git", "ls-remote", "--exit-code", "--tags", "https://github.com/"+c.StewardRepo+".git", "refs/tags/v"+c.Version)
	if err != nil {
		return err
	}
	if len(strings.Fields(string(p))) < 2 {
		return errors.New("release tag missing")
	}
	p, err = b.command(ctx, c, "", "gh", "api", "repos/"+c.StewardRepo+"/commits/v"+c.Version, "--jq", ".sha")
	if err != nil {
		return err
	}
	sha := strings.TrimSpace(string(p))
	if sha == "" {
		return errors.New("tag commit missing")
	}
	if err = b.green(ctx, c, c.StewardRepo, sha); err != nil {
		return err
	}
	if _, err = b.assets(ctx, c, sha); err != nil {
		return err
	}
	head, err := b.remoteHeads(ctx, c, "")
	if err != nil {
		return err
	}
	if err = b.green(ctx, c, c.UpKeeperRepo, head); err != nil {
		return err
	}
	id, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if id.Health != "healthy" {
		return errors.New("coordinator unhealthy")
	}
	ws, err := b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	for _, h := range c.ActiveWorkers {
		found := false
		for _, w := range ws {
			if w.Requirement.WorkerID == h {
				found = true
				if w.State != "observed" || w.Health != "ready" || w.Stale {
					return fmt.Errorf("%s worker is not ready", h)
				}
			}
		}
		if !found {
			return fmt.Errorf("worker %s missing", h)
		}
	}
	return b.timers(ctx, c, false)
}
func (b *ProductionBackend) setTimerFence(ctx context.Context, c Config, s *State) error {
	if err := b.timers(ctx, c, false); err != nil {
		return err
	}
	reachable, err := b.laptop(ctx, c)
	if err != nil {
		return err
	}
	if s.TimerPrior == nil {
		s.TimerPrior = map[string]string{}
	}
	for _, h := range c.TimerHosts {
		if h == c.LaptopHost && !reachable {
			s.TimerPrior[h] = "unreachable"
			continue
		}
		if prior, ok := s.TimerPrior[h]; !ok || prior == "unreachable" {
			p, err := b.on(ctx, c, h, timerScript)
			if err != nil {
				return err
			}
			s.TimerPrior[h] = string(p)
		}
	}
	if s.Save != nil {
		if err := s.Save(); err != nil {
			return err
		}
	}
	for _, h := range c.TimerHosts {
		if h == c.LaptopHost && !reachable {
			continue
		}
		if _, err = b.on(ctx, c, h, "systemctl --user stop upkeeper-self.timer"); err != nil {
			return err
		}
	}
	// Fences remain stopped; timer release requires the separately reviewed U1 interface.
	return b.timers(ctx, c, true)
}
func manifestScope(before, after []byte, version string) error {
	var a, z map[string]any
	if err := json.Unmarshal(before, &a); err != nil {
		return err
	}
	if err := json.Unmarshal(after, &z); err != nil {
		return err
	}
	for _, path := range [][]string{{"components", "t3-steward", "version"}, {"components", "t3", "compatibility", "steward_version"}} {
		left, right := a, z
		for _, key := range path[:len(path)-1] {
			var ok bool
			left, ok = left[key].(map[string]any)
			if !ok {
				return errors.New("missing before manifest path")
			}
			right, ok = right[key].(map[string]any)
			if !ok {
				return errors.New("missing after manifest path")
			}
		}
		key := path[len(path)-1]
		if right[key] != version {
			return errors.New("both Steward version fields must name target version")
		}
		right[key] = left[key]
	}
	if !reflect.DeepEqual(a, z) {
		return errors.New("author changed keys outside binary-only scope")
	}
	return nil
}

type preview struct {
	Hosts []struct {
		Host, Name, Result string
		Components         []struct {
			Name, State string
			Actions     []string
			ErrorClass  string `json:"error_class"`
		}
	}
}

func scopedPreview(p []byte, c Config) error {
	var v preview
	if err := json.Unmarshal(p, &v); err != nil {
		return err
	}
	seen := map[string]bool{}
	targets := append([]string{c.Canary}, c.FleetOrder...)
	for _, h := range v.Hosts {
		name := h.Host
		if name == "" {
			name = h.Name
		}
		if !contains(targets, name) || seen[name] {
			return errors.New("scoped preview host scope differs")
		}
		seen[name] = true
		if len(h.Components) != 1 {
			return errors.New("scoped preview changed component scope")
		}
		x := h.Components[0]
		if x.Name != "t3-steward" || x.State != "updated" || x.ErrorClass != "" || !strings.Contains(strings.Join(x.Actions, " "), c.Version) {
			return fmt.Errorf("%s scoped preview failed", name)
		}
	}
	if len(seen) != len(targets) {
		return errors.New("scoped preview missing target")
	}
	return nil
}
func (b *ProductionBackend) prepare(ctx context.Context, c Config, s *State) error {
	if err := b.timers(ctx, c, true); err != nil {
		return err
	}
	branch, err := b.command(ctx, c, "", "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(branch)) != "main" {
		return errors.New("release checkout must be on main")
	}
	base, err := b.remoteHeads(ctx, c, "")
	if err != nil {
		return err
	}
	head, err := b.command(ctx, c, "", "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != base {
		return errors.New("release checkout is not at agreed remote main")
	}
	if err = b.green(ctx, c, c.UpKeeperRepo, base); err != nil {
		return err
	}
	// Checkout is an operator-owned isolated release checkout; never capture host state.
	status, err := b.command(ctx, c, "", "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	dirty := len(bytes.TrimSpace(status)) != 0
	if dirty && (strings.TrimSpace(string(status)) != "M desired/release.json" || s.Extra["author-started"] == nil) {
		return errors.New("release checkout dirty without recorded clean author start")
	}
	if !dirty && s.Extra["author-started"] == nil {
		if err = record(s, "author-started", true); err != nil {
			return err
		}
	}
	before, err := b.command(ctx, c, "", "git", "show", "HEAD:desired/release.json")
	if err != nil {
		return err
	}
	var manifest struct {
		Components map[string]struct{ Commit string }
	}
	if err = json.Unmarshal(before, &manifest); err != nil {
		return err
	}
	setup := manifest.Components["omarchy-setup"].Commit
	if setup == "" {
		return errors.New("missing pinned omarchy-setup commit")
	}
	args := []string{"author", "--steward", c.Version, "--omarchy-setup", setup, "--omarchy-setup-repo", c.SetupRepository}
	if _, err = b.command(ctx, c, "", "upkeeper", append(args, "--dry-run", "--json")...); err != nil {
		return err
	}
	if !dirty {
		if _, err = b.command(ctx, c, "", "upkeeper", append(args, "--json")...); err != nil {
			return err
		}
	}
	after, err := b.command(ctx, c, "", "git", "diff", "--", "desired/release.json")
	if err != nil {
		return err
	}
	if err = record(s, "release.diff", string(after)); err != nil {
		return err
	}
	after, err = b.command(ctx, c, "", "cat", "desired/release.json")
	if err != nil {
		return err
	}
	if err = manifestScope(before, after, c.Version); err != nil {
		return err
	}
	status, err = b.command(ctx, c, "", "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(status)) != "M desired/release.json" {
		return errors.New("author changed other paths")
	}
	if err = record(s, "release-before", json.RawMessage(before)); err != nil {
		return err
	}
	if err = record(s, "release-after", json.RawMessage(after)); err != nil {
		return err
	}
	p, err := b.command(ctx, c, "", "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	s.ExpectHead = strings.TrimSpace(string(p))
	p, err = b.command(ctx, c, "", "upkeeper", "fleet-plan", "--json")
	if err != nil {
		return err
	}
	var plan struct {
		ReleaseSHA256 string `json:"release_sha256"`
	}
	if err = json.Unmarshal(p, &plan); err != nil {
		return err
	}
	if len(plan.ReleaseSHA256) != 64 {
		return errors.New("fleet-plan digest unavailable")
	}
	s.ReleaseSHA256 = plan.ReleaseSHA256
	if err = record(s, "fleet-plan", json.RawMessage(p)); err != nil {
		return err
	}
	full, fullErr := b.command(ctx, c, "", "upkeeper", "pull", "--all", "--skip-unreachable", "--dry-run", "--json")
	var fp preview
	if err = json.Unmarshal(full, &fp); err != nil || len(fp.Hosts) == 0 {
		return errors.New("full preview unavailable")
	}
	if fullErr != nil {
		var exit *exec.ExitError
		if !errors.As(fullErr, &exit) || exit.ExitCode() != 40 {
			return fullErr
		}
	}
	if err = record(s, "preview-full", json.RawMessage(full)); err != nil {
		return err
	}
	scopedArgs := append([]string{"pull", "--hosts", c.Canary}, c.FleetOrder...)
	scopedArgs = append(scopedArgs, "--components", "t3-steward", "--dry-run", "--json")
	p, err = b.command(ctx, c, "", "upkeeper", scopedArgs...)
	if err != nil {
		return err
	}
	if err = scopedPreview(p, c); err != nil {
		return err
	}
	if err = record(s, "preview-scoped", json.RawMessage(p)); err != nil {
		return err
	}
	p, err = b.command(ctx, c, "", "upkeeper", "rollback", "--to", c.PreviousRelease, "--hosts", c.Canary, "--components", "t3-steward", "--dry-run", "--json")
	if err != nil {
		return err
	}
	if err = record(s, "preview-rollback", json.RawMessage(p)); err != nil {
		return err
	}
	p, err = b.command(ctx, c, "", "gh", "api", "repos/"+c.StewardRepo+"/commits/v"+c.Version, "--jq", ".sha")
	if err != nil {
		return err
	}
	if err = b.green(ctx, c, c.StewardRepo, strings.TrimSpace(string(p))); err != nil {
		return err
	}
	assets, err := b.assets(ctx, c, strings.TrimSpace(string(p)))
	if err != nil {
		return err
	}
	if err = record(s, "assets", assets); err != nil {
		return err
	}
	if err = record(s, "rc118-findings", map[string]string{
		"B1": "avoided by t3-steward-only pull; full steward-fleet-configuration refusal remains open",
		"B2": "binary replacement gated; mise and catalog effects excluded by scoped pull and fences; live campaign continuity remains open",
		"B3": "laptop excluded, worker mask enforced, reachable laptop self timer fenced; exception unchanged",
		"B4": "self timers stopped with recorded prior state; release remains disabled pending separate full-effect review",
	}); err != nil {
		return err
	}
	// Review must explicitly bind this stable canonical package digest and both fences.
	pkg := struct {
		Head, Release, ConfigDigest string
		TimerPrior                  map[string]string
		Evidence                    map[string]json.RawMessage
	}{s.ExpectHead, s.ReleaseSHA256, s.ConfigDigest, s.TimerPrior, s.Extra}
	raw, err := json.Marshal(pkg)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		return err
	}
	if err = os.WriteFile(c.StatePath+".review-package.json", raw, 0600); err != nil {
		return err
	}
	s.PackageDigest = digest(raw)
	if s.Save != nil {
		return s.Save()
	}
	return nil
}
func (b *ProductionBackend) publish(ctx context.Context, c Config, s *State) error {
	branch, err := b.command(ctx, c, "", "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(branch)) != "main" {
		return errors.New("publication checkout must be on main")
	}
	p, err := os.ReadFile(c.ReviewFile)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(p), "\r\n", "\n"), "\n")
	if lines[0] != "VERDICT: ACCEPT" || !strings.Contains(string(p), s.ExpectHead) || !strings.Contains(string(p), s.ReleaseSHA256) || !contains(lines, "PACKAGE "+s.PackageDigest) {
		return errors.New("review does not accept exact package and fences")
	}
	pkg := struct {
		Head, Release, ConfigDigest string
		TimerPrior                  map[string]string
		Evidence                    map[string]json.RawMessage
	}{s.ExpectHead, s.ReleaseSHA256, s.ConfigDigest, s.TimerPrior, s.Extra}
	raw, err := json.Marshal(pkg)
	if err != nil {
		return err
	}
	if digest(raw) != s.PackageDigest {
		return errors.New("review package changed")
	}
	packageFile, err := os.ReadFile(c.StatePath + ".review-package.json")
	if err != nil {
		return err
	}
	if !bytes.Equal(packageFile, raw) {
		return errors.New("review package file changed")
	}
	if err = b.timers(ctx, c, true); err != nil {
		return err
	}
	if _, err = b.remoteHeads(ctx, c, s.ExpectHead); err != nil {
		return err
	}
	p, err = b.command(ctx, c, "", "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(p)) != s.ExpectHead {
		return errors.New("checkout HEAD moved")
	}
	p, err = b.command(ctx, c, "", "upkeeper", "fleet-plan", "--json")
	if err != nil {
		return err
	}
	var plan struct {
		ReleaseSHA256 string `json:"release_sha256"`
	}
	if err = json.Unmarshal(p, &plan); err != nil {
		return err
	}
	if plan.ReleaseSHA256 != s.ReleaseSHA256 {
		return errors.New("release digest changed")
	}
	if err = record(s, "publication-started", true); err != nil {
		return err
	}
	pushArgs := append([]string{"push", "--authored", "--expect-head", s.ExpectHead, "--expect-release-sha256", s.ReleaseSHA256, "--json"}, c.Remotes...)
	p, pushErr := b.command(ctx, c, "", "upkeeper", pushArgs...)
	if err = record(s, "push-receipt", string(p)); err != nil {
		return err
	}
	if pushErr != nil {
		if err = record(s, "push-error", pushErr.Error()); err != nil {
			return err
		}
		return pushErr
	}
	p, err = b.command(ctx, c, "", "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	s.ReleaseCommit = strings.TrimSpace(string(p))
	if s.Save != nil {
		return s.Save()
	}
	return nil
}

const snapshotScript = `set -eu
b="$HOME/.local/bin/t3-steward"
printf 'arch=%s\n' "$(uname -m)"
printf 'version=%s\n' "$("$b" version)"
printf 'binary_sha256=%s\n' "$(sha256sum "$(readlink -f "$b")" | cut -d" " -f1)"
for u in t3-steward.service t3-steward-worker.service t3code.service; do
 for p in ActiveState MainPID ExecMainStartTimestamp NRestarts; do
  printf '%s.%s=%s\n' "$u" "$p" "$(systemctl --user show "$u" -p "$p" --value)"
 done
 pid=$(systemctl --user show "$u" -p MainPID --value)
 if [ -n "$pid" ] && [ "$pid" != 0 ]; then printf '%s.exe=%s\n' "$u" "$(readlink "/proc/$pid/exe")"; fi
done
`

func snapshotGate(pre, post map[string]string, version string, assets map[string]string) error {
	platform := map[string]string{"x86_64": "linux_amd64", "aarch64": "linux_arm64"}[post["arch"]]
	want := assets[platform]
	if want == "" || post["binary_sha256"] != want || !strings.Contains(post["version"], version) {
		return errors.New("installed binary version/digest mismatch")
	}
	for _, u := range []string{"t3-steward.service", "t3-steward-worker.service"} {
		if post[u+".ActiveState"] != "active" || post[u+".MainPID"] == "" || post[u+".MainPID"] == "0" || pre[u+".MainPID"] == post[u+".MainPID"] || post[u+".exe"] == "" || strings.Contains(post[u+".exe"], "deleted") {
			return fmt.Errorf("%s not a new active binary generation", u)
		}
		if pre[u+".ExecMainStartTimestamp"] != "" && pre[u+".ExecMainStartTimestamp"] == post[u+".ExecMainStartTimestamp"] {
			return fmt.Errorf("%s start timestamp unchanged", u)
		}
	}
	if pre["t3code.service.MainPID"] == "" || pre["t3code.service.MainPID"] != post["t3code.service.MainPID"] || pre["t3code.service.ExecMainStartTimestamp"] != post["t3code.service.ExecMainStartTimestamp"] {
		return errors.New("t3code process restarted")
	}
	return nil
}
func (b *ProductionBackend) wait(ctx context.Context, c Config, predicate func() error) error {
	attempts := c.ReadinessSeconds / 5
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = predicate()
		if last == nil {
			return nil
		}
		if i+1 < attempts {
			if err := b.Sleep(ctx, 5*time.Second); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("readiness timeout: %w", last)
}
func (b *ProductionBackend) waitWorker(ctx context.Context, c Config, host, epoch, release string) error {
	return b.wait(ctx, c, func() error {
		ws, err := b.workers(ctx, c)
		if err != nil {
			return err
		}
		for _, w := range ws {
			if w.Requirement.WorkerID == host {
				if w.Health == "ready" && !w.Stale && w.Snapshot.CoordinatorEpoch.String() == epoch && (release == "" || w.Snapshot.Inventory.Runtime.Release == release) {
					return nil
				}
				return fmt.Errorf("%s worker not ready/current", host)
			}
		}
		return fmt.Errorf("%s worker absent", host)
	})
}
func (b *ProductionBackend) quiet(ctx context.Context, c Config) error {
	if len(c.QuietCommand) == 0 {
		return errors.New("quiet predicate not configured")
	}
	_, err := b.command(ctx, c, "", c.QuietCommand[0], c.QuietCommand[1:]...)
	return err
}
func (b *ProductionBackend) host(ctx context.Context, c Config, host string, s *State) error {
	if host == c.LaptopHost || !contains(append([]string{c.Canary}, c.FleetOrder...), host) {
		return errors.New("host outside rollout scope")
	}
	if err := b.releaseCI(ctx, c, s); err != nil {
		return err
	}
	p, err := b.command(ctx, c, "", "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(p)) != s.ReleaseCommit {
		return errors.New("checkout differs from published release")
	}
	if host == c.CoordinatorHost {
		if s.BackupCreated.IsZero() || b.Now().Before(s.BackupCreated) || b.Now().Sub(s.BackupCreated) > time.Duration(c.BackupMaxAgeSeconds)*time.Second {
			return errors.New("coordinator backup absent or stale")
		}
		var kv map[string]string
		if err = evidence(s, "backup", &kv); err != nil {
			return err
		}
		if kv["restore_integrity"] != "ok" || kv["restore_fk_violations"] != "0" || s.BackupSnapshot == "" || s.BackupScratch == "" {
			return errors.New("backup lacks successful scratch restore")
		}
		if err = b.quiet(ctx, c); err != nil {
			return err
		}
	}
	preID, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if preID.Health != "healthy" || preID.Epoch == "" {
		return errors.New("coordinator unhealthy before convergence")
	}
	if contains(c.ActiveWorkers, host) || contains(c.DrainedHosts, host) {
		if err = b.waitWorker(ctx, c, host, preID.Epoch.String(), ""); err != nil {
			return err
		}
	}
	if host == c.Canary {
		ws, err := b.workers(ctx, c)
		if err != nil {
			return err
		}
		found := false
		for _, w := range ws {
			if w.Requirement.WorkerID == host {
				found = true
				n := w.Snapshot.Inventory.Telemetry.RunningAttempts
				if n == nil || *n != 0 {
					return errors.New("canary not idle")
				}
			}
		}
		if !found {
			return errors.New("canary absent")
		}
	}
	raw, err := b.on(ctx, c, host, snapshotScript)
	if err != nil {
		return err
	}
	pre := keyValues(raw)
	for _, unit := range []string{"t3-steward.service", "t3-steward-worker.service"} {
		if pre[unit+".ActiveState"] != "active" || pre[unit+".MainPID"] == "" || pre[unit+".MainPID"] == "0" {
			return fmt.Errorf("%s must be active before convergence", unit)
		}
	}
	if pre["t3code.service.MainPID"] == "" {
		return errors.New("missing pre-convergence t3code generation")
	}
	if err = record(s, "host-pre:"+host, pre); err != nil {
		return err
	}
	if err = record(s, "identity-pre:"+host, preID); err != nil {
		return err
	}
	p, err = b.command(ctx, c, "", "upkeeper", "pull", "--hosts", host, "--components", "t3-steward", "--json")
	if err != nil {
		return err
	}
	if err = record(s, "pull:"+host, json.RawMessage(p)); err != nil {
		return err
	}
	raw, err = b.on(ctx, c, host, snapshotScript)
	if err != nil {
		return err
	}
	post := keyValues(raw)
	var assets assetEvidence
	if err = evidence(s, "assets", &assets); err != nil {
		return err
	}
	if err = snapshotGate(pre, post, c.Version, assets.Binaries); err != nil {
		return err
	}
	if err = record(s, "host-post:"+host, post); err != nil {
		return err
	}
	var id identity
	err = b.wait(ctx, c, func() error {
		var e error
		id, e = b.identity(ctx, c)
		if e != nil {
			return e
		}
		if id.Health != "healthy" || id.Epoch == "" {
			return errors.New("coordinator not healthy yet")
		}
		if host == c.CoordinatorHost {
			if id.Release != c.Version {
				return errors.New("coordinator wrong release")
			}
			if id.Epoch == preID.Epoch {
				return errors.New("coordinator epoch did not advance")
			}
		} else if id.Epoch != preID.Epoch {
			return errors.New("coordinator epoch changed outside coordinator step")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if contains(c.ActiveWorkers, host) || contains(c.DrainedHosts, host) {
		if err = b.waitWorker(ctx, c, host, id.Epoch.String(), c.Version); err != nil {
			return err
		}
	}
	if host == c.CoordinatorHost {
		for _, w := range append(append([]string{}, c.ActiveWorkers...), c.DrainedHosts...) {
			if w == host {
				continue
			}
			if err = b.waitWorker(ctx, c, w, id.Epoch.String(), ""); err != nil {
				return err
			}
		}
	}
	ws, err := b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	final, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if final.Health != "healthy" || final.Epoch != id.Epoch {
		return errors.New("coordinator epoch/health changed during host gates")
	}
	return record(s, "identity-post:"+host, final)
}
func (b *ProductionBackend) releaseCI(ctx context.Context, c Config, s *State) error {
	if s.ReleaseCommit == "" {
		return errors.New("published release commit missing")
	}
	if err := b.timers(ctx, c, true); err != nil {
		return err
	}
	if _, err := b.remoteHeads(ctx, c, s.ReleaseCommit); err != nil {
		return err
	}
	return b.green(ctx, c, c.UpKeeperRepo, s.ReleaseCommit)
}

const legacyBackupScript = `set -euo pipefail
cd "$HOME"
snap="$HOME/$1" scratch="$HOME/$2"
b="$HOME/.local/bin/t3-steward"
umask 077
mkdir -p "$(dirname "$snap")"
chmod 700 "$(dirname "$snap")"
trap 'systemctl --user start t3-steward.service t3-steward-worker.service' EXIT
systemctl --user stop t3-steward.service t3-steward-worker.service
for _ in $(seq 1 60); do systemctl --user is-active --quiet t3-steward.service || break; sleep 1; done
if systemctl --user is-active --quiet t3-steward.service; then echo "coordinator did not stop"; exit 1; fi
db="$HOME/.local/state/t3-steward/state.db"
if fuser "$db" >/dev/null 2>&1; then echo "state.db still open after stop"; systemctl --user start t3-steward.service t3-steward-worker.service; exit 1; fi
python3 - "$db" <<'PY' || { systemctl --user start t3-steward.service t3-steward-worker.service; echo "WAL checkpoint failed"; exit 1; }
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
busy, log, done = db.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone()
db.close()
print("wal checkpoint busy", busy, "log", log, "checkpointed", done)
sys.exit(1 if busy else 0)
PY
for f in "$db-wal" "$db-shm"; do [ ! -s "$f" ] && rm -f "$f"; done
[ -e "$db-wal" ] && { echo "WAL still present after checkpoint"; systemctl --user start t3-steward.service t3-steward-worker.service; exit 1; }
rc=0
{ "$b" backlog backup create "$snap" >"$snap.create.json" && "$b" backlog backup verify "$snap" >"$snap.verify.json"; } || rc=$?
systemctl --user start t3-steward.service t3-steward-worker.service
[ "$rc" -eq 0 ] || { echo "backup create/verify failed with $rc"; exit "$rc"; }
echo "snapshot ok: $(sha256sum "$snap.create.json" | cut -d' ' -f1) manifest"
# Scratch restore drill. The configuration loader always applies the UpKeeper-owned fleet
# projection in $HOME/.config/t3-steward/coordinator-fleet.json, whatever --config names, and
# refuses one whose coordinator id differs from the file's ("coordinator fleet projection
# names a different coordinator"). The scratch file is therefore a copy of the production
# file, keeping coordinator.id and the worker transport bindings the projection needs, with
# every path the coordinator writes rewritten into the scratch directory. Restore is offline:
# it reaches no coordinator or worker, and its lock is taken on the scratch database path.
mkdir -p "$scratch"
chmod 700 "$scratch"
prod_cfg="$HOME/.config/t3-steward/config.yaml"
python3 - "$prod_cfg" "$scratch" >"$scratch/config.yaml" <<'PY'
import re, sys
src, scratch = sys.argv[1], sys.argv[2]
want = {
    ("state_path",): f"{scratch}/state.db",
    ("backlog_v2", "storage", "artifacts"): f"{scratch}/artifacts",
    ("backlog_v2", "storage", "bundles"): f"{scratch}/bundles",
    ("backlog_v2", "storage", "workspaces"): f"{scratch}/workspaces",
    ("backlog", "dir"): f"{scratch}/backlog-drop",
    ("archive", "enabled"): "false",
    ("archive", "destination"): f"{scratch}/archive",
}
seen, stack, out = {}, [], []
for line in open(src):
    m = re.match(r"^( *)([A-Za-z_][A-Za-z0-9_-]*):(.*)$", line.rstrip("\n"))
    if m:
        indent = len(m.group(1))
        while stack and stack[-1][0] >= indent:
            stack.pop()
        path = tuple(k for _, k in stack) + (m.group(2),)
        stack.append((indent, m.group(2)))
        if path in want:
            seen[path] = seen.get(path, 0) + 1
            line = f"{m.group(1)}{m.group(2)}: {want[path]}\n"
    out.append(line)
bad = [ "/".join(p) for p in want if seen.get(p) != 1 ]
if bad:
    sys.stderr.write(f"scratch config: production config shape changed, keys not found exactly once: {bad}\n")
    sys.exit(1)
sys.stdout.write("".join(out))
PY
chmod 600 "$scratch/config.yaml"
mode=$(sed -n 's/^  mode: *//p' "$scratch/config.yaml" | head -1)
[ "$mode" = coordinator ] || { echo "scratch config: backlog_v2.mode is '$mode', not coordinator"; exit 1; }
# No production write path may survive in the scratch file.
if grep -nE '^(state_path| +(artifacts|bundles|workspaces|dir|destination)):' "$scratch/config.yaml" | grep -v -F "$scratch"; then
  echo "scratch config still names a non-scratch write path; refusing the drill"; exit 1
fi
prod_inode=$(stat -c %i "$db")
rc=0
(
  # A T3_STEWARD_STATE_PATH (or any other override) would redirect the restore away from scratch.
  for v in $(compgen -e); do case $v in T3_STEWARD_*) unset "$v" ;; esac; done
  "$b" backlog backup restore "$snap" --config "$scratch/config.yaml"
) >"$scratch/restore.json" 2>"$scratch/restore.err" || rc=$?
if [ "$rc" -ne 0 ]; then
  cat "$scratch/restore.err"
  if grep -q 'fleet projection names a different coordinator' "$scratch/restore.err"; then
    echo "REFUSED: the restore cannot run without the production coordinator identity; see gaps.md S2. No backup.env is written."
  fi
  echo "scratch restore failed with $rc"
  exit 1
fi
[ "$(stat -c %i "$db")" = "$prod_inode" ] || { echo "production state.db changed identity during the drill"; exit 1; }
[ -f "$scratch/state.db" ] && [ -d "$scratch/artifacts" ] || { echo "restore did not publish into the scratch targets"; exit 1; }
python3 - "$scratch/state.db" <<'PY'
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
ic = db.execute("PRAGMA integrity_check").fetchall()
fk = db.execute("PRAGMA foreign_key_check").fetchall()
uv = db.execute("PRAGMA user_version").fetchone()[0]
tables = db.execute("select count(*) from sqlite_master where type='table'").fetchone()[0]
print("scratch restore: integrity", ic[0][0], "foreign-key violations", len(fk), "user_version", uv, "tables", tables)
print("restore_integrity=" + ("ok" if ic == [("ok",)] else "failed"))
print("restore_fk_violations=%d" % len(fk))
sys.exit(0 if ic == [("ok",)] and not fk else 1)
PY
echo "scratch=$scratch"
echo "snapshot=$snap"`

func (b *ProductionBackend) backup(ctx context.Context, c Config, s *State) error {
	if err := b.quiet(ctx, c); err != nil {
		return err
	}
	if err := b.timers(ctx, c, true); err != nil {
		return err
	}
	var out []byte
	var err error
	if len(c.BackupCommand) > 0 {
		out, err = b.on(ctx, c, c.CoordinatorHost, "exec "+strings.Join(quoted(c.BackupCommand), " "))
	} else {
		stamp := b.Now().UTC().Format("20060102T150405Z")
		out, err = b.on(ctx, c, c.CoordinatorHost, legacyBackupScript, ".local/state/t3-steward-rollout-backups/pre-"+stamp, ".local/state/t3-steward-rollout-backups/scratch-restore-"+stamp)
	}
	if err != nil {
		return err
	}
	kv := keyValues(out)
	if kv["restore_integrity"] != "ok" || kv["restore_fk_violations"] != "0" || kv["snapshot"] == "" || kv["scratch"] == "" {
		return errors.New("backup has no successful stopped snapshot and scratch drill evidence")
	}
	if err = b.wait(ctx, c, func() error {
		id, e := b.identity(ctx, c)
		if e != nil {
			return e
		}
		if id.Health != "healthy" {
			return errors.New("coordinator reconnecting after backup")
		}
		return nil
	}); err != nil {
		return err
	}
	s.BackupSnapshot = kv["snapshot"]
	s.BackupScratch = kv["scratch"]
	s.BackupCreated = b.Now()
	return record(s, "backup", kv)
}
func quoted(xs []string) []string {
	out := make([]string, len(xs))
	for i, s := range xs {
		out[i] = quote(s)
	}
	return out
}
func (b *ProductionBackend) enroll(ctx context.Context, c Config, s *State) error {
	id, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if id.Health != "healthy" {
		return errors.New("coordinator unhealthy before enrollment")
	}
	ws, err := b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	if err = record(s, "workers-before-enroll", ws); err != nil {
		return err
	}
	for _, h := range c.ActiveWorkers {
		if contains(c.DrainedHosts, h) || h == c.LaptopHost {
			return errors.New("cannot enroll drained/excluded worker")
		}
		found := false
		for _, w := range ws {
			if w.Requirement.WorkerID != h {
				continue
			}
			found = true
			if w.Requirement.Draining || w.State == "draining" {
				return errors.New("active worker is actually drained")
			}
			if w.Enrolled && len(w.Requirement.CatalogRevision) > 0 && bytes.Equal(w.Requirement.CatalogRevision, w.Enrollment.Request.CatalogRevision) {
				continue
			}
			if _, err = b.on(ctx, c, c.CoordinatorHost, "exec \"$HOME/.local/bin/t3-steward\" worker enroll "+quote(h)+" --current-catalog --reason "+quote("binary-only rollout "+c.Version)); err != nil {
				return err
			}
		}
		if !found {
			return fmt.Errorf("worker %s missing", h)
		}
	}
	for _, h := range c.ActiveWorkers {
		if err = b.waitWorker(ctx, c, h, id.Epoch.String(), c.Version); err != nil {
			return err
		}
	}
	ws, err = b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	for _, h := range c.ActiveWorkers {
		for _, w := range ws {
			if w.Requirement.WorkerID == h && (!w.Enrolled || w.State != "observed" || !bytes.Equal(w.Requirement.CatalogRevision, w.Enrollment.Request.CatalogRevision)) {
				return fmt.Errorf("%s not enrolled/current", h)
			}
		}
	}
	final, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if final.Health != "healthy" || final.Epoch != id.Epoch {
		return errors.New("coordinator changed during enrollment")
	}
	return nil
}
func (b *ProductionBackend) verify(ctx context.Context, c Config, s *State) error {
	if err := b.timers(ctx, c, true); err != nil {
		return err
	}
	id, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if id.Health != "healthy" || id.Release != c.Version {
		return errors.New("final coordinator not healthy on release")
	}
	for _, h := range append(append([]string{}, c.ActiveWorkers...), c.DrainedHosts...) {
		if err = b.waitWorker(ctx, c, h, id.Epoch.String(), c.Version); err != nil {
			return err
		}
	}
	ws, err := b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	for _, h := range c.ActiveWorkers {
		found := false
		for _, w := range ws {
			if w.Requirement.WorkerID == h {
				found = true
				if !w.Enrolled || w.State != "observed" {
					return errors.New("active worker not enrolled/observed")
				}
			}
		}
		if !found {
			return errors.New("active worker missing")
		}
	}
	var assets assetEvidence
	if err = evidence(s, "assets", &assets); err != nil {
		return err
	}
	for _, h := range append([]string{c.Canary}, c.FleetOrder...) {
		var pre map[string]string
		if err = evidence(s, "host-pre:"+h, &pre); err != nil {
			return err
		}
		p, err := b.on(ctx, c, h, snapshotScript)
		if err != nil {
			return err
		}
		if err = snapshotGate(pre, keyValues(p), c.Version, assets.Binaries); err != nil {
			return err
		}
	}
	p, err := b.command(ctx, c, "", "upkeeper", "hold", "show", "--json")
	if err != nil {
		return err
	}
	var holds struct{ Holds []json.RawMessage }
	if err = json.Unmarshal(p, &holds); err != nil {
		return err
	}
	if holds.Holds == nil || len(holds.Holds) != 0 {
		return errors.New("failed scheduled-run holds present or missing")
	}
	final, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if final.Health != "healthy" || final.Epoch != id.Epoch {
		return errors.New("final coordinator identity changed")
	}
	return nil
}
func (b *ProductionBackend) observe(ctx context.Context, c Config, s *State) error {
	if err := b.timers(ctx, c, true); err != nil {
		return err
	}
	var expected identity
	if err := evidence(s, "identity-post:"+c.Canary, &expected); err != nil {
		return err
	}
	id, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if id.Health != "healthy" || id.Epoch != expected.Epoch {
		return errors.New("canary soak coordinator changed")
	}
	if err = b.waitWorker(ctx, c, c.Canary, id.Epoch.String(), c.Version); err != nil {
		return err
	}
	ws, err := b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	var pre map[string]string
	if err = evidence(s, "host-pre:"+c.Canary, &pre); err != nil {
		return err
	}
	var assets assetEvidence
	if err = evidence(s, "assets", &assets); err != nil {
		return err
	}
	p, err := b.on(ctx, c, c.Canary, snapshotScript)
	if err != nil {
		return err
	}
	return snapshotGate(pre, keyValues(p), c.Version, assets.Binaries)
}
func (b *ProductionBackend) Execute(ctx context.Context, c Config, action string, s *State) error {
	switch action {
	case "fence":
		return b.fence(ctx, c, s)
	case "prepare":
		return b.prepare(ctx, c, s)
	case "publish":
		return b.publish(ctx, c, s)
	case "release-ci":
		return b.releaseCI(ctx, c, s)
	case "backup":
		return b.createBackup(ctx, c, s)
	case "enroll":
		return b.enroll(ctx, c, s)
	case "verify":
		return b.verify(ctx, c, s)
	case "canary-observe":
		return b.observe(ctx, c, s)
	}
	if strings.HasPrefix(action, "host:") {
		return b.host(ctx, c, strings.TrimPrefix(action, "host:"), s)
	}
	if strings.HasPrefix(action, "rollback:") {
		host := strings.TrimPrefix(action, "rollback:")
		if !contains(append([]string{c.Canary}, c.FleetOrder...), host) {
			return errors.New("rollback outside rollout scope")
		}
		p, err := b.command(ctx, c, "", "upkeeper", "rollback", "--to", c.PreviousRelease, "--hosts", host, "--components", "t3-steward", "--json")
		if err != nil {
			return err
		}
		if err = record(s, "rollback:"+host, string(p)); err != nil {
			return err
		}
		var pre map[string]string
		if err = evidence(s, "host-pre:"+host, &pre); err != nil {
			return fmt.Errorf("rollback command completed; no prior snapshot to verify: %w", err)
		}
		raw, err := b.on(ctx, c, host, snapshotScript)
		if err != nil {
			return err
		}
		post := keyValues(raw)
		if post["binary_sha256"] != pre["binary_sha256"] || post["version"] != pre["version"] {
			return errors.New("rollback installed binary differs from immutable previous generation")
		}
		for _, unit := range []string{"t3-steward.service", "t3-steward-worker.service"} {
			if post[unit+".ActiveState"] != pre[unit+".ActiveState"] || strings.Contains(post[unit+".exe"], "deleted") {
				return fmt.Errorf("%s rollback service state differs", unit)
			}
		}
		if post["t3code.service.MainPID"] != pre["t3code.service.MainPID"] {
			return errors.New("rollback restarted t3code")
		}
		if err = b.wait(ctx, c, func() error {
			id, e := b.identity(ctx, c)
			if e != nil {
				return e
			}
			if id.Health != "healthy" {
				return errors.New("coordinator not healthy after rollback")
			}
			return nil
		}); err != nil {
			if host == c.CoordinatorHost {
				return fmt.Errorf("immutable binary rollback completed but coordinator recovery needs supervised stopped restore from %s: %w", s.BackupSnapshot, err)
			}
			return err
		}
		ws, err := b.workers(ctx, c)
		if err != nil {
			return err
		}
		return drains(ws, c)
	}
	return fmt.Errorf("unknown rollout action %s", action)
}
