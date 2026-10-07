package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func init() {
	Register("t3-steward", Handler{Observe: stewardObserve, Apply: stewardApply})
}

const (
	// stewardWorkerUnit runs execution attempts for a coordinator. Installing a
	// binary does not change the process that is already running it.
	stewardWorkerUnit = "t3-steward-worker.service"
	// stewardArchiveLimit is the largest release archive the agent will read.
	stewardArchiveLimit = 128 * 1024 * 1024
	// stewardHookLimit is the largest session hook the agent will install.
	stewardHookLimit = 64 * 1024
	// stewardBinaryMember and stewardHookMember are the two archive members a
	// pinned install takes; nothing else in the archive is read.
	stewardBinaryMember = "t3-steward"
	stewardHookMember   = "packaging/opencode/steward-session.js"
)

var (
	stewardRangePattern   = regexp.MustCompile(`tested with T3 (\d+\.\d+\.\d+)\.\.(\d+\.\d+\.\d+)`)
	stewardVersionPattern = regexp.MustCompile(`^t3-steward v?(\S+)`)
	stewardNumericPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
)

// stewardFetch downloads one release asset. It is a package variable so that a
// test can exercise the install without reaching the network.
var stewardFetch = stewardHTTPFetch

// stewardHTTPFetch reads at most limit bytes of a URL under a bounded timeout.
func stewardHTTPFetch(url string, timeout time.Duration, limit int64) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	response, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d for %s", response.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(response.Body, limit))
}

// stewardArtifactError names the Python exception class a refusal used to carry,
// because the controller prints "steward artifact fetch failed: <name>".
type stewardArtifactError struct {
	kind  string
	cause error
}

func (e *stewardArtifactError) Error() string { return e.kind }
func (e *stewardArtifactError) Unwrap() error { return e.cause }

func stewardArtifactFailure(kind string, cause error) error {
	return &stewardArtifactError{kind: kind, cause: cause}
}

// stewardVersionTuple orders a steward version the way semantic versioning
// does: a prerelease sorts below the final version it leads to.
func stewardVersionTuple(value string) ([]int, int, string) {
	core, prerelease, _ := strings.Cut(value, "-")
	numbers := []int{}
	for _, part := range strings.Split(core, ".") {
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, 0, prerelease
		}
		numbers = append(numbers, number)
	}
	final := 1
	if prerelease != "" {
		final = 0
	}
	return numbers, final, prerelease
}

// stewardPairRange reads the T3 range a steward binary says it was tested with.
func stewardPairRange(output string) (string, string, bool) {
	match := stewardRangePattern.FindStringSubmatch(output)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

// stewardPairVersion reads the version a steward binary reports for itself.
func stewardPairVersion(output string) string {
	match := stewardVersionPattern.FindStringSubmatch(output)
	if match == nil {
		return ""
	}
	return match[1]
}

// stewardNumericVersion parses a stable numeric T3 version and refuses anything
// else, because a prerelease cannot be placed inside a frozen range.
func stewardNumericVersion(value string) ([]int, error) {
	if !stewardNumericPattern.MatchString(value) {
		return nil, Failf("T3 version is not a stable numeric version")
	}
	numbers := make([]int, 0, 3)
	for _, part := range strings.Split(value, ".") {
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, Failf("T3 version is not a stable numeric version")
		}
		numbers = append(numbers, number)
	}
	return numbers, nil
}

func stewardCompareVersions(left, right []int) int {
	for index := range left {
		if index >= len(right) {
			return 1
		}
		if left[index] != right[index] {
			if left[index] < right[index] {
				return -1
			}
			return 1
		}
	}
	if len(left) < len(right) {
		return -1
	}
	return 0
}

// stewardVerifyPairBinary makes a steward binary prove, by running it, that it
// is the pinned version and that the release's T3 sits inside the range it was
// tested with. It is used on the candidate before anything is replaced and on
// the installed binary afterwards.
func stewardVerifyPairBinary(c *Context, binary string) (string, string, error) {
	observed := c.Run([]string{binary, "version"}, 15*time.Second, "")
	low, high, bounded := stewardPairRange(observed.Output)
	pinned := adapterSpecString(c.Spec("t3-steward"), "version")
	if !observed.OK || !bounded || stewardPairVersion(observed.Output) != pinned {
		return "", "", Failf(
			"steward binary does not prove the pinned version and T3 compatibility")
	}
	if !c.Has("t3") {
		return low, high, nil
	}
	spec := c.Spec("t3")
	wanted := adapterSpecMap(spec, "compatibility")
	if low != adapterSpecString(wanted, "min") || high != adapterSpecString(wanted, "max") {
		return "", "", Failf(
			"steward binary does not prove the frozen T3 compatibility range")
	}
	lowest, err := stewardNumericVersion(low)
	if err != nil {
		return "", "", err
	}
	served, err := stewardNumericVersion(adapterSpecString(spec, "version"))
	if err != nil {
		return "", "", err
	}
	highest, err := stewardNumericVersion(high)
	if err != nil {
		return "", "", err
	}
	if stewardCompareVersions(lowest, served) > 0 || stewardCompareVersions(served, highest) > 0 {
		return "", "", Failf(
			"steward binary does not prove the frozen T3 compatibility range")
	}
	return low, high, nil
}

// stewardSessionHook is the OpenCode caller hook. Only an explicitly pinned
// hook is managed; a legacy release leaves whatever the host has alone.
func stewardSessionHook(c *Context) string {
	return c.Path(".config/opencode/plugins/steward-session.js")
}

func stewardSessionHookConverged(c *Context, spec map[string]any) bool {
	expected := adapterSpecString(spec, "session_hook_sha256")
	if expected == "" {
		return true
	}
	path := stewardSessionHook(c)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false
	}
	return c.SHA256File(path) == expected
}

// stewardArchiveMembers reads the binary and, when the release pins one, the
// session hook out of a release archive.
func stewardArchiveMembers(payload []byte, spec map[string]any) ([]byte, []byte, error) {
	var stream io.Reader = bytes.NewReader(payload)
	if decompressed, err := gzip.NewReader(bytes.NewReader(payload)); err == nil {
		defer decompressed.Close()
		stream = decompressed
	}
	wantHook := adapterSpecString(spec, "session_hook_sha256") != ""
	var binary, hook []byte
	var binaryFound, hookFound bool
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, stewardArtifactFailure("ReadError", err)
		}
		switch header.Name {
		case stewardBinaryMember:
			binaryFound = true
			if header.Typeflag != tar.TypeReg || header.Size > stewardArchiveLimit {
				return nil, nil, Failf("invalid steward archive member")
			}
			body, err := io.ReadAll(io.LimitReader(reader, stewardArchiveLimit))
			if err != nil {
				return nil, nil, stewardArtifactFailure("ReadError", err)
			}
			binary = body
		case stewardHookMember:
			if !wantHook {
				continue
			}
			hookFound = true
			if header.Typeflag != tar.TypeReg || header.Size > stewardHookLimit {
				return nil, nil, Failf("invalid steward session hook member")
			}
			body, err := io.ReadAll(io.LimitReader(reader, stewardHookLimit))
			if err != nil {
				return nil, nil, stewardArtifactFailure("ReadError", err)
			}
			hook = body
		}
	}
	if !binaryFound || (wantHook && !hookFound) {
		// A missing member was a KeyError on the Python side, which the caller
		// reported as an artifact fetch failure rather than as a bad member.
		return nil, nil, stewardArtifactFailure("KeyError", nil)
	}
	if wantHook {
		sum := sha256.Sum256(hook)
		if hex.EncodeToString(sum[:]) != adapterSpecString(spec, "session_hook_sha256") {
			return nil, nil, Failf("steward session hook checksum mismatch")
		}
	}
	return binary, hook, nil
}

// stewardStage downloads, checksums and unpacks the pinned release into a
// private staging directory, then proves both that the candidate binary is the
// pinned pair and that the running T3 is inside the range that candidate
// supports. Nothing on the host has changed when this returns.
func stewardStage(
	c *Context, spec map[string]any, stage string,
) (string, string, error) {
	architecture := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	system := runtime.GOOS
	if architecture == "" || (system != "linux" && system != "darwin") {
		return "", "", Failf("no steward release artifact for this platform")
	}
	version := adapterSpecString(spec, "version")
	archive := fmt.Sprintf("t3-steward_%s_%s_%s.tar.gz", version, system, architecture)
	base := fmt.Sprintf("https://github.com/%s/releases/download/v%s",
		adapterSpecString(spec, "repository"), version)

	checksums, err := stewardFetch(base+"/checksums.txt", 60*time.Second, 1024*1024)
	if err != nil {
		return "", "", stewardArtifactFailure("URLError", err)
	}
	expected := []string{}
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimLeft(fields[1], "*") == archive {
			expected = append(expected, fields[0])
		}
	}
	if len(expected) != 1 {
		return "", "", Failf("release checksum entry missing or ambiguous")
	}
	payload, err := stewardFetch(base+"/"+archive, 120*time.Second, stewardArchiveLimit+1)
	if err != nil {
		return "", "", stewardArtifactFailure("URLError", err)
	}
	sum := sha256.Sum256(payload)
	if len(payload) > stewardArchiveLimit || hex.EncodeToString(sum[:]) != expected[0] {
		return "", "", Failf("steward release archive checksum mismatch")
	}
	binaryBody, hookBody, err := stewardArchiveMembers(payload, spec)
	if err != nil {
		return "", "", err
	}
	candidate := filepath.Join(stage, "t3-steward")
	if err := os.WriteFile(candidate, binaryBody, 0o755); err != nil {
		return "", "", stewardArtifactFailure("OSError", err)
	}
	if err := os.Chmod(candidate, 0o755); err != nil {
		return "", "", stewardArtifactFailure("OSError", err)
	}
	hookCandidate := ""
	if hookBody != nil {
		hookCandidate = filepath.Join(stage, "steward-session.js")
		if err := os.WriteFile(hookCandidate, hookBody, 0o644); err != nil {
			return "", "", stewardArtifactFailure("OSError", err)
		}
		if err := os.Chmod(hookCandidate, 0o644); err != nil {
			return "", "", stewardArtifactFailure("OSError", err)
		}
	}
	low, high, err := stewardVerifyPairBinary(c, candidate)
	if err != nil {
		return "", "", err
	}
	t3Spec := c.Spec("t3")
	if t3Spec == nil {
		t3Spec = map[string]any{"service": "t3code.service"}
	}
	// The running T3 is observed through a steward. A host that has none yet
	// is observed through the verified candidate instead, which proves the same
	// thing about the same server.
	if _, err := os.Stat(c.Path(adapterSpecString(spec, "path"))); err != nil {
		c.stewardCheckBinary = candidate
		defer func() { c.stewardCheckBinary = "" }()
	}
	current, err := t3Observe(c, t3Spec)
	if err != nil {
		return "", "", err
	}
	// No T3 at all is a safe transition: there is no running server for the
	// steward to be incompatible with, the candidate has just proved that the
	// release's own T3 is inside its range, and the t3 component installs that
	// T3 after this one. Without this a fresh host could never converge.
	if t3NotInstalled(c, t3Spec, current) {
		return candidate, hookCandidate, nil
	}
	if err := stewardRunningT3InRange(
		current, low, high, adapterSpecString(t3Spec, "version")); err != nil {
		return "", "", err
	}
	return candidate, hookCandidate, nil
}

// t3NotInstalled reports whether an observation proves that the host has no
// T3 whatsoever: no package, no controlled installation, nothing serving, no
// active service, and no unit systemd knows or the user directory holds. Any
// trace of T3, or an answer that cannot be had, is not "not installed".
func t3NotInstalled(c *Context, t3Spec, observed map[string]any) bool {
	if observed["installed"] != nil || observed["served"] != nil {
		return false
	}
	if active, _ := observed["service_active"].(bool); active {
		return false
	}
	if checkOK, _ := observed["check_ok"].(bool); checkOK {
		return false
	}
	if managed, _ := observed["artifact_managed"].(bool); managed {
		return false
	}
	if observed["installed_path"] != nil || observed["running_file_count"] != nil {
		return false
	}
	service := adapterSpecString(t3Spec, "service")
	if service == "" {
		service = "t3code.service"
	}
	if pathExists(c.Path(userUnitDir, service)) {
		return false
	}
	status, err := unitStatus(c, service)
	if err != nil || status["LoadState"] != "not-found" {
		return false
	}
	return t3PackageAbsent(c)
}

// t3PackageAbsent reports whether npm itself says that no global t3 package
// is installed. The observation leaves its package unset both when npm says
// so and when npm could not answer at all, so absence is asked again here and
// only a JSON object npm produced on its own terms, without an error and
// without a t3 entry, counts. npm exits 1 when the package it was asked about
// is not installed. A missing npm, a timeout, a permission error or output
// that does not parse leaves the package unknown, which is not absent. npm
// also leaves out a package it cannot read, without saying so, so its empty
// answer counts only once the package's own directory is proved missing.
func t3PackageAbsent(c *Context) bool {
	result := c.Runner(procx.Command{
		Argv: []string{"npm", "ls", "-g", "--depth=0", "--json", "t3"},
		Env:  c.Env, Timeout: 30 * time.Second,
	})
	if result.Err != nil || result.TimedOut {
		return false
	}
	var document map[string]json.RawMessage
	if json.Unmarshal([]byte(result.Stdout), &document) != nil || document == nil {
		return false
	}
	if raw, failed := document["error"]; failed {
		// npm cannot list a global prefix whose lib directory does not exist
		// yet. Such a prefix holds no package at all, which is proved by
		// looking at it, never by the wording of the error. A t3 command
		// linked under its bin directory is still a trace of T3.
		var report struct {
			Code string `json:"code"`
		}
		return json.Unmarshal(raw, &report) == nil && report.Code == "ENOENT" && t3GlobalPathsMissing(c, "lib", "bin/t3")
	}
	if result.Code != 0 && result.Code != 1 {
		return false
	}
	if raw, listed := document["dependencies"]; listed {
		var dependencies map[string]json.RawMessage
		if json.Unmarshal(raw, &dependencies) != nil || dependencies == nil {
			return false
		}
		if _, present := dependencies["t3"]; present {
			return false
		}
	}
	// The command npm links for the package is a trace of T3 too, even when
	// the package it points at lives in another tree.
	return t3GlobalPathsMissing(c, "lib/node_modules/t3", "bin/t3")
}

// t3GlobalPathsMissing reports whether npm's global prefix is an absolute path
// and every path below it certainly does not exist. Only "no such file"
// counts: a directory on the way that cannot be searched answers "permission
// denied", which says nothing about what it holds.
func t3GlobalPathsMissing(c *Context, below ...string) bool {
	result := c.Runner(procx.Command{Argv: []string{"npm", "prefix", "-g"}, Env: c.Env, Timeout: 30 * time.Second})
	if result.Err != nil || result.TimedOut || result.Code != 0 {
		return false
	}
	// Only npm's line ending is removed: a space may be part of the name, and
	// cleaning ".." away lexically would look past a symlink into another tree.
	prefix := strings.TrimSuffix(strings.TrimSuffix(result.Stdout, "\n"), "\r")
	if !filepath.IsAbs(prefix) || strings.ContainsAny(prefix, "\n\r") || filepath.Clean(prefix) != prefix {
		return false
	}
	for _, path := range below {
		if _, err := os.Lstat(filepath.Join(prefix, path)); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

// stewardUnitPresent reports whether the host has a steward service unit at
// all. Only systemd saying it has no file for the unit counts as absent; any
// other failure, such as an unreachable user bus, counts as present, so the
// install restarts and fails loudly rather than skipping the restart.
func stewardUnitPresent(c *Context, service string) bool {
	if pathExists(c.Path(userUnitDir, service)) {
		return true
	}
	result := c.Run([]string{"systemctl", "--user", "cat", service}, 10*time.Second, "")
	return result.OK || !strings.Contains(result.Output, "No files found for "+service)
}

// stewardRunningT3InRange refuses a pair transition the running T3 cannot take.
//
// The precondition is unchanged from the original: a served version inside
// the candidate's range proceeds. What changed is what a refusal says. An
// observation that could not be made is reported as such, because "outside
// the range" and "could not be read" call for different remedies; and when the
// version could not be read because the program behind the T3 unit has been
// deleted, that is named outright, since the steward's check cannot tell it
// from a server that is merely down. A missing program under a server that
// still answers is a warning in the observation, not a refusal.
func stewardRunningT3InRange(current map[string]any, low, high, pinned string) error {
	service, _ := current["service"].(string)
	if service == "" {
		service = "t3code.service"
	}
	unobserved := func(cause string) error {
		if exists, known := current["installed_path_exists"].(bool); known && !exists {
			path, _ := current["installed_path"].(string)
			installed := pinned
			if installed == "" {
				installed = "an unknown version"
			}
			return Failf("running T3 binary missing on disk: %s (installed %s from the manifest); "+
				"restart T3 after reinstalling, then retry", path, installed)
		}
		return Failf("running T3 version could not be observed (%s); no safe pair transition. "+
			"Check that T3 is serving (systemctl --user status %s) and that the steward "+
			"reaches it, then retry", cause, service)
	}
	checkOK, _ := current["check_ok"].(bool)
	if !checkOK {
		cause, _ := current["check_error"].(string)
		if cause == "" {
			cause = "no output"
		}
		return unobserved("t3-steward check failed: " + cause)
	}
	served, _ := current["served"].(string)
	if served == "" {
		return unobserved("t3-steward check reported no T3 server version")
	}
	lowest, err := stewardNumericVersion(low)
	if err != nil {
		return err
	}
	running, err := stewardNumericVersion(served)
	if err != nil {
		return err
	}
	highest, err := stewardNumericVersion(high)
	if err != nil {
		return err
	}
	if stewardCompareVersions(lowest, running) > 0 ||
		stewardCompareVersions(running, highest) > 0 {
		return Failf("running T3 %s is outside the candidate steward's T3 range %s..%s; "+
			"no safe pair transition. Install a T3 inside that range or publish a steward "+
			"tested with T3 %s, then retry", served, low, high, served)
	}
	return nil
}

func stewardObserve(c *Context, spec map[string]any) (map[string]any, error) {
	binary := c.Path(adapterSpecString(spec, "path"))
	result := c.Run([]string{binary, "version"}, 15*time.Second, "")
	observed := map[string]any{
		"version":        nil,
		"service_active": c.UnitActive(adapterSpecString(spec, "service")),
		"binary_sha256":  nil,
	}
	if result.OK {
		if words := strings.Fields(result.Output); len(words) > 1 {
			observed["version"] = words[1]
		}
	}
	if digest := c.SHA256File(binary); digest != "" {
		observed["binary_sha256"] = digest
	}
	found := stewardWorkerRestartPendingSince(c, binary)
	observed["worker_restart_pending"] = found.pending
	observed["drift"] = []string{}
	if found.pending {
		observed["drift"] = []string{"worker restart pending"}
		// A host that is never idle keeps the restart waiting indefinitely;
		// saying since when, and on what, is what lets an operator act.
		waiting := stewardWorkerRestartWaiting(c, binary, stewardPendingSince(c, found))
		observed["worker_restart_pending_since"] = waiting["since"]
		observed["worker_restart_waiting"] = waiting
	}
	return observed, nil
}

// stewardWorkerExecutable reports the path a process was started from, without
// the " (deleted)" suffix the kernel adds once that file has been replaced, and
// the identity of the file the process is actually executing. It is a package
// variable so that a test can stand in for /proc.
var stewardWorkerExecutable = func(pid int) (string, os.FileInfo, error) {
	executable := filepath.Join("/proc", strconv.Itoa(pid), "exe")
	link, err := os.Readlink(executable)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(executable)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSuffix(link, " (deleted)"), info, nil
}

// stewardWorkerRestartPendingSince reports whether the running persistent
// worker predates the installed binary: it was started from the installed
// path, and the file it executes is no longer the file at that path.
//
// A restart deferred because the worker owned execution leaves exactly this
// state behind, and the next pass finds the pinned version already installed,
// so without this check the worker would serve the previous release until
// something else restarted it. Anything that cannot be proven, such as a worker
// that is not running or one started from another path, is not pending.
//
// It also reports what stewardPendingSince needs to say since when: the worker
// process and the modification times of the file it executes and of the
// installed one.
func stewardWorkerRestartPendingSince(c *Context, binary string) stewardPendingRestart {
	if !c.UnitLoaded(stewardWorkerUnit) || !c.UnitActive(stewardWorkerUnit) {
		return stewardPendingRestart{}
	}
	result := c.Run([]string{"systemctl", "--user", "show", stewardWorkerUnit,
		"-p", "MainPID", "--value"}, 10*time.Second, "")
	pid, err := strconv.Atoi(strings.TrimSpace(result.Output))
	if !result.OK || err != nil || pid <= 0 {
		return stewardPendingRestart{}
	}
	installedPath, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return stewardPendingRestart{}
	}
	installed, err := os.Stat(installedPath)
	if err != nil {
		return stewardPendingRestart{}
	}
	startedFrom, running, err := stewardWorkerExecutable(pid)
	if err != nil || startedFrom != installedPath {
		return stewardPendingRestart{}
	}
	return stewardPendingRestart{
		pending: !os.SameFile(running, installed), pid: pid,
		running: running.ModTime(), installed: installed.ModTime(),
	}
}

func stewardApply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	return stewardApplyPinned(c, spec, before, actions)
}

// stewardApplyPinned installs the artifact the release pins.
//
// This is deliberately not the steward's own self-update command or its timer:
// a release names one version, and the agent proves that version before and
// after it replaces anything.
func stewardApplyPinnedBinary(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	binary := c.Path(adapterSpecString(spec, "path"))
	version := adapterSpecString(spec, "version")
	service := adapterSpecString(spec, "service")
	observedVersion, _ := before["version"].(string)
	if observedVersion != "" && strings.TrimPrefix(observedVersion, "v") == version {
		if _, _, err := stewardVerifyPairBinary(c, binary); err != nil {
			return "", err
		}
		active, _ := before["service_active"].(bool)
		if active && stewardSessionHookConverged(c, spec) {
			if before["worker_restart_pending"] != true {
				return "converged", nil
			}
			// An earlier pass installed this binary and deferred the worker
			// restart; retry it now, under the same idle rule.
			restarted, err := stewardRestartPersistentWorker(c, binary, actions)
			if err != nil {
				return "", err
			}
			if restarted {
				return "updated", nil
			}
			stewardRecordDeferredRestart(c, binary)
			return "converged", nil
		}
		// A host without a steward unit has nothing to restart: the binary is
		// what this component installs, and it is in place and verified.
		if !active && stewardSessionHookConverged(c, spec) && !stewardUnitPresent(c, service) {
			*actions = append(*actions, stewardNoUnitAction(service))
			return "converged", nil
		}
	}
	if c.DryRun {
		*actions = append(*actions, "would install and verify pinned steward "+version)
		return "updated", nil
	}

	stage, err := os.MkdirTemp("", "upkeeper-steward-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	candidate, hookCandidate, err := stewardStage(c, spec, stage)
	if err != nil {
		var failed *StepFailed
		if errors.As(err, &failed) {
			return "", failed
		}
		var artifact *stewardArtifactError
		if errors.As(err, &artifact) {
			return "", Failf("steward artifact fetch failed: %s", artifact.kind)
		}
		return "", Failf("steward artifact fetch failed: %s", "OSError")
	}

	backup := c.Path(".local/state/dev-fleet/binary-rollbacks", c.RunID)
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return "", err
	}
	hookPath := stewardSessionHook(c)
	var hookBefore []byte
	hookExisted := false
	hookMode := fs.FileMode(0o644)
	if hookCandidate != "" {
		info, statError := os.Lstat(hookPath)
		if statError == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
			return "", Failf("session hook destination is not a regular file")
		}
		if statError == nil {
			hookBefore, err = os.ReadFile(hookPath)
			if err != nil {
				return "", err
			}
			hookExisted = true
			hookMode = info.Mode().Perm()
		}
	}
	_, statError := os.Stat(binary)
	existed := statError == nil
	if existed {
		if err := agentConfigCopy(binary, backup); err != nil {
			return "", err
		}
	}

	hookInstalled := false
	unitPresent := stewardUnitPresent(c, service)
	failure := stewardInstall(c, service, binary, candidate, hookCandidate, hookPath, unitPresent, &hookInstalled)
	if failure != nil {
		return "", stewardRestorePrevious(c, before, stewardRestoration{
			binary:        binary,
			service:       service,
			backup:        backup,
			existed:       existed,
			hookPath:      hookPath,
			hookInstalled: hookInstalled,
			hookExisted:   hookExisted,
			hookBefore:    hookBefore,
			hookMode:      hookMode,
		})
	}

	*actions = append(*actions, "installed verified pinned steward "+version)
	if !unitPresent {
		*actions = append(*actions, stewardNoUnitAction(service))
	}
	if adapterSpecString(spec, "session_hook_sha256") != "" {
		*actions = append(*actions,
			"installed verified OpenCode caller hook; loaded by new OpenCode servers")
	}
	restarted, err := stewardRestartPersistentWorker(c, binary, actions)
	if err != nil {
		return "", err
	}
	if !restarted {
		stewardRecordDeferredRestart(c, binary)
	}
	return "updated", nil
}

// stewardNoUnitAction tells the operator that the steward service is theirs
// to install on this host.
func stewardNoUnitAction(service string) string {
	return service + " is not installed on this host; the verified binary is in place. " +
		"Install the service with t3-steward install-service once T3 serves"
}

// stewardInstall replaces the binary and the hook and proves the result. A
// host without a steward unit gets the binary proved without a restart.
func stewardInstall(
	c *Context, service, binary, candidate, hookCandidate, hookPath string,
	unitPresent bool, hookInstalled *bool,
) error {
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		return err
	}
	temporary := binary + ".upkeeper-tmp"
	if pathExists(temporary) {
		return Failf("steward staging path already exists")
	}
	if err := agentConfigCopy(candidate, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, binary); err != nil {
		return err
	}
	if hookCandidate != "" {
		if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
			return err
		}
		body, err := os.ReadFile(hookCandidate)
		if err != nil {
			return err
		}
		hookTemporary := hookPath + ".upkeeper-tmp"
		handle, err := os.OpenFile(hookTemporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := handle.Write(body); err != nil {
			_ = handle.Close()
			return err
		}
		if err := handle.Close(); err != nil {
			return err
		}
		if err := os.Chmod(hookTemporary, 0o644); err != nil {
			return err
		}
		if err := os.Rename(hookTemporary, hookPath); err != nil {
			return err
		}
		*hookInstalled = true
	}
	if !unitPresent {
		_, _, err := stewardVerifyPairBinary(c, binary)
		return err
	}
	if _, err := c.Must([]string{"systemctl", "--user", "restart", service},
		0, "", "restart pinned steward"); err != nil {
		return err
	}
	if _, _, err := stewardVerifyPairBinary(c, binary); err != nil {
		return err
	}
	if !c.UnitActive(service) {
		return Failf("pinned steward service is inactive")
	}
	return nil
}

// stewardRestoration is what a failed install has to put back.
type stewardRestoration struct {
	binary        string
	service       string
	backup        string
	existed       bool
	hookPath      string
	hookInstalled bool
	hookExisted   bool
	hookBefore    []byte
	hookMode      fs.FileMode
}

// stewardRestorePrevious puts the machine back the way the run found it.
//
// The returned error is the outcome of the component: RolledBack when the
// previous state is proven back in place, and a "rollback failed" StepFailed
// only when restoration itself did not work.
func stewardRestorePrevious(
	c *Context, before map[string]any, state stewardRestoration,
) error {
	hookRestoreFailed := false
	if state.hookInstalled {
		if !state.hookExisted {
			if err := os.Remove(state.hookPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				hookRestoreFailed = true
			}
		} else if err := os.WriteFile(state.hookPath, state.hookBefore, state.hookMode); err != nil {
			hookRestoreFailed = true
		} else if err := os.Chmod(state.hookPath, state.hookMode); err != nil {
			hookRestoreFailed = true
		}
	}
	if !state.existed {
		_ = os.Remove(state.binary)
		return Failf("new steward failed; removed unverified binary")
	}
	if err := stewardRestoreBinary(c, before, state); err != nil {
		return Failf("rollback failed: steward restoration failed")
	}
	if hookRestoreFailed {
		return Failf("rollback failed: steward restored but session hook restoration failed")
	}
	return RolledBackf("restored previous steward binary, service and session hook")
}

func stewardRestoreBinary(c *Context, before map[string]any, state stewardRestoration) error {
	restore := state.binary + ".upkeeper-restore"
	if err := agentConfigCopy(state.backup, restore); err != nil {
		return err
	}
	if err := os.Rename(restore, state.binary); err != nil {
		return err
	}
	if _, err := c.Must([]string{"systemctl", "--user", "restart", state.service},
		0, "", "restore steward"); err != nil {
		return err
	}
	previousDigest, _ := before["binary_sha256"].(string)
	if c.SHA256File(state.binary) != previousDigest || !c.UnitActive(state.service) {
		return Failf("previous steward did not verify")
	}
	return nil
}

// stewardRestartPersistentWorker moves the persistent worker onto the installed
// binary when it is idle.
//
// Installing the binary does not change the worker process that is already
// running, so a host that runs the worker would keep serving the previous
// release against a coordinator that already speaks the new one. A worker whose
// durable journal still owns dispatched or settle-pending execution is reported
// and left running, never forced; the next pass sees the restart pending and
// retries it. restarted is true when the restart was performed, or would have
// been in a dry run.
func stewardRestartPersistentWorker(c *Context, binary string, actions *[]string) (bool, error) {
	if !c.UnitLoaded(stewardWorkerUnit) {
		return false, nil
	}
	summary := c.Run([]string{binary, "worker", "inspect-journal"}, 30*time.Second, "")
	if !summary.OK {
		tail := summary.Output
		if len(tail) > 200 {
			tail = tail[len(tail)-200:]
		}
		return false, Failf("worker journal inspection failed: %s", tail)
	}
	document, err := canonjson.DecodeObject([]byte(summary.Output))
	if err != nil {
		return false, Failf("worker journal inspection returned no usable summary")
	}
	dispatched, counted := stewardJournalCount(document, "dispatched")
	if !counted {
		return false, Failf("worker journal inspection returned no usable summary")
	}
	if dispatched != 0 {
		*actions = append(*actions, fmt.Sprintf(
			"worker restart deferred: %d dispatched attempt(s) in the journal", dispatched))
		return false, nil
	}
	// An attempt waiting to settle is still owned by this worker, and a summary
	// that does not count such attempts cannot prove there are none.
	if _, present := document["settlePending"]; !present {
		*actions = append(*actions,
			"worker restart deferred: the journal summary does not count settle-pending attempts")
		return false, nil
	}
	settlePending, counted := stewardJournalCount(document, "settlePending")
	if !counted {
		return false, Failf("worker journal inspection returned no usable summary")
	}
	if settlePending != 0 {
		*actions = append(*actions, fmt.Sprintf(
			"worker restart deferred: %d settle-pending attempt(s) in the journal", settlePending))
		return false, nil
	}
	if c.DryRun {
		*actions = append(*actions,
			"would restart "+stewardWorkerUnit+" onto the pinned steward")
		return true, nil
	}
	if _, err := c.Must([]string{"systemctl", "--user", "restart", stewardWorkerUnit},
		0, "", "restart persistent worker"); err != nil {
		return false, err
	}
	if !c.UnitActive(stewardWorkerUnit) {
		return false, Failf("persistent worker is inactive after restart")
	}
	*actions = append(*actions, "restarted "+stewardWorkerUnit+" onto the pinned steward")
	return true, nil
}

// stewardJournalCount reads one attempt count from the worker journal summary.
//
// A count is a non-negative integer written in decimal digits alone, as a JSON
// number or a string, that fits in an int. Anything else is malformed: a
// fractional, negative, signed, exponent or out-of-range value must never be
// read as zero, because zero authorizes a worker restart.
func stewardJournalCount(document map[string]any, field string) (int, bool) {
	var text string
	switch typed := document[field].(type) {
	case json.Number:
		text = typed.String()
	case string:
		text = typed
	default:
		return 0, false
	}
	// ParseUint accepts neither a sign, a fraction nor an exponent.
	number, err := strconv.ParseUint(text, 10, strconv.IntSize-1)
	if err != nil {
		return 0, false
	}
	return int(number), true
}
