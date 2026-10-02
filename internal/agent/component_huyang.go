package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
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
// would put every unit ordered after huyang.service in a cycle with
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
	// Unknown (nil) when the unit cannot be read; apply's check then fails
	// with the reason.
	observed["unit_ordering_cycle"] = nil
	if cycle, err := userUnitOrderingCycle(
		c.Path(userUnitDir), adapterSpecString(spec, "service"), nil); err == nil {
		observed["unit_ordering_cycle"] = cycle
	}
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
	if commit, _ := before["commit"].(string); commit != "" {
		var err error
		if pending, err = huyangReconcileUnit(c, spec, actions); err != nil {
			return "", err
		}
	}
	state, err := huyangApplyCheckout(c, spec, before, actions)
	if err != nil {
		return state, err
	}
	if err := checkUserUnitOrdering(c, pending); err != nil {
		return "", err
	}
	if state == "converged" && pending != nil {
		state = "updated"
	}
	return state, nil
}

// huyangReconcileUnit removes After=default.target from an installed Huyang
// unit and changes nothing else in it. Units seeded before that line was
// dropped from the template still carry it, and the bootstrap never replaces
// an existing unit.
//
// Ordering only decides how systemd schedules jobs, so a daemon-reload is
// enough and a running Huyang is not restarted. A unit that is a symlink or
// not a regular file is left alone; the ordering check then names it.
//
// The returned map holds the rewritten content when the unit changed (or
// would have, in a dry run), keyed by unit name, and is nil otherwise.
func huyangReconcileUnit(
	c *Context, spec map[string]any, actions *[]string,
) (map[string][]byte, error) {
	service := adapterSpecString(spec, "service")
	unit := c.Path(userUnitDir, service)
	info, err := os.Lstat(unit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	raw, err := os.ReadFile(unit)
	if err != nil {
		return nil, err
	}
	reconciled, changed := unitDropOrderingAfter(raw, userUnitBootTarget)
	if !changed {
		return nil, nil
	}
	pending := map[string][]byte{service: reconciled}
	if c.DryRun {
		*actions = append(*actions, "would remove After=default.target from "+service)
		return pending, nil
	}
	if err := fsatomic.WriteFile(unit, reconciled, info.Mode().Perm()); err != nil {
		return nil, err
	}
	if _, err := c.Must([]string{"systemctl", "--user", "daemon-reload"},
		0, "", "Huyang unit reload"); err != nil {
		return nil, err
	}
	*actions = append(*actions, "removed After=default.target from "+service+
		"; reloaded systemd without restarting Huyang")
	return pending, nil
}

// huyangApplyCheckout moves the checkout to the pinned commit, or seeds Huyang
// on a host that has none.
func huyangApplyCheckout(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	checkout := c.Path(adapterSpecString(spec, "path"))
	commit, _ := before["commit"].(string)
	if commit == "" {
		return huyangBootstrap(c, spec, actions)
	}
	if dirty, _ := before["dirty"].(bool); dirty {
		return "", Failf("%s has local changes; refusing to move it", checkout)
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
func huyangBootstrap(c *Context, spec map[string]any, actions *[]string) (string, error) {
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
	// The directory is an exclusive claim: even an empty pre-existing directory
	// at the live path belongs to whoever made it.
	if err := os.Mkdir(checkout, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(candidate, checkout); err != nil {
		return "", err
	}
	if err := os.Remove(stage); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return "", err
	}
	handle, err := os.OpenFile(unit, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := handle.WriteString(fmt.Sprintf(huyangUnitTemplate, relative)); err != nil {
		_ = handle.Close()
		return "", err
	}
	if err := handle.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(unit, 0o644); err != nil {
		return "", err
	}
	if err := huyangStart(c, spec, checkout, service); err != nil {
		// Keep the candidate bytes and the unit file for diagnosis; the
		// scheduler's host hold is what prevents a blind retry.
		if _, stopErr := c.Must(
			[]string{"systemctl", "--user", "disable", "--now", service},
			60*time.Second, "", "stop failed Huyang bootstrap",
		); stopErr != nil {
			return "", stopErr
		}
		return "", err
	}
	*actions = append(*actions, fmt.Sprintf("installed Huyang %s; enabled %s",
		adapterShortCommit(commit), service))
	return "updated", nil
}

// huyangStart brings a freshly seeded unit up and proves it answers the probe.
func huyangStart(c *Context, spec map[string]any, checkout, service string) error {
	if _, err := c.Must([]string{"systemctl", "--user", "daemon-reload"},
		0, "", "Huyang unit reload"); err != nil {
		return err
	}
	if _, err := c.Must([]string{"systemctl", "--user", "enable", "--now", service},
		60*time.Second, "", "Huyang service start"); err != nil {
		return err
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
