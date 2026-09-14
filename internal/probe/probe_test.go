package probe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/transport"
)

// scriptedRunner answers the commands a test names and refuses every other
// command, so that a probe which reaches for something unexpected is visible in
// the result rather than silently succeeding.
func scriptedRunner(script map[string]procx.Result) procx.Runner {
	return func(command procx.Command) procx.Result {
		if result, found := script[strings.Join(command.Argv, " ")]; found {
			return result
		}
		return procx.Result{Code: 1}
	}
}

// remoteHost is an inventory host that is certainly not this machine, so that
// every test of the remote path stays on the remote path.
func remoteHost() inventory.HostSpec {
	return inventory.HostSpec{
		Name:            "laptop",
		Address:         "10.0.0.2",
		User:            "igor",
		MachineIDSHA256: strings.Repeat("0", 64),
	}
}

func TestHostRendersEveryDocumentedProbeOutcome(t *testing.T) {
	payload := `{"hostname": "laptop", "locks": [], "reachable": true}`
	tests := []struct {
		name          string
		result        procx.Result
		wantReachable bool
		wantError     any
		wantTransport any
		wantReturn    any
	}{
		{
			name:          "a non-zero exit status is a failed probe",
			result:        procx.Result{Code: 7, Stderr: "boom"},
			wantError:     "probe-failed",
			wantTransport: "ssh",
			wantReturn:    7,
		},
		{
			name:          "output that is not an object is invalid",
			result:        procx.Result{Code: 0, Stdout: "[1, 2, 3]"},
			wantError:     "invalid-probe-output",
			wantTransport: "ssh",
		},
		{
			name:          "output that is not JSON at all is invalid",
			result:        procx.Result{Code: 0, Stdout: "Welcome to the host\n"},
			wantError:     "invalid-probe-output",
			wantTransport: "ssh",
		},
		{
			name: "a target without the binary is diagnosed",
			result: procx.Result{
				Code:   127,
				Stderr: "bash: line 1: .local/bin/upkeeper: No such file or directory",
			},
			wantError:     transport.MissingBinaryDiagnosis,
			wantTransport: "ssh",
		},
		{
			name: "a timeout reports no transport because nothing ran",
			result: procx.Result{
				Code:     -1,
				TimedOut: true,
				Err:      &procx.TimeoutError{Argv: []string{"ssh"}, Timeout: time.Second},
			},
			wantError: "TimeoutExpired",
		},
		{
			name:      "a command that could not start reports no transport",
			result:    procx.Result{Code: -1, Err: os.ErrNotExist},
			wantError: "OSError",
		},
		{
			name:          "a valid payload is a reachable observation",
			result:        procx.Result{Code: 0, Stdout: payload},
			wantReachable: true,
			wantTransport: "ssh",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := func(procx.Command) procx.Result { return test.result }
			document := Host(remoteHost(), 20*time.Second, runner)

			if document["reachable"] != test.wantReachable {
				t.Fatalf("reachable = %v, want %v", document["reachable"], test.wantReachable)
			}
			if test.wantError != nil && document["error"] != test.wantError {
				t.Errorf("error = %v, want %v", document["error"], test.wantError)
			}
			if test.wantReachable {
				if _, found := document["error"]; found {
					t.Errorf("a reachable observation must carry no error: %v", document)
				}
				if document["hostname"] != "laptop" {
					t.Errorf("hostname = %v, want laptop", document["hostname"])
				}
			}
			if test.wantTransport == nil {
				if _, found := document["transport"]; found {
					t.Errorf("transport must be absent when no command ran: %v", document)
				}
			} else if document["transport"] != test.wantTransport {
				t.Errorf("transport = %v, want %v", document["transport"], test.wantTransport)
			}
			if test.wantReturn != nil && document["returncode"] != test.wantReturn {
				t.Errorf("returncode = %v, want %v", document["returncode"], test.wantReturn)
			}
		})
	}
}

func TestHostInvokesTheTargetBinaryOverStrictSSH(t *testing.T) {
	var observed []string
	runner := func(command procx.Command) procx.Result {
		observed = command.Argv
		return procx.Result{Code: 0, Stdout: "{}"}
	}
	Host(remoteHost(), 20*time.Second, runner)

	joined := strings.Join(observed, " ")
	for _, required := range []string{
		"BatchMode=yes",
		"StrictHostKeyChecking=yes",
		"igor@10.0.0.2",
		transport.RemoteBinary + " " + Subcommand,
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("argument vector %q is missing %q", joined, required)
		}
	}
	if strings.Contains(joined, "python") {
		t.Errorf("the production path must not invoke Python: %q", joined)
	}
}

func TestFleetBoundsTheTimeoutAnOperatorMayAskFor(t *testing.T) {
	config := &inventory.Config{Hosts: map[string]inventory.HostSpec{"laptop": remoteHost()}}
	tests := []struct {
		name        string
		timeout     time.Duration
		wantRefusal bool
	}{
		{"below the lower bound", 4 * time.Second, true},
		{"at the lower bound", MinimumTimeout, false},
		{"at the upper bound", MaximumTimeout, false},
		{"above the upper bound", 61 * time.Second, true},
		{"no timeout at all", 0, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := func(procx.Command) procx.Result {
				return procx.Result{Code: 0, Stdout: `{"hostname": "laptop"}`}
			}
			observations, err := fleet(config, []string{"laptop"}, test.timeout, runner)
			if !test.wantRefusal {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				if len(observations) != 1 {
					t.Fatalf("observations = %v, want one entry", observations)
				}
				return
			}
			if err == nil {
				t.Fatal("an out-of-range timeout must be refused")
			}
			if err.Error() != "timeout must be between 5 and 60 seconds" {
				t.Errorf("message = %q", err.Error())
			}
			if !failure.Is(err, failure.KindValue) {
				t.Errorf("a refused timeout must be a value failure: %v", err)
			}
		})
	}
}

func TestFleetObservesEverySelectedHostAndRefusesUnknownOnes(t *testing.T) {
	config := &inventory.Config{Hosts: map[string]inventory.HostSpec{
		"laptop":   remoteHost(),
		"normandy": {Name: "normandy", Address: "10.0.0.1", User: "igor", MachineIDSHA256: strings.Repeat("1", 64)},
	}}
	runner := func(command procx.Command) procx.Result {
		// The destination is the seventh argument of the strict SSH vector.
		return procx.Result{Code: 0, Stdout: `{"hostname": "` + command.Argv[7] + `"}`}
	}
	selected := []string{"normandy", "laptop"}
	observations, err := fleet(config, selected, DefaultTimeout, runner)
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	for _, name := range selected {
		document, found := observations[name].(map[string]any)
		if !found {
			t.Fatalf("%s has no observation in %v", name, observations)
		}
		if document["reachable"] != true {
			t.Errorf("%s reachable = %v, want true", name, document["reachable"])
		}
	}

	if _, err := fleet(config, []string{"missing"}, DefaultTimeout, runner); err == nil {
		t.Fatal("a selection naming an unknown host must be refused")
	}
}

// collectFixture builds a home directory and a /proc that a probe can observe
// without touching the machine the test runs on.
func collectFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, relative := range []string{
		".local/bin",
		".config/t3-steward",
		".local/state/dev-fleet/locks/normandy.lock",
		".local/state/dev-fleet/locks/ignored",
	} {
		if err := os.MkdirAll(filepath.Join(home, relative), 0o755); err != nil {
			t.Fatalf("cannot build the fixture: %v", err)
		}
	}
	// A file that merely ends in .lock is not a lock; only a directory is.
	lockFile := filepath.Join(home, ".local/state/dev-fleet/locks/stray.lock")
	if err := os.WriteFile(lockFile, []byte(""), 0o644); err != nil {
		t.Fatalf("cannot build the fixture: %v", err)
	}

	release := filepath.Join(t.TempDir(), "os-release")
	content := "NAME=\"Debian GNU/Linux\"\nID=debian\nVERSION_ID=\"12\"\nPRETTY_NAME=x\n"
	if err := os.WriteFile(release, []byte(content), 0o644); err != nil {
		t.Fatalf("cannot build the fixture: %v", err)
	}
	previous := osReleasePath
	osReleasePath = release
	t.Cleanup(func() { osReleasePath = previous })

	process := t.TempDir()
	for name, cmdline := range map[string]string{
		"1471": "/usr/bin/agent99-bridge\x00--socket\x00/run/user/1000/agent99\x00",
		"908":  "/usr/bin/sshd\x00",
		"self": "/usr/bin/agent99-bridge\x00",
		"1290": "/usr/bin/agent99-bridge\x00",
	} {
		directory := filepath.Join(process, name)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("cannot build the fixture: %v", err)
		}
		if err := os.WriteFile(filepath.Join(directory, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatalf("cannot build the fixture: %v", err)
		}
		if err := os.Symlink("/usr/bin/agent99-bridge", filepath.Join(directory, "exe")); err != nil {
			t.Fatalf("cannot build the fixture: %v", err)
		}
		if err := os.Symlink(home, filepath.Join(directory, "cwd")); err != nil {
			t.Fatalf("cannot build the fixture: %v", err)
		}
	}
	previousProc := procPath
	procPath = process
	t.Cleanup(func() { procPath = previousProc })

	// An empty PATH keeps the observation independent of whatever the machine
	// running the test happens to have installed.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("LOGNAME", "igor")
	return home
}

func TestCollectObservesTheHostWithoutPython(t *testing.T) {
	home := collectFixture(t)
	runner := scriptedRunner(map[string]procx.Result{
		"uname -m":                                  {Code: 0, Stdout: "x86_64\n"},
		"systemctl --user is-system-running":        {Code: 0, Stdout: "running\n"},
		"loginctl show-user igor -p Linger --value": {Code: 0, Stdout: "yes\n"},
		"systemctl --user show t3-steward.service -p LoadState -p ActiveState " +
			"-p SubState -p UnitFileState": {
			Code: 0,
			Stdout: "LoadState=loaded\nActiveState=active\nSubState=running\n" +
				"UnitFileState=enabled\n",
		},
	})

	document := collect(home, runner)

	if document["architecture"] != "x86_64" {
		t.Errorf("architecture = %v, want x86_64", document["architecture"])
	}
	if document["user"] != "igor" {
		t.Errorf("user = %v, want igor", document["user"])
	}
	operatingSystem := document["os"].(map[string]any)
	if operatingSystem["id"] != "debian" || operatingSystem["version"] != "12" {
		t.Errorf("os = %v, want debian 12", operatingSystem)
	}
	systemd := document["systemd_user"].(map[string]any)
	if systemd["state"] != "running" || systemd["linger"] != "yes" {
		t.Errorf("systemd_user = %v, want running and yes", systemd)
	}

	// Python is reported for compatibility only, and an absent one is a null
	// rather than a failure.
	python := document["python"].(map[string]any)
	if python["version"] != nil || python["executable"] != nil {
		t.Errorf("python = %v, want nulls when no interpreter is installed", python)
	}
	ansible := document["ansible"].(map[string]any)
	if ansible["available"] != false || ansible["version"] != nil {
		t.Errorf("ansible = %v, want unavailable", ansible)
	}

	roots := document["writable_existing_roots"].(map[string]any)
	if roots[".local"] != true || roots[".local/bin"] != true {
		t.Errorf("writable_existing_roots = %v, want the created roots writable", roots)
	}
	if roots[".local/state"] != true {
		t.Errorf("writable_existing_roots = %v, want .local/state writable", roots)
	}

	locks := document["locks"].([]any)
	if len(locks) != 1 || locks[0] != "normandy.lock" {
		t.Errorf("locks = %v, want only the lock directory", locks)
	}

	if document["forgejo_access"] != false {
		t.Errorf("forgejo_access = %v, want false", document["forgejo_access"])
	}
	sources := document["source_access"].(map[string]any)
	for _, component := range []string{"t3-steward", "agent99", "nvim-configuration", "homelab-cli"} {
		access, found := sources[component].(map[string]any)
		if !found {
			t.Fatalf("source_access has no entry for %s: %v", component, sources)
		}
		if access["reachable"] != false || access["via"] != nil {
			t.Errorf("%s source access = %v, want unreachable", component, access)
		}
	}

	components := document["components"].(map[string]any)
	steward := components["t3-steward"].(map[string]any)
	if steward["command"] != nil || steward["version"] != nil {
		t.Errorf("t3-steward = %v, want nulls when it is not installed", steward)
	}
	service := steward["service"].(map[string]any)
	if service["ActiveState"] != "active" || service["UnitFileState"] != "enabled" {
		t.Errorf("service = %v, want the properties systemctl printed", service)
	}
	configuration := steward["config"].(map[string]any)
	if configuration["exists"] != true || configuration["kind"] != "directory" {
		t.Errorf("config metadata = %v, want an existing directory", configuration)
	}
	if configuration["mode"] != "0755" {
		t.Errorf("config mode = %v, want 0755", configuration["mode"])
	}
	if state := steward["state"].(map[string]any); state["exists"] != false {
		t.Errorf("state metadata = %v, want a missing file", state)
	}

	agent := components["agent99"].(map[string]any)
	bridges := agent["active_bridges"].([]any)
	if len(bridges) != 2 {
		t.Fatalf("active_bridges = %v, want the two bridge processes", bridges)
	}
	first := bridges[0].(map[string]any)
	if first["pid"] != 1290 {
		t.Errorf("active_bridges is not ordered by pid: %v", bridges)
	}
	if first["executable"] != "/usr/bin/agent99-bridge" {
		t.Errorf("executable = %v, want the resolved link", first["executable"])
	}
	if checkout := agent["checkout"].(map[string]any); checkout["present"] != false {
		t.Errorf("checkout = %v, want an absent checkout", checkout)
	}

	configured := components["agent-config"].(map[string]any)
	for _, key := range []string{"config", "codex_skills", "claude_skills"} {
		if _, found := configured[key]; !found {
			t.Errorf("agent-config has no %s entry: %v", key, configured)
		}
	}

	disk := document["disk"].(map[string]any)
	if free, ok := disk["home_free_bytes"].(uint64); !ok || free == 0 {
		t.Errorf("home_free_bytes = %v, want a positive byte count", disk["home_free_bytes"])
	}
}

func TestCollectReportsACheckoutItCanSee(t *testing.T) {
	home := collectFixture(t)
	checkout := filepath.Join(home, ".config/nvim")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("cannot build the fixture: %v", err)
	}
	head := strings.Repeat("a", 40)
	runner := scriptedRunner(map[string]procx.Result{
		"git -C " + checkout + " rev-parse HEAD":                                 {Code: 0, Stdout: head + "\n"},
		"git -C " + checkout + " symbolic-ref --short -q HEAD":                   {Code: 0, Stdout: "main\n"},
		"git -C " + checkout + " status --porcelain=v1 --untracked-files=normal": {Code: 0, Stdout: " M init.lua\n?? notes.md\n"},
	})

	components := collect(home, runner)["components"].(map[string]any)
	observed := components["nvim-configuration"].(map[string]any)["checkout"].(map[string]any)
	if observed["present"] != true || observed["git"] != true {
		t.Fatalf("checkout = %v, want a present Git checkout", observed)
	}
	if observed["head"] != head {
		t.Errorf("head = %v, want %s", observed["head"], head)
	}
	if observed["branch"] != "main" {
		t.Errorf("branch = %v, want main", observed["branch"])
	}
	if observed["dirty_entries"] != 2 {
		t.Errorf("dirty_entries = %v, want 2", observed["dirty_entries"])
	}
}

func TestCollectCallsADetachedCheckoutDetached(t *testing.T) {
	home := collectFixture(t)
	checkout := filepath.Join(home, ".config/nvim")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("cannot build the fixture: %v", err)
	}
	runner := scriptedRunner(map[string]procx.Result{
		"git -C " + checkout + " rev-parse HEAD": {Code: 0, Stdout: strings.Repeat("b", 40)},
	})

	components := collect(home, runner)["components"].(map[string]any)
	observed := components["nvim-configuration"].(map[string]any)["checkout"].(map[string]any)
	if observed["branch"] != "detached" {
		t.Errorf("branch = %v, want detached", observed["branch"])
	}
	if observed["dirty_entries"] != nil {
		t.Errorf("dirty_entries = %v, want null when status failed", observed["dirty_entries"])
	}
}

func TestForgejoAccessAcceptsTheGreetingForgejoActuallySends(t *testing.T) {
	tests := []struct {
		name   string
		result procx.Result
		want   bool
	}{
		{"a clean exit", procx.Result{Code: 0}, true},
		{
			name: "a shell-less greeting",
			result: procx.Result{
				Code:   1,
				Stderr: "Hi there, igor! You've successfully authenticated.",
			},
			want: true,
		},
		{"a refusal", procx.Result{Code: 255, Stderr: "Permission denied (publickey)."}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := func(procx.Command) procx.Result { return test.result }
			if got := forgejoAccess(runner); got != test.want {
				t.Errorf("forgejoAccess = %v, want %v", got, test.want)
			}
		})
	}
}
