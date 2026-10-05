package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func t3ArtifactObservation(c *Context, spec, observed map[string]any) {
	prefix := adapterSpecString(observed, "npm_prefix")
	_, desired := spec["artifact"]
	managed := release.T3ControlledEvidence(c.Home, prefix)
	observed["artifact_managed"] = managed
	if !desired && !managed {
		return
	}
	observed["artifact_installed"] = false
	observed["artifact_served"] = false
	if !desired && release.VerifyT3Upstream(c.Home, prefix, adapterSpecString(spec, "version")) == nil {
		if e := t3ControlledSelected(c, prefix); e != nil {
			observed["artifact_error"] = e.Error()
			return
		}
		observed["artifact_installed"] = true
		observed["artifact_served"] = t3UpstreamServed(c, spec, prefix)
		return
	}
	retained, e := release.ReadT3Retained(c.Home)
	if e != nil {
		observed["artifact_error"] = e.Error()
		return
	}
	if retained.Prefix != prefix {
		observed["artifact_error"] = "controlled T3 npm prefix differs"
		return
	}
	node, selected := t3SelectedPrograms(c)
	_ = node
	actual, e := filepath.EvalSymlinks(selected)
	if e != nil || actual != filepath.Join(prefix, "lib/node_modules/t3/bin/t3.js") {
		observed["artifact_error"] = "selected T3 launcher differs"
		return
	}
	raw, ok := spec["artifact"]
	if !ok {
		observed["artifact_error"] = "controlled-to-upstream identity transition required"
		return
	}
	wanted, e := release.DecodeT3Artifact(raw, adapterSpecString(spec, "version"))
	if e != nil {
		observed["artifact_error"] = e.Error()
		return
	}
	b, _ := json.Marshal(wanted)
	a, _ := json.Marshal(retained.Artifact)
	if string(a) != string(b) {
		observed["artifact_error"] = "installed controlled T3 identity differs"
		return
	}
	observed["artifact_installed"] = true
	observed["artifact_served"] = t3UnitSelectsOwned(c, adapterSpecString(spec, "service"), prefix) && t3ServedArtifact(c, spec, retained)
}

// Kernel executable identity is bound to the unit MainPID lifetime and its
// descendant tree. Deleted/replaced inodes and a different prefix stay unknown.
func t3ServedArtifact(c *Context, spec map[string]any, r *release.T3Retained) bool {
	service := adapterSpecString(spec, "service")
	get := func() string {
		result := c.Run([]string{"systemctl", "--user", "show", service, "-p", "MainPID", "--value"}, 10*time.Second, "")
		pid := strings.TrimSpace(result.Output)
		n, e := strconv.Atoi(pid)
		if !result.OK || e != nil || n <= 0 {
			return ""
		}
		return pid
	}
	start := func(pid string) string {
		b, e := os.ReadFile(filepath.Join(t3ProcRoot, pid, "stat"))
		if e != nil {
			return ""
		}
		text := string(b)
		i := strings.LastIndex(text, ") ")
		if i < 0 {
			return ""
		}
		fields := strings.Fields(text[i+2:])
		if len(fields) < 20 {
			return ""
		}
		return fields[19]
	}
	pid := get()
	epoch := start(pid)
	if pid == "" || epoch == "" {
		return false
	}
	native := filepath.Join(r.Prefix, "lib/node_modules/t3/node_modules/@t3code/t3-"+r.Receipt.Platform+"/t3")
	pending := []string{pid}
	visited := map[string]bool{}
	found := ""
	nativeEpoch := ""
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		if visited[current] {
			return false
		}
		visited[current] = true
		if len(visited) > 4096 {
			return false
		}
		base := filepath.Join(t3ProcRoot, current)
		_, children, ok := t3ProcessFiles(base)
		if !ok {
			return false
		}
		pending = append(pending, children...)
		link, e := os.Readlink(filepath.Join(base, "exe"))
		if e != nil {
			return false
		}
		if link != native {
			continue
		}
		a, e := os.Stat(filepath.Join(base, "exe"))
		if e != nil {
			return false
		}
		b, e := os.Stat(native)
		if e != nil || !os.SameFile(a, b) {
			return false
		}
		cmd, e := os.ReadFile(filepath.Join(base, "cmdline"))
		if e != nil {
			return false
		}
		args := strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")
		if len(args) < 2 || args[1] != "start" {
			continue
		}
		if found != "" {
			return false
		}
		found = current
		nativeEpoch = start(current)
	}
	if found == "" || nativeEpoch == "" {
		return false
	}
	body, e := release.T3ReadFile(native, release.T3FileLimit)
	if e != nil {
		return false
	}
	hash := ""
	for _, f := range r.Receipt.Files {
		if f.Path == "node_modules/@t3code/t3-"+r.Receipt.Platform+"/t3" {
			hash = f.SHA256
		}
	}
	if release.T3Digest(body) != hash {
		return false
	}
	link, e := os.Readlink(filepath.Join(t3ProcRoot, found, "exe"))
	a, e2 := os.Stat(filepath.Join(t3ProcRoot, found, "exe"))
	b, e3 := os.Stat(native)
	return e == nil && e2 == nil && e3 == nil && link == native && os.SameFile(a, b) &&
		epoch == start(pid) && nativeEpoch == start(found) && get() == pid
}

func t3ControlledSelected(c *Context, prefix string) error {
	real, e := filepath.EvalSymlinks(prefix)
	if e != nil || real != prefix {
		return errors.New("T3 prefix changed")
	}
	_, selected := t3SelectedPrograms(c)
	resolved, e := filepath.EvalSymlinks(selected)
	if e != nil || resolved != filepath.Join(prefix, "lib/node_modules/t3/bin/t3.js") {
		return errors.New("selected T3 launcher is outside owned prefix")
	}
	return nil
}

type t3Transaction struct {
	c                                                                         *Context
	prefix, packagePath, link, state, dir                                     string
	oldDigest, oldLink, oldStateDigest                                        string
	hadPackage, hadLink, hadState                                             bool
	installedDigest, newLink, newStateDigest                                  string
	packageMoved, packageActivated, linkActivated, stateMoved, stateActivated bool
	fault                                                                     func(string) error // Deterministic filesystem transition injection in tests.
	activationGuard                                                           func() error
	guard                                                                     func() error // Fresh proof after hashing, immediately before mutation.
}

func t3Begin(c *Context, prefix string) (*t3Transaction, error) {
	// Limit mutations to the resolved user-owned prefix, never Node or a mise shim.
	rel, e := filepath.Rel(c.Home, prefix)
	if e != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, errors.New("T3 controlled prefix must be inside the user's home")
	}
	if _, e = agentenv.TargetPath(c.Home, rel); e != nil {
		return nil, e
	}
	packagePath := filepath.Join(prefix, "lib/node_modules/t3")
	link := filepath.Join(prefix, "bin/t3")
	// Parent paths must not redirect to another prefix.
	for _, p := range []string{filepath.Dir(packagePath), filepath.Dir(link)} {
		real, e := filepath.EvalSymlinks(p)
		if e != nil || real != p {
			return nil, errors.New("T3 package/link parent changed")
		}
	}
	state, e := agentenv.TargetPath(c.Home, release.T3StatePath)
	if e != nil {
		return nil, e
	}
	if state != filepath.Join(c.Home, release.T3StatePath) {
		return nil, errors.New("T3 retained state redirects")
	}
	if e = os.MkdirAll(state, 0700); e != nil {
		return nil, e
	}
	tx := &t3Transaction{c: c, prefix: prefix, packagePath: packagePath, link: link, state: filepath.Join(state, "current")}
	if i, e := os.Lstat(packagePath); e == nil {
		if !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("refusing unusual existing T3 package")
		}
		tx.hadPackage = true
		tx.oldDigest, e = release.T3TreeDigest(packagePath)
		if e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if i, e := os.Lstat(link); e == nil {
		if i.Mode()&os.ModeSymlink == 0 {
			return nil, errors.New("refusing non-owned T3 launcher")
		}
		tx.oldLink, e = os.Readlink(link)
		if e != nil {
			return nil, e
		}
		resolved, e := filepath.EvalSymlinks(link)
		if e != nil || !strings.HasPrefix(resolved, packagePath+string(os.PathSeparator)) {
			return nil, errors.New("T3 launcher is not owned by selected package")
		}
		tx.hadLink = true
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if _, e := os.Lstat(tx.state); e == nil {
		tx.hadState = true
		tx.oldStateDigest, e = release.T3TreeDigest(tx.state)
		if e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	// Backup and stage share the package filesystem; current state uses its own
	// filesystem rename. Every partial transition has an explicit restoration path.
	tx.dir, e = os.MkdirTemp(filepath.Dir(packagePath), ".upkeeper-t3-")
	if e != nil {
		return nil, e
	}
	return tx, nil
}
func t3LinkValue(p string) (string, error) {
	v, e := os.Readlink(p)
	if os.IsNotExist(e) {
		return "", nil
	}
	return v, e
}
func (tx *t3Transaction) unchanged() error {
	if e := tx.parentsUnchanged(); e != nil {
		return e
	}
	real, e := filepath.EvalSymlinks(tx.prefix)
	if e != nil || real != tx.prefix {
		return errors.New("T3 prefix ownership changed")
	}
	digest, e := release.T3TreeDigest(tx.packagePath)
	if tx.hadPackage {
		if e != nil || digest != tx.oldDigest {
			return errors.New("T3 package changed during staging")
		}
	} else if !os.IsNotExist(e) {
		return errors.New("T3 package appeared during staging")
	}
	link, e := t3LinkValue(tx.link)
	if e != nil || link != tx.oldLink {
		return errors.New("T3 launcher changed during staging")
	}
	digest, e = release.T3TreeDigest(tx.state)
	if tx.hadState {
		if e != nil || digest != tx.oldStateDigest {
			return errors.New("T3 provenance changed during staging")
		}
	} else if !os.IsNotExist(e) {
		return errors.New("T3 provenance appeared during staging")
	}
	return nil
}
func (tx *t3Transaction) record(phase string) error {
	b, e := json.Marshal(map[string]any{"phase": phase, "prefix": tx.prefix, "package": tx.packagePath, "link": tx.link, "state": tx.state,
		"old_digest": tx.oldDigest, "old_link": tx.oldLink, "old_state_digest": tx.oldStateDigest,
		"had_package": tx.hadPackage, "had_link": tx.hadLink, "had_state": tx.hadState,
		"new_digest": tx.installedDigest, "new_link": tx.newLink, "new_state_digest": tx.newStateDigest,
		"package_moved": tx.packageMoved, "package_activated": tx.packageActivated, "link_activated": tx.linkActivated, "state_moved": tx.stateMoved, "state_activated": tx.stateActivated})
	if e != nil {
		return e
	}
	if e = tx.persistJournal(phase, b); e == nil && tx.fault != nil {
		e = tx.fault(phase)
	}
	if e != nil {
		return &t3JournalError{e}
	}
	return nil
}
func (tx *t3Transaction) activate(stage, stateStage string) error {
	if e := tx.unchanged(); e != nil {
		return e
	}
	digest, e := release.T3TreeDigest(stage)
	if e != nil {
		return e
	}
	tx.installedDigest = digest
	tx.newStateDigest, e = release.T3TreeDigest(stateStage)
	if e != nil {
		return e
	}
	tx.newLink = "../lib/node_modules/t3/bin/t3.js"
	if tx.guard != nil {
		if e = tx.guard(); e != nil {
			return e
		}
	}
	if tx.activationGuard != nil {
		if e = tx.activationGuard(); e != nil {
			return e
		}
	}
	if e = tx.record("before-package-move"); e != nil {
		return e
	}
	if tx.hadPackage {
		if e = os.Rename(tx.packagePath, filepath.Join(tx.dir, "previous")); e != nil {
			return e
		}
		tx.packageMoved = true
	}
	if e = tx.record("package-moved"); e != nil {
		return e
	}
	if e = os.Rename(stage, tx.packagePath); e != nil {
		return e
	}
	tx.packageActivated = true
	if e = tx.record("package-activated"); e != nil {
		return e
	}
	if e = t3SwapLink(tx.newLink, tx.link); e != nil {
		return e
	}
	tx.linkActivated = true
	if e = tx.record("link-activated"); e != nil {
		return e
	}
	if tx.hadState {
		if e = os.Rename(tx.state, tx.state+"-previous"); e != nil {
			return e
		}
		tx.stateMoved = true
	}
	if e = tx.record("state-moved"); e != nil {
		return e
	}
	if e = os.Rename(stateStage, tx.state); e != nil {
		return e
	}
	tx.stateActivated = true
	return tx.record("state-activated")
}
func t3SwapLink(value, link string) error {
	f, e := os.CreateTemp(filepath.Dir(link), ".upkeeper-t3-link-")
	if e != nil {
		return e
	}
	p := f.Name()
	if e = f.Close(); e != nil {
		return e
	}
	defer os.Remove(p)
	if e = os.Remove(p); e != nil {
		return e
	}
	if e = os.Symlink(value, p); e != nil {
		return e
	}
	return os.Rename(p, link)
}
func (tx *t3Transaction) restore() error {
	if e := tx.parentsUnchanged(); e != nil {
		return e
	}
	// Refuse restoration if another actor changed ANY owned activated boundary.
	real, e := filepath.EvalSymlinks(tx.prefix)
	if e != nil || real != tx.prefix {
		return errors.New("rollback prefix changed")
	}
	if tx.packageActivated {
		d, e := release.T3TreeDigest(tx.packagePath)
		if e != nil || d != tx.installedDigest {
			return errors.New("rollback package ownership changed")
		}
	}
	if tx.packageMoved {
		d, e := release.T3TreeDigest(filepath.Join(tx.dir, "previous"))
		if e != nil || d != tx.oldDigest {
			return errors.New("rollback backup changed")
		}
	}
	expected := tx.oldLink
	if tx.linkActivated {
		expected = tx.newLink
	}
	link, e := t3LinkValue(tx.link)
	if e != nil || link != expected {
		return errors.New("rollback launcher ownership changed")
	}
	if tx.stateActivated {
		d, e := release.T3TreeDigest(tx.state)
		if e != nil || d != tx.newStateDigest {
			return errors.New("rollback provenance ownership changed")
		}
	}
	if tx.stateMoved {
		d, e := release.T3TreeDigest(tx.state + "-previous")
		if e != nil || d != tx.oldStateDigest {
			return errors.New("rollback retained backup changed")
		}
	}
	if tx.guard != nil {
		if e = tx.guard(); e != nil {
			return e
		}
	}
	if tx.packageActivated {
		if e = os.Rename(tx.packagePath, filepath.Join(tx.dir, "failed")); e != nil {
			return e
		}
	} else if tx.packageMoved {
		if _, e = os.Lstat(tx.packagePath); !os.IsNotExist(e) {
			return errors.New("rollback foreign package appeared")
		}
	}
	if tx.packageMoved {
		if e = os.Rename(filepath.Join(tx.dir, "previous"), tx.packagePath); e != nil {
			return e
		}
	}
	if tx.linkActivated {
		if tx.hadLink {
			e = t3SwapLink(tx.oldLink, tx.link)
		} else {
			e = os.Remove(tx.link)
		}
		if e != nil {
			return e
		}
	}
	if tx.stateActivated {
		if e = os.Rename(tx.state, filepath.Join(filepath.Dir(tx.state), "failed-"+filepath.Base(tx.dir))); e != nil {
			return e
		}
	}
	if tx.stateMoved {
		if e = os.Rename(tx.state+"-previous", tx.state); e != nil {
			return e
		}
	}
	if tx.hadPackage {
		d, e := release.T3TreeDigest(tx.packagePath)
		if e != nil || d != tx.oldDigest {
			return errors.New("restored T3 bytes/modes differ")
		}
	} else if _, e = os.Lstat(tx.packagePath); !os.IsNotExist(e) {
		return errors.New("restored T3 absence differs")
	}
	link, e = t3LinkValue(tx.link)
	if e != nil || link != tx.oldLink {
		return errors.New("restored T3 launcher differs")
	}
	if tx.hadState {
		d, e := release.T3TreeDigest(tx.state)
		if e != nil || d != tx.oldStateDigest {
			return errors.New("restored T3 provenance differs")
		}
	}
	return nil
}
func t3Download(c *Context, s *release.T3Artifact, a release.T3Asset, dir string) ([]byte, []byte, error) {
	// gh is restricted to exact owned coordinates; a streaming size bound prevents
	// an arbitrarily large response from filling disk before validation.
	download := func(name string, limit int64) ([]byte, error) {
		// The helper writes at most limit+1 bytes, aborts the owned gh process on
		// overflow, and never prints archive/receipt contents or credentials.
		helper := `import subprocess,sys
p=subprocess.Popen(sys.argv[1:5]+['--repo',sys.argv[9],'--pattern',sys.argv[5],'--output','-'],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
try:
 with open(sys.argv[6],'xb') as f:
  left=int(sys.argv[7])+1
  while left:
   b=p.stdout.read(min(65536,left))
   if not b: break
   f.write(b);left-=len(b)
  if left==0: raise RuntimeError('download limit')
 if p.wait(timeout=30)!=0: raise RuntimeError('download failed')
finally:
 if p.poll() is None: p.kill()
 p.wait()
`
		file := filepath.Join(dir, name)
		r := c.Run([]string{"python3", "-c", helper, "gh", "release", "download", s.Tag, name, file, strconv.FormatInt(limit, 10), "--repo", s.Repository}, 180*time.Second, "")
		// Exact repository is supplied to gh by the bounded helper.
		if !r.OK {
			return nil, errors.New("bounded controlled T3 download failed")
		}
		return release.T3ReadFile(file, limit)
	}
	b, e := download(a.Asset, release.T3ArchiveLimit)
	if e != nil {
		return nil, nil, e
	}
	r, e := download(a.ReceiptAsset, release.T3ReceiptLimit)
	return b, r, e
}

func t3ControlledApply(c *Context, spec, before map[string]any, actions *[]string) (string, error) {
	version := adapterSpecString(spec, "version")
	service := adapterSpecString(spec, "service")
	raw, controlled := spec["artifact"]
	var s *release.T3Artifact
	var err error
	if controlled {
		s, err = release.DecodeT3Artifact(raw, version)
		if err != nil {
			return "", err
		}
		if release.T3Target() == "" {
			return "", Failf("unsupported controlled T3 platform")
		}
	}
	upstream := !controlled && release.VerifyT3Upstream(c.Home, adapterSpecString(before, "npm_prefix"), version) == nil
	if installed, _ := before["artifact_installed"].(bool); (controlled || upstream) && installed {
		served, _ := before["artifact_served"].(bool)
		active, _ := before["service_active"].(bool)
		if served && active && before["check_ok"] == true && before["installed"] == version && before["served"] == version {
			if !c.DryRun {
				if _, _, err = stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
					return "", err
				}
			}
			return "converged", nil
		}
	}
	if (controlled || upstream) && before["artifact_installed"] == true && before["service_active"] == false && !c.DryRun {
		if _, _, err = stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
			return "", err
		}
		fresh, e := t3Observe(c, spec)
		if e != nil {
			return "", e
		}
		if fresh["artifact_installed"] != true {
			return "", Failf("T3 identity changed before seed")
		}
		if !t3Idle(fresh) {
			*actions = append(*actions, "T3 seed deferred: fresh zero-thread proof unavailable")
			return "deferred", nil
		}
		if e = t3VerifyInstalledCLI(c, version); e != nil {
			return "", e
		}
		if !t3BoundaryIdle(c) {
			return "deferred", nil
		}
		seedPrefix := adapterSpecString(fresh, "npm_prefix")
		seeded, e := seedT3Service(c, actions, func() error {
			obs, err := t3Observe(c, spec)
			if err != nil || obs["artifact_installed"] != true || adapterSpecString(obs, "npm_prefix") != seedPrefix {
				return errors.New("T3 installed identity changed before seed start")
			}
			if !t3Idle(obs) || !t3BoundaryIdle(c) {
				return errors.New("T3 seed start idle proof changed")
			}
			_, err = t3UnitBinding(c, service, seedPrefix)
			return err
		})
		if e != nil {
			return "", e
		}
		if seeded {
			after, e := t3Observe(c, spec)
			if e != nil || after["artifact_installed"] != true || after["artifact_served"] != true || after["service_active"] != true || after["check_ok"] != true {
				return "", Failf("seeded T3 identity did not verify")
			}
			return "updated", nil
		}
	}
	if upstream {
		return t3RetainedUpstreamApply(c, spec, actions)
	}
	if c.DryRun {
		*actions = append(*actions, "would verify T3 artifact identity and native runtime; activation and restart require fresh zero-thread proof")
		return "updated", nil
	}
	if _, _, err = stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
		return "", err
	}
	fresh, err := t3Observe(c, spec)
	if err != nil {
		return "", err
	}
	prefix := adapterSpecString(fresh, "npm_prefix")
	if prefix == "" {
		return "", Failf("unknown controlled T3 npm prefix")
	}
	binding, err := t3UnitBinding(c, service, prefix)
	if err != nil {
		return "", err
	}
	checkBinding := func() error {
		current, e := t3UnitBinding(c, service, prefix)
		if e != nil || current != binding {
			return errors.New("T3 service launcher mapping changed or unsupported")
		}
		return nil
	}
	tx, err := t3Begin(c, prefix)
	if err != nil {
		return "", err
	}
	// Retain transaction evidence on failure. Successful installs keep the backup
	// until an operator-reviewed cleanup, so no automatic retry destroys recovery.
	journal := tx.record
	if err = journal("staging"); err != nil {
		return "", fmt.Errorf("T3 staging journal failed; evidence %s: %w", tx.dir, err)
	}
	stage := filepath.Join(tx.dir, "stage")
	if err = os.Mkdir(stage, 0700); err != nil {
		return "", err
	}
	stateBase := filepath.Dir(tx.state)
	if _, e := os.Lstat(tx.state + "-previous"); !os.IsNotExist(e) {
		return "", Failf("T3 recovery backup exists; reconcile before retry")
	}
	stateStage, err := os.MkdirTemp(stateBase, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stateStage)
	node, _ := t3SelectedPrograms(c)
	if node == "" {
		return "", Failf("selected Node unknown")
	}
	if controlled {
		downloadDir := filepath.Join(tx.dir, "downloads")
		if err = os.Mkdir(downloadDir, 0700); err != nil {
			return "", err
		}
		archive, receipt, e := t3Download(c, s, s.Assets[release.T3Target()], downloadDir)
		if e != nil {
			return "", e
		}
		r, e := release.VerifyT3Bundle(archive, receipt, s, release.T3Target(), version, stage)
		if e != nil {
			return "", e
		}
		if e = release.VerifyT3Tree(stage, r, s); e != nil {
			return "", e
		}
		pin, e := json.Marshal(spec)
		if e != nil {
			return "", e
		}
		location, _ := json.Marshal(map[string]any{"prefix": prefix, "link": "../lib/node_modules/t3/bin/t3.js"})
		for name, b := range map[string][]byte{"pin.json": pin, "location.json": location, "archive.tar.gz": archive, "receipt.json": receipt} {
			if e = os.WriteFile(filepath.Join(stateStage, name), b, 0600); e != nil {
				return "", e
			}
		}
	} else {
		// Returning to upstream uses ordinary npm acquisition in a private prefix.
		npmStage := filepath.Join(tx.dir, "npm")
		if _, err = c.Must([]string{"npm", "install", "-g", "--prefix", npmStage, "t3@" + version}, 400*time.Second, "", "stage pinned upstream T3"); err != nil {
			return "", err
		}
		source := filepath.Join(npmStage, "lib/node_modules/t3")
		if _, err = release.T3TreeDigest(source); err != nil {
			return "", err
		}
		if err = os.Remove(stage); err != nil {
			return "", err
		}
		if err = os.Rename(source, stage); err != nil {
			return "", err
		}
		// Bounded first support requires the modern owned launcher; legacy installed
		// packages stay supported by the ordinary handler and as exact backups.
		if _, err = os.Stat(filepath.Join(stage, "bin/t3.js")); err != nil {
			return "", Failf("controlled-to-upstream requires modern owned launcher")
		}
		digest, e := release.T3TreeDigest(stage)
		if e != nil {
			return "", e
		}
		b, _ := json.Marshal(map[string]any{"schema": "upkeeper-t3-upstream/v1", "prefix": prefix, "version": version, "tree_sha256": digest, "link": "../lib/node_modules/t3/bin/t3.js"})
		if err = os.WriteFile(filepath.Join(stateStage, "upstream.json"), b, 0600); err != nil {
			return "", err
		}
	}
	if err = t3QualifyInstalled(c, node, filepath.Join(stage, "bin/t3.js"), version); err != nil {
		return "", err
	}
	fresh, err = t3Observe(c, spec)
	if err != nil {
		return "", err
	}
	if !t3Idle(fresh) {
		*actions = append(*actions, "T3 activation deferred: fresh zero-thread proof unavailable")
		if err = journal("deferred-before-activation"); err != nil {
			return "", fmt.Errorf("T3 files unchanged; deferred journal failed; evidence %s: %w", tx.dir, err)
		}
		return "deferred", nil
	}
	if adapterSpecString(fresh, "npm_prefix") != prefix {
		return "", Failf("T3 prefix changed before activation")
	}
	priorActive, _ := fresh["service_active"].(bool)
	priorServed := fresh["served"]
	priorSpec := map[string]any{"service": service, "version": fresh["installed"]}
	priorUpstream := release.VerifyT3Upstream(c.Home, prefix, adapterSpecString(priorSpec, "version")) == nil
	priorRetained, _ := release.ReadT3Retained(c.Home)
	fail := func(cause error, restarted bool) (string, error) {
		journalErr := journal("restoration-required")
		cause = errors.Join(cause, journalErr)
		obs, e := t3Observe(c, spec)
		if e != nil || !t3Idle(obs) {
			return "", Failf("rollback incomplete: fresh idle proof unavailable; backup %s; cause %v", tx.dir, cause)
		}
		if e = tx.restore(); e != nil {
			return "", Failf("rollback incomplete: %v; backup %s; cause %v", e, tx.dir, cause)
		}
		journalErr = errors.Join(journalErr, journal("files-restored"))
		if journalErr != nil {
			return "", fmt.Errorf("files restored; service restoration unverified; journal persistence failed; backup %s; cause %v: %w", tx.dir, cause, journalErr)
		}
		if restarted {
			// No retry restart without a new independent successful idle observation.
			obs, e = t3Observe(c, spec)
			if e != nil || !t3Idle(obs) {
				return "", Failf("files restored; rollback service idle proof unavailable; backup %s", tx.dir)
			}
			if priorActive {
				if !t3BoundaryIdle(c) {
					return "", Failf("files restored; rollback restart idle proof changed; backup %s", tx.dir)
				}
				if e = checkBinding(); e != nil {
					return "", fmt.Errorf("files restored; rollback service mapping changed; backup %s: %w", tx.dir, e)
				}
				if _, e = c.Must([]string{"systemctl", "--user", "restart", service}, 0, "", "restore idle previous T3"); e != nil {
					return "", Failf("files restored; previous service restoration failed; backup %s", tx.dir)
				}
			} else if c.UnitActive(service) {
				return "", Failf("files restored; service unexpectedly active; no unsafe stop")
			}
		}
		if c.UnitActive(service) != priorActive {
			return "", Failf("files restored; previous service state not verified")
		}
		if priorActive {
			restored, e := t3Observe(c, spec)
			identityOK := true
			if priorUpstream {
				identityOK = t3UpstreamServed(c, priorSpec, prefix)
			}
			if priorRetained != nil {
				identityOK = t3UnitSelectsOwned(c, service, prefix) && t3ServedArtifact(c, priorSpec, priorRetained)
			}
			if e != nil || !identityOK || restored["check_ok"] != true || restored["served"] != priorServed {
				return "", Failf("files restored; previous service health not verified; backup %s", tx.dir)
			}
		}
		if e = journal("restored"); e != nil {
			return "", fmt.Errorf("files and previous service restored; journal persistence failed; backup %s: %w", tx.dir, e)
		}
		var persistence *t3JournalError
		if errors.As(cause, &persistence) {
			return "", fmt.Errorf("files and previous service restored; journal persistence failed; backup %s: %w", tx.dir, cause)
		}
		return "", RolledBackf("restored exact previous T3 bytes, modes, launcher and provenance; cause %v; evidence %s", cause, tx.dir)
	}
	tx.guard = func() error {
		if !t3BoundaryIdle(c) {
			return errT3BoundaryIdle
		}
		return nil
	}
	tx.activationGuard = checkBinding
	if err = tx.activate(stage, stateStage); err != nil {
		if errors.Is(err, errT3BoundaryIdle) && !tx.packageMoved && !tx.packageActivated {
			*actions = append(*actions, "T3 activation deferred: idle changed at file boundary")
			return "deferred", nil
		}
		return fail(err, false)
	}
	if err = journal("activated"); err != nil {
		return fail(fmt.Errorf("activation journal failed: %w", err), false)
	}
	if err = t3ControlledSelected(c, prefix); err != nil {
		return fail(err, false)
	}
	if err = t3VerifyInstalledCLI(c, version); err != nil {
		return fail(err, false)
	}
	fresh, err = t3Observe(c, spec)
	if err != nil {
		return fail(err, false)
	}
	if !t3Idle(fresh) {
		if err = journal("installed-served-split"); err != nil {
			return "", fmt.Errorf("T3 installed; service unchanged; split journal failed; backup %s: %w", tx.dir, err)
		}
		*actions = append(*actions, "T3 installed; restart deferred because idle evidence changed; rollback retained at "+tx.dir)
		return "deferred", nil
	}
	if adapterSpecString(fresh, "npm_prefix") != prefix {
		return fail(errors.New("T3 prefix changed before restart"), false)
	}
	if controlled {
		if ok, _ := fresh["artifact_installed"].(bool); !ok {
			return fail(errors.New("activated T3 identity differs"), false)
		}
	}
	if !t3BoundaryIdle(c) {
		if err = journal("installed-served-split"); err != nil {
			return "", fmt.Errorf("T3 installed; service unchanged; split journal failed; backup %s: %w", tx.dir, err)
		}
		*actions = append(*actions, "T3 installed; restart deferred at restart boundary; backup "+tx.dir)
		return "deferred", nil
	}
	if err = checkBinding(); err != nil {
		return fail(err, false)
	}
	if _, err = c.Must([]string{"systemctl", "--user", "restart", service}, 0, "", "restart idle T3"); err != nil {
		return fail(err, true)
	}
	after, err := t3Observe(c, spec)
	if err != nil {
		return fail(err, true)
	}
	active, _ := after["service_active"].(bool)
	verified := after["installed"] == version && after["served"] == version && active && after["check_ok"] == true
	if controlled {
		i, _ := after["artifact_installed"].(bool)
		r, _ := after["artifact_served"].(bool)
		verified = verified && i && r
	} else {
		verified = verified && t3UpstreamServed(c, spec, prefix)
	}
	if !verified {
		return fail(errors.New("T3 post-restart identity or service verification failed"), true)
	}
	if err = journal("committed"); err != nil {
		return "", fmt.Errorf("T3 installed and serving verified identity; commit journal failed; backup %s: %w", tx.dir, err)
	}
	// Previous state is retained under the transaction, allowing later transitions.
	if tx.stateMoved {
		if err = os.Rename(tx.state+"-previous", filepath.Join(stateBase, "backup-"+filepath.Base(tx.dir))); err != nil {
			return "", fmt.Errorf("T3 updated; retain provenance backup: %w", err)
		}
	}
	*actions = append(*actions, "installed and serving verified T3 identity; exact previous bytes retained at "+tx.dir)
	return "updated", nil
}
