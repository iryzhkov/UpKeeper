package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/rpcprobe"
)

func init() {
	Register(release.ToolingComponent, Handler{Observe: toolingObserve, Apply: toolingApply})
}

var toolingProbe = probeTooling
var toolingModels = rpcprobe.Models
var toolingExchange = rpcprobe.Exchange

func toolingIdentity(spec map[string]any) string {
	b, _ := canonjson.Compact(spec)
	return agentenv.Digest(b)
}

func toolingObserve(c *Context, spec map[string]any) (map[string]any, error) {
	t, err := release.DecodeTooling(spec)
	if err != nil {
		return nil, err
	}
	base := c.Path(".local/share/upkeeper-tools")
	current := filepath.Join(base, "current")
	resolved, _ := filepath.EvalSymlinks(current)
	expected := filepath.Join(base, "releases", toolingIdentity(spec))
	drift := []string{}
	if resolved != expected {
		drift = append(drift, "active tooling bundle differs")
	}
	if err := verifyToolFiles(c, expected, t); err != nil {
		drift = append(drift, err.Error())
	} else if err := toolingProbe(c, expected, t); err != nil {
		drift = append(drift, err.Error())
	}
	for name, target := range toolingLinks(t, base) {
		link, err := os.Readlink(c.Path(".local/bin", name))
		if err != nil || link != target {
			drift = append(drift, "command binding differs: "+name)
		}
	}
	_, retirement, err := toolingBacklogRetirement(c, base, t)
	if err != nil {
		drift = append(drift, "t3-backlog retirement: "+err.Error())
	} else if retirement == "owned" {
		drift = append(drift, "retired command binding remains: t3-backlog")
	}
	versions := map[string]any{}
	for _, tool := range []string{"codex", "claude", "opencode", "node"} {
		result := c.Run([]string{c.Path(".local/share/mise/shims", tool), "--version"}, 15*time.Second, c.Home)
		if !result.OK {
			drift = append(drift, "missing or broken CLI: "+tool)
			versions[tool] = nil
		} else {
			versions[tool] = result.Output
		}
	}
	models, err := toolingModels([]string{c.Path(".local/share/mise/shims/codex"), "app-server", "--stdio"}, c.Env, c.Home)
	if err != nil {
		drift = append(drift, "Codex catalog: "+err.Error())
	}
	sort.Strings(drift)
	return map[string]any{"drift": drift, "versions": versions, "codex_models": models, "bundle": resolved, "desired_bundle": expected, "t3_backlog_binding": retirement}, nil
}

func verifyToolFiles(c *Context, dir string, t *release.Tooling) error {
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err != nil {
		return errors.New("tooling bundle not installed")
	}
	for _, f := range t.Files {
		path := filepath.Join(dir, f.Path)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || int(info.Mode().Perm()) != f.Mode || c.SHA256File(path) != f.SHA256 {
			return fmt.Errorf("tool source differs: %s", f.Path)
		}
	}
	for path, file := range toolingGeneratedFiles(t) {
		target := filepath.Join(dir, path)
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != file.mode || c.SHA256File(target) != agentenv.Digest(file.data) {
			return fmt.Errorf("generated tooling file differs: %s", path)
		}
	}
	return nil
}

type generatedToolFile struct {
	data []byte
	mode os.FileMode
}

func toolingGeneratedFiles(t *release.Tooling) map[string]generatedToolFile {
	files := map[string]generatedToolFile{
		"ov-memory/requirements.lock": {[]byte(strings.Join(t.Python, "\n") + "\n"), 0600},
		".complete":                   {[]byte("staged\n"), 0600},
	}
	for name, body := range map[string]string{
		"ov-memory":      "exec \"$root/venv/bin/python\" \"$root/server.py\" \"$@\"\n",
		"local-file-llm": "exec \"$HOME/.local/share/mise/shims/node\" \"$root/server.mjs\" \"$@\"\n",
	} {
		files[name+"/run"] = generatedToolFile{[]byte("#!/bin/sh\nroot=$(dirname -- \"$(readlink -f -- \"$0\")\")\n" + body), 0755}
	}
	return files
}

const toolingBacklogSource = "homelab-cli/bin/t3-backlog"

func toolingHasBacklog(t *release.Tooling) bool {
	for _, f := range t.Files {
		if f.Path == toolingBacklogSource {
			return true
		}
	}
	return false
}

// The source omission authorizes exactly one known binding, not general bin
// ownership. Lstat/Readlink deliberately inspect the leaf without following it.
func toolingBacklogRetirement(c *Context, base string, t *release.Tooling) (string, string, error) {
	if toolingHasBacklog(t) {
		return "", "desired", nil
	}
	bin, err := agentenv.TargetPath(c.Home, ".local/bin")
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(bin, "t3-backlog")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return path, "absent", nil
	}
	if err != nil {
		return "", "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", "", err
		}
		if target == filepath.Join(base, "current", toolingBacklogSource) {
			return path, "owned", nil
		}
	}
	return path, "preserved foreign binding", nil
}

func toolingLinks(t *release.Tooling, base string) map[string]string {
	links := map[string]string{
		"ov-memory-mcp":      filepath.Join(base, "current/ov-memory/run"),
		"local-file-llm-mcp": filepath.Join(base, "current/local-file-llm/run"),
	}
	for _, f := range t.Files {
		if strings.HasPrefix(f.Path, "homelab-cli/bin/") && f.Mode == 0755 {
			links[filepath.Base(f.Path)] = filepath.Join(base, "current", f.Path)
		}
	}
	return links
}

func toolingApply(c *Context, spec, before map[string]any, actions *[]string) (string, error) {
	t, err := release.DecodeTooling(spec)
	if err != nil {
		return "", err
	}
	if c.DryRun {
		*actions = append(*actions, "would run mise upgrade --yes --no-prune (host version ranges and release-age policy)")
		if drift, _ := before["drift"].([]string); len(drift) > 0 {
			*actions = append(*actions, "would stage, probe and activate the tooling bundle")
			return "updated", nil
		}
		return "converged", nil
	}
	// A rollback restores frozen source bundles, but cannot undo the explicitly
	// floating mise policy. Never upgrade tools while rolling a release back.
	if !c.Payload.AllowDowngrade {
		if _, err := c.Must([]string{"mise", "upgrade", "--yes", "--no-prune"}, 15*time.Minute, c.Home, "mise upgrade"); err != nil {
			return "", err
		}
		*actions = append(*actions, "ran mise upgrade --yes --no-prune using host version ranges")
	}
	for _, tool := range []string{"codex", "claude", "opencode", "node"} {
		if !c.Run([]string{c.Path(".local/share/mise/shims", tool), "--version"}, 15*time.Second, c.Home).OK {
			return "", Failf("required mise tool %s is missing or broken; configure it in the host's global mise tools", tool)
		}
	}
	base, err := agentenv.TargetPath(c.Home, ".local/share/upkeeper-tools")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "releases", toolingIdentity(spec))
	if err := stageTooling(c, dir, t); err != nil {
		return "", err
	}
	if err := toolingProbe(c, dir, t); err != nil {
		return "", err
	}
	if err := activateTooling(c, base, dir, t); err != nil {
		return "", err
	}
	*actions = append(*actions, "verified and activated CLI sources, MCP runtimes and dependency environments")
	after, err := toolingObserve(c, spec)
	if err != nil {
		return "", err
	}
	if drift, _ := after["drift"].([]string); len(drift) > 0 {
		return "", Failf("tooling verification: %s", strings.Join(drift, "; "))
	}
	if reflect.DeepEqual(before, after) {
		return "converged", nil
	}
	return "updated", nil
}

func stageTooling(c *Context, dir string, t *release.Tooling) error {
	// A staged release is only verified. The CLI check is for staging, as the
	// preflight's staging-only prerequisites are.
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err == nil {
		return verifyToolFiles(c, dir, t)
	}
	if _, err := c.Must([]string{"bash", "-c", "command -v bash curl jq python3 ssh readlink secret-tool >/dev/null"}, 15*time.Second, c.Home, "CLI prerequisites"); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		if err := setAsideOwnFailedStage(c, dir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	if err := claimToolingStage(c, dir); err != nil {
		return err
	}
	// Failed staging directories remain inspectable, never become current, and
	// never cause an existing source checkout or virtualenv to be overwritten.
	for _, f := range t.Files {
		data, _ := base64.StdEncoding.DecodeString(f.Content)
		if err := agentenv.AtomicWrite(filepath.Join(dir, f.Path), data, os.FileMode(f.Mode)); err != nil {
			return err
		}
	}
	lock := filepath.Join(dir, "ov-memory/requirements.lock")
	if err := agentenv.AtomicWrite(lock, []byte(strings.Join(t.Python, "\n")+"\n"), 0600); err != nil {
		return err
	}
	venv := filepath.Join(dir, "ov-memory/venv")
	if _, err := c.Must([]string{"python3", "-m", "venv", venv}, 2*time.Minute, c.Home, "OV memory virtualenv"); err != nil {
		return err
	}
	if _, err := c.Must([]string{filepath.Join(venv, "bin/python"), "-m", "pip", "install", "--disable-pip-version-check", "--only-binary=:all:", "-r", lock}, 10*time.Minute, c.Home, "OV memory dependencies"); err != nil {
		return err
	}
	if _, err := c.Must([]string{"npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"}, 5*time.Minute, filepath.Join(dir, "local-file-llm"), "local-file-llm dependencies"); err != nil {
		return err
	}
	// Wrappers find their immutable bundle through readlink, so a running process
	// never crosses from one release's script to another release's library.
	for path, file := range toolingGeneratedFiles(t) {
		if path == ".complete" {
			continue
		}
		if err := agentenv.AtomicWrite(filepath.Join(dir, path), file.data, file.mode); err != nil {
			return err
		}
	}
	return agentenv.AtomicWrite(filepath.Join(dir, ".complete"), []byte("staged\n"), 0600)
}

// toolingStageMarker records in a stage directory that UpKeeper created it,
// for which release and when. It is written before anything else.
const toolingStageMarker = ".upkeeper-stage"

// claimToolingStage creates the stage directory with its ownership marker
// already inside: the marker is written into a private directory that is then
// renamed into place, so no crash can leave a stage at dir that does not
// prove it is UpKeeper's own.
func claimToolingStage(c *Context, dir string) error {
	marker, err := json.Marshal(toolingStageOwner{
		CreatedBy: "upkeeper", Identity: filepath.Base(dir), RunID: c.RunID,
		StartedAt: c.now().UTC().Format(toolingStageTime),
	})
	if err != nil {
		return err
	}
	private, err := os.MkdirTemp(filepath.Dir(dir), ".stage-")
	if err != nil {
		return err
	}
	if err := os.Chmod(private, 0700); err != nil {
		_ = os.RemoveAll(private)
		return err
	}
	if err := agentenv.AtomicWrite(filepath.Join(private, toolingStageMarker), marker, 0600); err != nil {
		_ = os.RemoveAll(private)
		return err
	}
	if err := fsatomic.RenameNoReplace(private, dir); err != nil {
		_ = os.RemoveAll(private)
		return err
	}
	return nil
}

// toolingFailedStageLimit is how many failed stages are kept for diagnosis.
const toolingFailedStageLimit = 3

// toolingStageTime names failed stages so that they sort by when they began.
const toolingStageTime = "20060102T150405Z"

type toolingStageOwner struct {
	CreatedBy string `json:"created_by"`
	Identity  string `json:"identity"`
	RunID     string `json:"run_id"`
	StartedAt string `json:"started_at"`
}

// setAsideOwnFailedStage moves an incomplete stage of this same release that
// UpKeeper itself created into failed-stages, beside releases, where it stays
// for diagnosis but can never become current, and keeps only the newest few.
// A stage it cannot prove it created is somebody else's, and is refused.
func setAsideOwnFailedStage(c *Context, dir string) error {
	refuse := func() error {
		return Failf("incomplete tooling stage of unknown origin preserved at %s; inspect it, "+
			"then move it aside before retrying", dir)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return refuse()
	}
	path := filepath.Join(dir, toolingStageMarker)
	if markerInfo, err := os.Lstat(path); err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Size() > 4096 {
		return refuse()
	}
	// Read without blocking, so a marker swapped for a FIFO cannot hang.
	data, ok := readSmallRegularFile(path, 4096)
	if !ok {
		return refuse()
	}
	var owner toolingStageOwner
	if json.Unmarshal(data, &owner) != nil || owner.CreatedBy != "upkeeper" ||
		owner.Identity != filepath.Base(dir) {
		return refuse()
	}
	started, err := time.Parse(toolingStageTime, owner.StartedAt)
	if err != nil {
		started = c.now().UTC()
	}
	failed := filepath.Join(filepath.Dir(filepath.Dir(dir)), "failed-stages")
	if err := os.MkdirAll(failed, 0700); err != nil {
		return err
	}
	// Stages are moved into, and pruned from, a directory UpKeeper made. A
	// symlink there would send both somewhere else entirely.
	if info, err := os.Lstat(failed); err != nil || !info.IsDir() {
		return Failf("%s is not a directory UpKeeper made; preserved %s", failed, dir)
	}
	name := started.UTC().Format(toolingStageTime) + "-" + shortIdentity(owner.Identity)
	target := filepath.Join(failed, name)
	for n := 2; pathExists(target); n++ {
		if n > 999 {
			return Failf("too many failed stages named %s; preserved %s", name, dir)
		}
		target = filepath.Join(failed, fmt.Sprintf("%s-%03d", name, n))
	}
	if err := fsatomic.RenameNoReplace(dir, target); err != nil {
		return err
	}
	return pruneFailedStages(failed)
}

func shortIdentity(identity string) string {
	if len(identity) > 12 {
		return identity[:12]
	}
	return identity
}

// failedStageName is the name setAsideOwnFailedStage gives a failed stage:
// when the attempt began, the release identity, and a counter for attempts
// that began in the same second. Such names sort by time.
var failedStageName = regexp.MustCompile(`^([0-9]{8}T[0-9]{6}Z)-([0-9a-f]{1,12})(-[0-9]{3})?$`)

// pruneFailedStages keeps the newest toolingFailedStageLimit failed stages.
// Only a directory setAsideOwnFailedStage moved there counts, recognised by
// its name and its marker; anything else in the directory is left alone.
func pruneFailedStages(failed string) error {
	entries, err := os.ReadDir(failed)
	if err != nil {
		return err
	}
	names := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// A stage a pruning was interrupted in is finished first. It still has
		// its marker, which is always the last thing removed.
		if name, found := strings.CutPrefix(entry.Name(), failedStagePruning); found {
			if ownFailedStage(filepath.Join(failed, entry.Name()), name) {
				if err := removeMarkerLast(filepath.Join(failed, entry.Name())); err != nil {
					return err
				}
			}
			continue
		}
		if ownFailedStage(filepath.Join(failed, entry.Name()), entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for len(names) > toolingFailedStageLimit {
		if err := removeFailedStage(failed, names[0]); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

// failedStagePruning prefixes the name of a failed stage while it is removed.
const failedStagePruning = ".pruning-"

// removeFailedStage removes one validated failed stage. The stage is first
// renamed to a private name and validated again there, so a directory that
// something else put at its name after it was validated is renamed back
// rather than removed.
func removeFailedStage(failed, name string) error {
	path := filepath.Join(failed, name)
	pruning := filepath.Join(failed, failedStagePruning+name)
	if err := fsatomic.RenameNoReplace(path, pruning); err != nil {
		// Gone already, or a directory of unknown origin holds the private
		// name: either way there is nothing this prune may remove.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	if !ownFailedStage(pruning, name) {
		// Not the stage that was validated. If its name has been taken again
		// meanwhile, it stays under the private name, which is never removed
		// without a valid marker either.
		_ = fsatomic.RenameNoReplace(pruning, path)
		return nil
	}
	return removeMarkerLast(pruning)
}

// removeMarkerLast removes a stage directory with its ownership marker last,
// so a removal that stops part way leaves a directory a later prune still
// recognises and finishes.
func removeMarkerLast(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == toolingStageMarker {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, toolingStageMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Remove(dir)
}

// ownFailedStage reports whether dir is a stage setAsideOwnFailedStage moved
// into failed-stages under name: its marker is a small regular file that says
// UpKeeper created it, for the release identity name abbreviates, and, when
// the marker records when it began, at the time name gives. A marker that
// cannot be read, does not parse, or says anything else makes the directory
// unknown, and an unknown directory is never counted or deleted.
func ownFailedStage(dir, name string) bool {
	parts := failedStageName.FindStringSubmatch(name)
	if parts == nil {
		return false
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return false
	}
	path := filepath.Join(dir, toolingStageMarker)
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return false
	}
	data, ok := readSmallRegularFile(path, 4096)
	if !ok {
		return false
	}
	var owner toolingStageOwner
	if json.Unmarshal(data, &owner) != nil || owner.CreatedBy != "upkeeper" ||
		owner.Identity == "" || shortIdentity(owner.Identity) != parts[2] {
		return false
	}
	if started, err := time.Parse(toolingStageTime, owner.StartedAt); err == nil &&
		started.UTC().Format(toolingStageTime) != parts[1] {
		return false
	}
	return true
}

func probeTooling(c *Context, dir string, t *release.Tooling) error {
	// CLI help never invokes a service action or requires copying credentials.
	for _, name := range []string{"ha", "toolfeedback", "feed", "pve", "unifi", "t3-backlog"} {
		if name == "t3-backlog" && !toolingHasBacklog(t) {
			continue
		}
		if _, err := c.Must([]string{filepath.Join(dir, "homelab-cli/bin", name), "--help"}, 15*time.Second, c.Home, name+" help"); err != nil {
			return err
		}
	}
	if _, err := c.Must([]string{filepath.Join(dir, "ov-memory/venv/bin/python"), "-m", "pip", "check"}, 30*time.Second, c.Home, "OV dependency check"); err != nil {
		return err
	}
	frozen := c.Run([]string{filepath.Join(dir, "ov-memory/venv/bin/python"), "-m", "pip", "freeze"}, 30*time.Second, c.Home)
	actual := strings.Split(strings.TrimSpace(frozen.Output), "\n")
	sort.Strings(actual)
	if !frozen.OK || !reflect.DeepEqual(actual, t.Python) {
		return Failf("OV Python installed versions differ from release lock")
	}
	for _, name := range []string{"ov-memory", "local-file-llm"} {
		env, err := agentenv.MCPEnvironment(c.Spec("agent-environment"), c.Home, name, c.Env)
		if err != nil {
			return Failf("%s environment could not be rendered", name)
		}
		calls := []rpcprobe.Call{{Method: "tools/list", Params: map[string]any{}}}
		if name == "ov-memory" {
			calls = append(calls, rpcprobe.Call{Method: "tools/call", Params: map[string]any{"name": "memory_status", "arguments": map[string]any{}}})
		} else {
			calls = append(calls, rpcprobe.Call{Method: "tools/call", Params: map[string]any{"name": "local_status", "arguments": map[string]any{}}})
		}
		results, err := toolingExchange([]string{filepath.Join(dir, name, "run")}, env, c.Home, false, calls, 40*time.Second)
		if err != nil {
			return Failf("%s health check: %s", name, err)
		}
		tools, ok := results[0]["tools"].([]any)
		if !ok || len(tools) == 0 {
			return Failf("%s returned no tools", name)
		}
	}
	// Discovery checks the client/account, never a frozen model allowlist.
	if _, err := toolingModels([]string{c.Path(".local/share/mise/shims/codex"), "app-server", "--stdio"}, c.Env, c.Home); err != nil {
		return Failf("Codex model discovery: %s", err)
	}
	return nil
}

type linkSnapshot struct {
	Path   string `json:"path"`
	Target string `json:"target,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Exists bool   `json:"exists"`
}

func snapshotLink(path string) (linkSnapshot, error) {
	s := linkSnapshot{Path: path}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Exists = true
	s.Mode = uint32(info.Mode())
	if info.Mode()&os.ModeSymlink != 0 {
		s.Target, err = os.Readlink(path)
	} else if info.Mode().IsRegular() {
		s.Data, err = os.ReadFile(path)
	} else {
		err = errors.New("tool binding is not a file or symlink")
	}
	return s, err
}
func swapLink(path, target string) error {
	temp := path + ".upkeeper-link"
	if err := os.Symlink(target, temp); err != nil {
		return err
	}
	defer os.Remove(temp)
	return os.Rename(temp, path)
}
func activateTooling(c *Context, base, dir string, t *release.Tooling) error {
	bin, err := agentenv.TargetPath(c.Home, ".local/bin")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(bin, 0755); err != nil {
		return err
	}
	links := map[string]string{filepath.Join(base, "current"): dir}
	for name, target := range toolingLinks(t, base) {
		links[filepath.Join(bin, name)] = target
	}
	retiredPath, retirement, err := toolingBacklogRetirement(c, base, t)
	if err != nil {
		return err
	}
	// Empty target is a narrow retirement operation, covered by the same
	// private snapshot and compensation scope as current and desired links.
	if retirement == "owned" {
		links[retiredPath] = ""
	}
	keys := make([]string, 0, len(links))
	for path := range links {
		keys = append(keys, path)
	}
	sort.Strings(keys)
	prior := []linkSnapshot{}
	for _, path := range keys {
		s, err := snapshotLink(path)
		if err != nil {
			return err
		}
		if path == retiredPath && retirement == "owned" &&
			(s.Mode&uint32(os.ModeSymlink) == 0 || s.Target != filepath.Join(base, "current", toolingBacklogSource)) {
			return Failf("concurrent t3-backlog binding change; preserved %s", path)
		}
		prior = append(prior, s)
	}
	encoded, _ := json.MarshalIndent(prior, "", "  ")
	backup := c.Path(".local/state/dev-fleet/tooling", c.RunID, "bindings.json")
	if err := agentenv.AtomicWrite(backup, encoded, 0600); err != nil {
		return err
	}
	changed := []int{}
	restore := func(cause error) error {
		for n := len(changed) - 1; n >= 0; n-- {
			i := changed[n]
			s := prior[i]
			now, err := snapshotLink(s.Path)
			expected := linkSnapshot{Path: s.Path}
			if links[s.Path] != "" {
				expected.Exists = true
				expected.Mode = uint32(os.ModeSymlink | 0777)
				expected.Target = links[s.Path]
			}
			if err != nil || !reflect.DeepEqual(now, expected) {
				return Failf("rollback failed: concurrent change to %s; prior bindings at %s", s.Path, backup)
			}
			if s.Target != "" {
				err = swapLink(s.Path, s.Target)
			} else if s.Exists {
				err = agentenv.AtomicWrite(s.Path, s.Data, os.FileMode(s.Mode).Perm())
			} else {
				err = os.Remove(s.Path)
			}
			if err != nil {
				return Failf("rollback failed restoring tooling bindings: %s", err)
			}
		}
		return RolledBackf("tooling activation failed; restored prior bindings: %s", cause)
	}
	for i, s := range prior {
		now, err := snapshotLink(s.Path)
		if err != nil || !reflect.DeepEqual(now, s) {
			return restore(errors.New("concurrent tooling binding change"))
		}
		if s.Target == links[s.Path] {
			continue
		}
		if links[s.Path] == "" {
			err = os.Remove(s.Path)
		} else {
			err = swapLink(s.Path, links[s.Path])
		}
		if err != nil {
			return restore(err)
		}
		changed = append(changed, i)
	}
	// Verify the active bindings before discarding the compensation scope.
	if err := toolingProbe(c, filepath.Join(base, "current"), t); err != nil {
		return restore(err)
	}
	for path, target := range links {
		if target == "" {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				return restore(errors.New("retired tool binding did not verify"))
			}
			continue
		}
		actual, err := os.Readlink(path)
		if err != nil || actual != target {
			return restore(errors.New("tool binding did not verify"))
		}
	}
	return nil
}
