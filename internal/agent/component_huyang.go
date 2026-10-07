package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
)

func init() {
	Register("huyang", Handler{Observe: huyangObserve, Apply: huyangApply})
}

// The probe and the two waits below are package variables only so that a test
// can drive this adapter without a real Huyang process and without a real
// control socket. Production keeps the bounds the Python agent used.
var (
	huyangProbeFunc     = huyangProbe
	huyangSocketTimeout = 45 * time.Second
	huyangSocketPoll    = 1 * time.Second
	huyangSettleDelay   = 1 * time.Second
	huyangProbeJoin     = 30 * time.Second
	huyangHandshakeWait = 1 * time.Second
)

// huyangUnitTemplate is the unit a bootstrap seeds for a host that has none.
// It is wanted by default.target and so must not be ordered after it: that
// puts the unit, and every unit ordered after huyang.service, in a cycle with
// default.target (see user_unit_ordering.go).
const huyangUnitTemplate = "[Unit]\nDescription=Huyang semantic workspace service\n" +
	"\n[Service]\nType=simple\n" +
	"WorkingDirectory=%%h/%[1]s\nExecStart=%%h/%[1]s/bin/huyang serve\n" +
	"Restart=on-failure\nRestartSec=2\n\n[Install]\nWantedBy=default.target\n"

// huyangProbeResult is what one MCP handshake proved about a running Huyang.
type huyangProbeResult struct {
	OK bool
	// Tools is nil when the server never answered tools/list.
	Tools *int
	// ServerVersion is nil when the server never answered initialize.
	ServerVersion *string
	// Error is set when the probe could not be spoken to at all.
	Error string
}

// huyangProbeRepr renders a probe result the way the Python agent's dictionary
// printed, because the controller shows these strings to an operator verbatim.
func huyangProbeRepr(probe huyangProbeResult) string {
	if probe.Error != "" {
		return "{'ok': False, 'error': " + huyangQuote(probe.Error) + "}"
	}
	version := "None"
	if probe.ServerVersion != nil {
		version = huyangQuote(*probe.ServerVersion)
	}
	return fmt.Sprintf("{'ok': %s, 'tools': %s, 'server_version': %s}",
		huyangBool(probe.OK), huyangOptionalInt(probe.Tools), version)
}

func huyangQuote(value string) string {
	escaped := strings.ReplaceAll(value, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "'", "\\'")
	return "'" + escaped + "'"
}

func huyangBool(value bool) string {
	if value {
		return "True"
	}
	return "False"
}

func huyangOptionalInt(value *int) string {
	if value == nil {
		return "None"
	}
	return strconv.Itoa(*value)
}

func huyangOptionalString(value *string) string {
	if value == nil {
		return "None"
	}
	return *value
}

// huyangSocket is the control socket a running Huyang listens on.
func huyangSocket(c *Context) string {
	return filepath.Join(c.Runtime, "huyang/control.sock")
}

// huyangProbe starts one throwaway MCP client against the built binary and
// asks it to list its tools.
//
// The exchange is JSON-RPC over the child's standard input and output: an
// initialize request, the initialized notification, and a tools/list request.
// A server that answers with the number of tools the release pins is the only
// evidence that a build is actually usable.
func huyangProbe(c *Context, checkout string, expectedTools int) huyangProbeResult {
	binary := filepath.Join(checkout, "bin/huyang")
	command := exec.Command(binary, "mcp", "-socket", huyangSocket(c), "-profile", "full")
	command.Env = c.Env
	command.Stderr = nil
	stdin, err := command.StdinPipe()
	if err != nil {
		return huyangProbeResult{Error: err.Error()}
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return huyangProbeResult{Error: err.Error()}
	}
	if err := command.Start(); err != nil {
		return huyangProbeResult{Error: err.Error()}
	}

	var guard sync.Mutex
	found := map[int64]map[string]any{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			message, err := canonjson.DecodeObject([]byte(line))
			if err != nil {
				continue
			}
			identifier, numbered := message["id"].(json.Number)
			if !numbered {
				continue
			}
			value, err := identifier.Int64()
			if err != nil || (value != 1 && value != 2) {
				continue
			}
			guard.Lock()
			found[value] = message
			_, complete := found[2]
			guard.Unlock()
			if complete {
				return
			}
		}
	}()

	stop := func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}
	write := func(message map[string]any) error {
		encoded, err := canonjson.Compact(message)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(encoded, '\n'))
		return err
	}
	if err := write(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "upkeeper", "version": "1"},
		},
	}); err != nil {
		stop()
		return huyangProbeResult{Error: "mcp write failed: " + err.Error()}
	}
	time.Sleep(huyangHandshakeWait)
	notified := write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if notified == nil {
		notified = write(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	}
	if notified != nil {
		stop()
		return huyangProbeResult{Error: "mcp write failed: " + notified.Error()}
	}
	select {
	case <-done:
	case <-time.After(huyangProbeJoin):
	}
	stop()

	guard.Lock()
	defer guard.Unlock()
	result := huyangProbeResult{}
	if listing, ok := found[2]; ok {
		if payload, ok := listing["result"].(map[string]any); ok {
			if tools, ok := payload["tools"].([]any); ok {
				count := len(tools)
				result.Tools = &count
			}
		}
	}
	if handshake, ok := found[1]; ok {
		if payload, ok := handshake["result"].(map[string]any); ok {
			if server, ok := payload["serverInfo"].(map[string]any); ok {
				if version, ok := server["version"].(string); ok {
					result.ServerVersion = &version
				}
			}
		}
	}
	result.OK = result.Tools != nil && *result.Tools == expectedTools
	return result
}

// huyangWaitForSocket blocks until a restarted service is listening again.
func huyangWaitForSocket(c *Context) error {
	deadline := time.Now().Add(huyangSocketTimeout)
	for {
		if pathExists(huyangSocket(c)) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return Failf("control socket did not appear after restart")
		}
		time.Sleep(huyangSocketPoll)
	}
}

// huyangWaitProbe waits for the control socket and then probes.
func huyangWaitProbe(c *Context, checkout string, expectedTools int) (huyangProbeResult, error) {
	if err := huyangWaitForSocket(c); err != nil {
		return huyangProbeResult{}, err
	}
	return huyangProbeFunc(c, checkout, expectedTools), nil
}

func huyangObserve(c *Context, spec map[string]any) (map[string]any, error) {
	checkout := c.Path(adapterSpecString(spec, "path"))
	head := c.GitHead(checkout)
	observed := map[string]any{
		"commit":         nil,
		"dirty":          nil,
		"service_active": c.UnitActive(adapterSpecString(spec, "service")),
		"socket":         pathExists(huyangSocket(c)),
		"binary_sha256":  nil,
	}
	// Both are unknown (nil) when the units cannot be read; apply's check then
	// fails with the reason.
	observed["unit_ordering_cycle"] = nil
	if cycle, err := userUnitOrderingCycle(
		c.Path(userUnitDir), adapterSpecString(spec, "service"), nil); err == nil {
		observed["unit_ordering_cycle"] = cycle
	}
	observed["user_unit_ordering_cycles"] = nil
	if cycles, err := userUnitOrderingCycles(c, nil); err == nil {
		observed["user_unit_ordering_cycles"] = cycles
	}
	// A reconciled unit whose daemon-reload has not completed: the bytes are
	// right, but systemd still schedules by the old ordering.
	_, markerErr := os.Lstat(c.Path(huyangReloadPending))
	observed["unit_reload_pending"] = markerErr == nil
	if head != "" {
		observed["commit"] = head
		if dirty := c.GitDirty(checkout); dirty != nil {
			observed["dirty"] = *dirty
		}
	}
	if digest := c.SHA256File(filepath.Join(checkout, "bin/huyang")); digest != "" {
		observed["binary_sha256"] = digest
	}
	return observed, nil
}

// huyangUnitOrderingClean reports whether observe proved that no user unit is
// in an ordering cycle with default.target and that no reload of the
// reconciled Huyang unit is outstanding. An unknown observation is not clean.
// A cycle in a unit Huyang does not own does not fail the step, so status is
// where it stays visible as drift.
func huyangUnitOrderingClean(observed map[string]any) bool {
	if observed["unit_ordering_cycle"] != false || observed["unit_reload_pending"] != false {
		return false
	}
	switch cycles := observed["user_unit_ordering_cycles"].(type) {
	case []string:
		return len(cycles) == 0
	case []any:
		return len(cycles) == 0
	}
	return false
}

// huyangProbeTools is the tool count the release expects a build to serve.
func huyangProbeTools(spec map[string]any) int {
	tools, _ := adapterSpecInt(adapterSpecMap(spec, "probe"), "tools")
	return tools
}

// huyangBuildCommand is the release's build command, defaulting to make build.
func huyangBuildCommand(spec map[string]any) []string {
	build := adapterSpecStrings(spec, "build")
	if len(build) == 0 {
		return []string{"make", "build"}
	}
	return build
}

// huyangBuildAndRestart moves the checkout to one commit, builds it, restarts
// the service and proves the result answers the MCP probe.
//
// It is deliberately usable in both directions: the rollback path calls it with
// the commit the run found rather than the commit the release pins.
func huyangBuildAndRestart(
	c *Context, spec map[string]any, checkout, commit string, actions *[]string,
) error {
	if _, err := c.Must(
		[]string{"git", "-C", checkout, "checkout", "--quiet", "--detach", commit},
		60*time.Second, "", "git checkout",
	); err != nil {
		return err
	}
	if c.GitHead(checkout) != commit {
		return Failf("checkout did not land on the requested commit")
	}
	*actions = append(*actions, "checked out "+adapterShortCommit(commit))
	if _, err := c.Must(huyangBuildCommand(spec), 600*time.Second, checkout, "build"); err != nil {
		return err
	}
	*actions = append(*actions, "built")
	service := adapterSpecString(spec, "service")
	if _, err := c.Must([]string{"systemctl", "--user", "restart", service},
		60*time.Second, "", "restart"); err != nil {
		return err
	}
	*actions = append(*actions, "restarted "+service)
	if err := huyangWaitForSocket(c); err != nil {
		return err
	}
	time.Sleep(huyangSettleDelay)
	probe := huyangProbeFunc(c, checkout, huyangProbeTools(spec))
	*actions = append(*actions, fmt.Sprintf("mcp probe tools=%s server=%s",
		huyangOptionalInt(probe.Tools), huyangOptionalString(probe.ServerVersion)))
	if !probe.OK {
		return Failf("mcp probe failed: %s", huyangProbeRepr(probe))
	}
	return nil
}

// huyangApply converges the checkout and the installed unit, then checks that
// no user unit is left in an ordering cycle with default.target.
func huyangApply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	var pending map[string][]byte
	var touched, prepared bool
	prepareUnit := func() error {
		var err error
		pending, touched, err = huyangReconcileUnit(c, spec, actions)
		prepared = err == nil
		return err
	}
	prepareBuild := func() error {
		if blocker := requirementsBlockForStaging(c, "huyang", spec, actions, true); blocker != "" {
			return &Blocked{Message: blocker}
		}
		return prepareUnit()
	}
	state, err := huyangApplyCheckout(c, spec, before, actions, prepareBuild)
	if err != nil {
		return state, err
	}
	// A converged or ahead checkout did not select a build, but still needs
	// its unit migration. Build paths prepare it only after requirements pass.
	if !prepared {
		if err := prepareUnit(); err != nil {
			return "", err
		}
	}
	service := adapterSpecString(spec, "service")
	if err := checkUserUnitOrdering(c, service, pending, actions); err != nil {
		return "", err
	}
	if state == "converged" && touched {
		state = "updated"
	}
	return state, nil
}

// huyangReloadPending marks a reconciled unit whose daemon-reload has not
// completed yet. It is created before the unit is rewritten and removed after
// the reload, so a run that rewrote the unit but failed to reload leaves it
// behind, and the next run reloads although the bytes on disk already match.
const huyangReloadPending = ".local/state/dev-fleet/unit-migrations/huyang-ordering.reload-pending"

// huyangReconcileUnit removes After=default.target from an installed Huyang
// unit and changes nothing else in it. Units seeded before that line was
// dropped from the template still carry it, and the bootstrap never replaces
// an existing unit.
//
// Ordering only decides how systemd schedules jobs, so a daemon-reload is
// enough and a running Huyang is not restarted. A unit that is a symlink or
// not a regular file is left alone; the ordering check then names it.
//
// It runs whether or not a checkout exists: the bootstrap refuses a host that
// has a unit but no checkout, and that unit must not keep the cycle meanwhile.
//
// pending holds the rewritten content when the unit changed (or would have,
// in a dry run), keyed by unit name, and is nil otherwise. touched reports
// whether the run changed the unit or completed a reload an earlier run left
// pending.
func huyangReconcileUnit(
	c *Context, spec map[string]any, actions *[]string,
) (pending map[string][]byte, touched bool, err error) {
	service := adapterSpecString(spec, "service")
	unit := c.Path(userUnitDir, service)
	marker := c.Path(huyangReloadPending)
	_, markerErr := os.Lstat(marker)
	reloadPending := markerErr == nil
	info, err := os.Lstat(unit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, nil
	}
	raw, err := os.ReadFile(unit)
	if err != nil {
		return nil, false, err
	}
	reconciled, changed := unitDropOrderingAfter(raw, userUnitBootTarget)
	if !changed && !reloadPending {
		return nil, false, nil
	}
	if changed {
		pending = map[string][]byte{service: reconciled}
	}
	if c.DryRun {
		if changed {
			*actions = append(*actions, "would remove After=default.target from "+service)
		} else {
			*actions = append(*actions, "would reload systemd for the reconciled "+service)
		}
		return pending, true, nil
	}
	if changed {
		if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
			return nil, false, err
		}
		if err := fsatomic.WritePrivateFile(marker, []byte(service+"\n")); err != nil {
			return nil, false, err
		}
		if err := fsatomic.WriteFile(unit, reconciled, info.Mode().Perm()); err != nil {
			return nil, false, err
		}
	}
	if _, err := c.Must([]string{"systemctl", "--user", "daemon-reload"},
		0, "", "Huyang unit reload"); err != nil {
		return nil, false, err
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if changed {
		*actions = append(*actions, "removed After=default.target from "+service+
			"; reloaded systemd without restarting Huyang")
	} else {
		*actions = append(*actions, "reloaded systemd for the reconciled "+service+
			" after an earlier reload failed; Huyang was not restarted")
	}
	return pending, true, nil
}

// huyangWouldBuild reports whether huyangApplyCheckout would build, by asking
// what it asks in the same order; an answer that cannot be had is yes. A
// checkout already on the pin is built again to finish an interrupted
// bootstrap, and when its service is not running or does not answer the probe.
// Only the last needs the probe, and it is asked only when everything else
// says the installation is converged.
func huyangWouldBuild(c *Context, spec map[string]any) bool {
	wanted := adapterSpecString(spec, "commit")
	checkout := c.Path(adapterSpecString(spec, "path"))
	head := c.GitHead(checkout)
	if wanted == "" || head == "" {
		return true
	}
	// Local changes are refused, and so is an interrupted bootstrap whose
	// checkout has left the pin; neither builds.
	if dirty := c.GitDirty(checkout); dirty != nil && *dirty {
		return false
	}
	if pathExists(c.Path(huyangBootstrapPending)) {
		return head == wanted
	}
	if head != wanted {
		// A checkout ahead of the pin is kept. Whether one that is not
		// diverges is only known after a fetch, so it counts as a build.
		return c.Payload.AllowDowngrade || !c.GitContains(checkout, wanted, head)
	}
	if !c.UnitActive(adapterSpecString(spec, "service")) || !pathExists(huyangSocket(c)) {
		return true
	}
	return !huyangProbeFunc(c, checkout, huyangProbeTools(spec)).OK
}

// huyangApplyCheckout moves the checkout to the pinned commit, or seeds Huyang
// on a host that has none.
func huyangApplyCheckout(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
	prepareBuild ...func() error,
) (string, error) {
	checkout := c.Path(adapterSpecString(spec, "path"))
	commit, _ := before["commit"].(string)
	if commit == "" {
		// A checkout an interrupted bootstrap left whose HEAD cannot be read
		// is still that bootstrap's, and its marker must be kept.
		if pathExists(checkout) && pathExists(c.Path(huyangBootstrapPending)) {
			return "", huyangResumeRefused(c, checkout, "has a HEAD that could not be read")
		}
		return huyangBootstrap(c, spec, actions, prepareBuild...)
	}
	// Local changes outrank the release, including a bootstrap an earlier run
	// left unfinished: whoever changed the checkout since owns it now.
	if dirty, _ := before["dirty"].(bool); dirty {
		if pathExists(c.Path(huyangBootstrapPending)) {
			return "", huyangResumeRefused(c, checkout, "has local changes")
		}
		return "", Failf("%s has local changes; refusing to move it", checkout)
	}
	if pathExists(c.Path(huyangBootstrapPending)) {
		return huyangResumeBootstrap(c, spec, checkout, commit, actions, prepareBuild...)
	}
	wanted := adapterSpecString(spec, "commit")
	allowDowngrade := c.Payload.AllowDowngrade
	if commit != wanted && !allowDowngrade && c.GitContains(checkout, wanted, commit) {
		*actions = append(*actions,
			"checkout is ahead of the pin; normal pull preserves newer Huyang state")
		return "ahead", nil
	}
	serviceActive, _ := before["service_active"].(bool)
	socket, _ := before["socket"].(bool)
	if commit == wanted && serviceActive && socket {
		probe := huyangProbeFunc(c, checkout, huyangProbeTools(spec))
		*actions = append(*actions, "mcp probe tools="+huyangOptionalInt(probe.Tools))
		if probe.OK {
			return "converged", nil
		}
		*actions = append(*actions, "running service does not answer the probe; rebuilding")
	}
	// Preflight may have seen a healthy service that stopped before Observe,
	// or a probe that answered before this one failed. Enforce the build
	// requirements now, before any checkout preparation or dry-run promise.
	if len(prepareBuild) > 0 {
		if err := prepareBuild[0](); err != nil {
			return "", err
		}
	} else if blocker := requirementsBlockForStaging(c, "huyang", spec, actions, true); blocker != "" {
		return "", &Blocked{Message: blocker}
	}
	ref := adapterSpecString(spec, "ref")
	if c.DryRun {
		*actions = append(*actions, fmt.Sprintf(
			"would fetch origin %s, check out %s, build, restart",
			ref, adapterShortCommit(wanted)))
		return "updated", nil
	}
	if _, err := c.Must([]string{"git", "-C", checkout, "fetch", "--quiet", "origin", ref},
		180*time.Second, "", "git fetch"); err != nil {
		return "", err
	}
	*actions = append(*actions, "fetched origin "+ref)
	if !c.Run([]string{"git", "-C", checkout, "cat-file", "-e", wanted + "^{commit}"},
		15*time.Second, "").OK {
		return "", Failf("origin/%s does not contain %s", ref, adapterShortCommit(wanted))
	}
	// Somebody may have started working in this checkout between the observation
	// and now. Their work outranks the release.
	dirty := c.GitDirty(checkout)
	if (dirty != nil && *dirty) || c.GitHead(checkout) != commit {
		return "", Failf("Huyang checkout changed during preparation; preserving local work")
	}
	if !allowDowngrade && !c.GitContains(checkout, commit, wanted) {
		*actions = append(*actions,
			"checkout diverges from the pin; normal pull preserves local Huyang work")
		return "ahead", nil
	}
	if failure := huyangBuildAndRestart(c, spec, checkout, wanted, actions); failure != nil {
		*actions = append(*actions, "failure: "+failure.Error())
		*actions = append(*actions, "rolling back to "+adapterShortCommit(commit))
		if err := huyangBuildAndRestart(c, spec, checkout, commit, actions); err != nil {
			return "", Failf("rollback failed: %s", err.Error())
		}
		return "", RolledBackf("%s", failure.Error())
	}
	return "updated", nil
}

// huyangBootstrap installs a missing pinned Huyang checkout.
//
// Nothing that already exists is replaced: a path where the checkout belongs,
// or a user unit without a checkout, is a host state an operator has to look at
// rather than something a convergence may overwrite. The clone and the build
// happen in a private staging directory, so a build that fails never puts
// anything at the live path.
func huyangBootstrap(c *Context, spec map[string]any, actions *[]string, prepareBuild ...func() error) (string, error) {
	relative := adapterSpecString(spec, "path")
	checkout := c.Path(relative)
	service := adapterSpecString(spec, "service")
	unit := c.Path(".config/systemd/user", service)
	if pathExists(checkout) {
		return "", errors.New(checkout + " exists but is not a Git checkout; preserving it")
	}
	if pathExists(unit) {
		return "", errors.New(
			"Huyang checkout is absent but a user unit exists; inspect it first")
	}
	if len(prepareBuild) > 0 {
		if err := prepareBuild[0](); err != nil {
			return "", err
		}
	}
	for _, tool := range []string{"git", "go", "make"} {
		if adapterLookPath(c, tool) == "" {
			return "", errors.New("Huyang build prerequisite missing: " + tool)
		}
	}
	source := adapterSpecString(spec, "source")
	ref := adapterSpecString(spec, "ref")
	commit := adapterSpecString(spec, "commit")
	if c.DryRun {
		*actions = append(*actions, fmt.Sprintf(
			"would clone %s at %s, build and enable Huyang", source, commit))
		return "updated", nil
	}
	if err := os.MkdirAll(filepath.Dir(checkout), 0o755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(filepath.Dir(checkout), ".huyang-bootstrap-")
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(stage, "checkout")
	steps := []struct {
		argv    []string
		timeout time.Duration
		dir     string
		what    string
	}{
		{[]string{"git", "clone", "--quiet", "--no-checkout", source, candidate},
			180 * time.Second, "", "Huyang clone"},
		{[]string{"git", "-C", candidate, "fetch", "--quiet", "origin", ref},
			180 * time.Second, "", "Huyang fetch"},
		{[]string{"git", "-C", candidate, "merge-base", "--is-ancestor", commit, "FETCH_HEAD"},
			30 * time.Second, "", "Huyang pin provenance"},
		{[]string{"git", "-C", candidate, "checkout", "--quiet", "--detach", commit},
			60 * time.Second, "", "Huyang pinned checkout"},
		{huyangBuildCommand(spec), 600 * time.Second, candidate, "Huyang build"},
	}
	for _, step := range steps {
		if _, err := c.Must(step.argv, step.timeout, step.dir, step.what); err != nil {
			return "", err
		}
	}
	// From here a crash leaves a checkout without a started service; the
	// marker makes the next run finish the bootstrap instead of mistaking the
	// checkout for an installed Huyang.
	if err := os.MkdirAll(filepath.Dir(c.Path(huyangBootstrapPending)), 0o700); err != nil {
		return "", err
	}
	if err := fsatomic.WritePrivateFile(c.Path(huyangBootstrapPending), []byte(relative+"\n")); err != nil {
		return "", err
	}
	// The rename is the exclusive claim: even an empty pre-existing directory
	// at the live path belongs to whoever made it, so it is never replaced.
	// Claiming the path with a directory first cannot work, because nothing
	// can then be renamed onto that directory.
	if err := fsatomic.RenameNoReplace(candidate, checkout); err != nil {
		// Nothing of this bootstrap is at the live path, so there is nothing
		// for a later run to finish.
		_ = os.Remove(c.Path(huyangBootstrapPending))
		if errors.Is(err, fs.ErrExist) {
			return "", errors.New(checkout + " appeared during the bootstrap; preserving it and the built candidate at " + candidate)
		}
		return "", err
	}
	if err := os.Remove(stage); err != nil {
		return "", err
	}
	if err := huyangSeedUnit(unit, relative); err != nil {
		return "", err
	}
	if err := huyangStart(c, spec, checkout, service, false); err != nil {
		// Keep the candidate bytes and the unit file for diagnosis; the
		// scheduler's host hold is what prevents a blind retry.
		if _, stopErr := c.Must(
			[]string{"systemctl", "--user", "disable", "--now", service},
			60*time.Second, "", "stop failed Huyang bootstrap",
		); stopErr != nil {
			return "", Failf("%s; %s", err.Error(), stopErr.Error())
		}
		return "", err
	}
	if err := os.Remove(c.Path(huyangBootstrapPending)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	*actions = append(*actions, fmt.Sprintf("installed Huyang %s; enabled %s",
		adapterShortCommit(commit), service))
	return "updated", nil
}

// huyangBootstrapPending marks a bootstrap that has put the checkout in place
// but not yet seeded and started its unit. It is written before the checkout
// is renamed into place and removed once the service answers.
const huyangBootstrapPending = ".local/state/dev-fleet/unit-migrations/huyang-bootstrap.pending"

// huyangSeedUnit writes the bootstrap unit, never over an existing file.
func huyangSeedUnit(unit, relative string) error {
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return err
	}
	handle, err := os.OpenFile(unit, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := handle.WriteString(fmt.Sprintf(huyangUnitTemplate, relative)); err != nil {
		_ = handle.Close()
		return err
	}
	if err := handle.Close(); err != nil {
		return err
	}
	return os.Chmod(unit, 0o644)
}

// huyangResumeBootstrap finishes a bootstrap a crash interrupted after the
// checkout was in place: it builds the checkout again, seeds the unit if the
// crash came first, or rewrites one the crash left half written, then enables
// and proves the service. The build output is ignored by git, so a clean
// checkout on the pin says nothing about the binary the unit would start, and
// the pin may have moved since the bootstrap built it. A checkout that moved
// since, or a unit that is not the seeded one, is the operator's to look at.
func huyangResumeBootstrap(
	c *Context, spec map[string]any, checkout, commit string, actions *[]string,
	prepareBuild ...func() error,
) (string, error) {
	relative := adapterSpecString(spec, "path")
	service := adapterSpecString(spec, "service")
	unit := c.Path(".config/systemd/user", service)
	if commit != adapterSpecString(spec, "commit") {
		return "", huyangResumeRefused(c, checkout, "is on "+adapterShortCommit(commit)+", not the pinned commit")
	}
	// The observation may be stale by now. Nothing is seeded or started, and no
	// dry run promises either, unless the checkout is still on the pin and
	// provably has no local changes.
	if err := huyangResumeCheckout(c, checkout, commit); err != nil {
		return "", err
	}
	want := fmt.Sprintf(huyangUnitTemplate, relative)
	before, err := huyangResumeUnit(c, unit, service, want)
	if err != nil {
		return "", err
	}
	// Only a regular file can be removed once the bootstrap is finished;
	// anything else would leave a running service and a marker that makes
	// every later run build and restart it again.
	if info, err := os.Lstat(c.Path(huyangBootstrapPending)); err != nil || !info.Mode().IsRegular() {
		return "", Failf("an interrupted Huyang bootstrap found its marker %s is not a regular file; "+
			"inspect it, and replace it with an empty file to let the next run finish the bootstrap",
			c.Path(huyangBootstrapPending))
	}
	if len(prepareBuild) > 0 {
		if err := prepareBuild[0](); err != nil {
			return "", err
		}
	}
	if c.DryRun {
		*actions = append(*actions, "would finish the interrupted Huyang bootstrap: build, seed and enable "+service)
		return "updated", nil
	}
	// The directory stays open across the build, so its inode cannot be freed
	// and handed to a directory created in its place meanwhile.
	handle, err := os.Open(checkout)
	if err != nil {
		return "", huyangResumeRefused(c, checkout, "changed or could not be read")
	}
	defer handle.Close()
	origin, err := handle.Stat()
	if err != nil {
		return "", huyangResumeRefused(c, checkout, "changed or could not be read")
	}
	if _, err := c.Must(huyangBuildCommand(spec), 600*time.Second, checkout, "Huyang build"); err != nil {
		return "", err
	}
	*actions = append(*actions, "built "+adapterShortCommit(commit))
	// The build can take minutes, and the checkout and the unit stay the
	// operator's meanwhile: both are checked again, the checkout must still be
	// the directory that was built (its build output is ignored by git, so
	// another clone on the pin looks the same), and the unit must be exactly
	// what it was before the build, before anything is seeded or started.
	if now, err := os.Stat(checkout); err != nil || !os.SameFile(origin, now) {
		return "", huyangResumeRefused(c, checkout, "replaced while it built")
	}
	if err := huyangResumeCheckout(c, checkout, commit); err != nil {
		return "", err
	}
	after, err := huyangResumeUnit(c, unit, service, want)
	if err != nil {
		return "", err
	}
	if (before == nil) != (after == nil) || (before != nil && !bytes.Equal(*before, *after)) {
		return "", Failf("an interrupted Huyang bootstrap found %s changed while it built; inspect it. "+
			"Keep %s: the next run finishes the bootstrap once the unit is the seeded one or absent",
			unit, c.Path(huyangBootstrapPending))
	}
	if after == nil || string(*after) != want {
		// Absent, or a prefix of the template the crash cut short.
		if err := os.Remove(unit); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err := huyangSeedUnit(unit, relative); err != nil {
			return "", err
		}
	}
	if err := huyangStart(c, spec, checkout, service, true); err != nil {
		// As after a failed bootstrap: an unproven service is not left enabled
		// at boot, and the marker stays for the next run.
		if _, stopErr := c.Must(
			[]string{"systemctl", "--user", "disable", "--now", service},
			60*time.Second, "", "stop failed Huyang bootstrap",
		); stopErr != nil {
			return "", Failf("%s; %s", err.Error(), stopErr.Error())
		}
		return "", err
	}
	if err := os.Remove(c.Path(huyangBootstrapPending)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	*actions = append(*actions, fmt.Sprintf("finished the interrupted Huyang bootstrap at %s; enabled %s",
		adapterShortCommit(commit), service))
	return "updated", nil
}

// huyangResumeCheckout refuses recovery unless the checkout is provably on
// the pin without local changes.
func huyangResumeCheckout(c *Context, checkout, commit string) error {
	dirty := c.GitDirty(checkout)
	if dirty == nil || *dirty || c.GitHead(checkout) != commit {
		return huyangResumeRefused(c, checkout, "changed or could not be read")
	}
	return nil
}

// huyangResumeUnit reads the unit an interrupted bootstrap left: nil when
// there is none, otherwise its bytes, the start of the seeded unit. Only a
// regular file holding the start of the template is the seeded unit a crash
// cut short. A mask (a link to /dev/null) or an empty file reads as nothing,
// which is a prefix of anything, so neither is taken for one.
func huyangResumeUnit(c *Context, unit, service, want string) (*[]byte, error) {
	info, err := os.Lstat(unit)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err == nil && (!info.Mode().IsRegular() || info.Size() == 0) {
		return nil, Failf("an interrupted Huyang bootstrap found %s masked, empty or not a regular file; "+
			"inspect it. Only the next run's bootstrap enables %s at boot: remove the file to let it "+
			"finish, or enable the service yourself before removing %s",
			unit, service, c.Path(huyangBootstrapPending))
	}
	// A unit longer than the template cannot be the start of it, so no more
	// than one byte past the template is ever read.
	var raw []byte
	if err == nil {
		raw, err = huyangReadAtMost(unit, len(want)+1)
	}
	if err != nil {
		return nil, Failf("an interrupted Huyang bootstrap could not read %s (%v); inspect it. "+
			"Only the next run's bootstrap enables %s at boot: make it readable or remove it to let it "+
			"finish, or enable the service yourself before removing %s",
			unit, err, service, c.Path(huyangBootstrapPending))
	}
	if len(raw) == 0 || !strings.HasPrefix(want, string(raw)) {
		return nil, Failf("an interrupted Huyang bootstrap found %s changed; inspect it. "+
			"Only the next run's bootstrap enables %s at boot: restore the seeded unit to let it "+
			"finish, or enable the service yourself before removing %s",
			unit, service, c.Path(huyangBootstrapPending))
	}
	return &raw, nil
}

// huyangReadAtMost reads up to limit bytes from the start of path.
func huyangReadAtMost(path string, limit int) ([]byte, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	return io.ReadAll(io.LimitReader(handle, int64(limit)))
}

// huyangResumeRefused explains why an interrupted bootstrap was not finished.
// The pending marker is what makes a later run seed and enable the unit, so the
// operator is told to keep it: the normal update path only restarts Huyang and
// would leave a service that never starts at boot.
func huyangResumeRefused(c *Context, checkout, why string) error {
	return Failf("an interrupted Huyang bootstrap found %s %s; preserving local work. "+
		"Keep %s: once the checkout is back on the pinned commit without local changes, "+
		"the next run finishes the bootstrap and enables the service", checkout, why, c.Path(huyangBootstrapPending))
}

// huyangStart brings a freshly seeded unit up and proves it answers the probe.
// With restart, a unit that was already running (a crash after it was enabled,
// or a reboot since) is restarted, because enable --now leaves a running unit
// serving the binary it started with.
func huyangStart(c *Context, spec map[string]any, checkout, service string, restart bool) error {
	if _, err := c.Must([]string{"systemctl", "--user", "daemon-reload"},
		0, "", "Huyang unit reload"); err != nil {
		return err
	}
	if _, err := c.Must([]string{"systemctl", "--user", "enable", "--now", service},
		60*time.Second, "", "Huyang service start"); err != nil {
		return err
	}
	if restart {
		if _, err := c.Must([]string{"systemctl", "--user", "restart", service},
			60*time.Second, "", "Huyang service restart"); err != nil {
			return err
		}
		// The socket the previous process left can still be there when
		// restart returns, before the new process listens; settle as the
		// ordinary update path does before probing.
		if err := huyangWaitForSocket(c); err != nil {
			return err
		}
		time.Sleep(huyangSettleDelay)
	}
	probe, err := huyangWaitProbe(c, checkout, huyangProbeTools(spec))
	if err != nil {
		return err
	}
	if !probe.OK {
		return errors.New("Huyang bootstrap probe failed: " + huyangProbeRepr(probe))
	}
	return nil
}
