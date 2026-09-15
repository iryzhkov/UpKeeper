// Package units installs the systemd user timers that converge a host without
// anybody logging in.
//
// The unit text is embedded in the binary rather than read from the checkout,
// so that a host which has the released binary can install its own timer with
// nothing else present. That is the property the migration is for: a supported
// host needs the binary, Git, OpenSSH and systemd, and no language runtime.
package units

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
)

// BinaryPath is where a host keeps its own upkeeper binary, relative to home.
const BinaryPath = ".local/bin/upkeeper"

// SelfCheckout and FleetCheckout are the only locations their respective timers
// accept. A timer that could run from an arbitrary directory would converge a
// host from whatever tree happened to be there.
const (
	SelfCheckout  = ".local/share/dev-fleet"
	FleetCheckout = "Work/dev-fleet"
)

const selfService = `[Unit]
Description=Refresh the gated UpKeeper release and converge this host

[Service]
Type=oneshot
WorkingDirectory=%h/` + SelfCheckout + `
Environment=PATH=%h/.local/share/mise/shims:%h/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=%h/` + BinaryPath + ` self-pull
TimeoutStartSec=45min
UMask=0077
`

const selfTimer = `[Unit]
Description=Daily local UpKeeper convergence

[Timer]
OnCalendar=*-*-* 09:00
OnStartupSec=5min
RandomizedDelaySec=10m
Persistent=true

[Install]
WantedBy=timers.target
`

const fleetService = `[Unit]
Description=Converge reachable fleet hosts through UpKeeper

[Service]
Type=oneshot
WorkingDirectory=%h/` + FleetCheckout + `
Environment=PATH=%h/.local/share/mise/shims:%h/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=%h/` + BinaryPath + ` scheduled-pull
TimeoutStartSec=3h
UMask=0077
`

const fleetTimer = `[Unit]
Description=Daily UpKeeper fleet convergence on Normandy

[Timer]
OnCalendar=*-*-* 06:30
RandomizedDelaySec=10m
Persistent=true

[Install]
WantedBy=timers.target
`

// Definition is one installable unit.
type Definition struct {
	Name    string
	Content string
}

// SelfTimer is the pair every fleet host installs.
func SelfTimer() []Definition {
	return []Definition{
		{Name: "upkeeper-self.service", Content: selfService},
		{Name: "upkeeper-self.timer", Content: selfTimer},
	}
}

// FleetTimer is the controller pair that belongs on normandy only.
func FleetTimer() []Definition {
	return []Definition{
		{Name: "upkeeper-pull.service", Content: fleetService},
		{Name: "upkeeper-pull.timer", Content: fleetTimer},
	}
}

// InstallSelfTimer installs and starts this host's own convergence timer.
func InstallSelfTimer(root string, runner procx.Runner) ([]string, error) {
	if err := verifyCheckout(root, runner, SelfCheckout, ""); err != nil {
		return nil, err
	}
	return install(SelfTimer(), "upkeeper-self.timer", runner)
}

// InstallFleetTimer installs and starts the controller's daily convergence.
func InstallFleetTimer(root string, runner procx.Runner) ([]string, error) {
	if err := verifyCheckout(root, runner, FleetCheckout, "normandy"); err != nil {
		return nil, err
	}
	return install(FleetTimer(), "upkeeper-pull.timer", runner)
}

// verifyCheckout refuses to install a timer that would converge a host from an
// unexpected tree, from a dirty tree, or on the wrong machine.
func verifyCheckout(root string, runner procx.Runner, expected, requiredHost string) error {
	if _, err := selfconv.CheckoutRevision(root, runner); err != nil {
		return err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return err
	}
	host, err := selfconv.LocalHost(config)
	if err != nil {
		return err
	}
	if requiredHost != "" && host != requiredHost {
		return failure.Validation(
			"this timer belongs on %s only; this machine is %s", requiredHost, host)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return failure.Validation("cannot resolve the home directory: %v", err)
	}
	wanted := filepath.Join(home, expected)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolvedWanted, err := filepath.EvalSymlinks(wanted); err == nil {
		wanted = resolvedWanted
	}
	if root != wanted {
		return failure.Validation("this timer requires ~/%s", expected)
	}
	if _, err := os.Stat(filepath.Join(home, BinaryPath)); err != nil {
		return failure.Validation(
			"install the released binary at ~/%s before enabling the timer", BinaryPath)
	}
	return nil
}

// refuseWhenADropInOwnsIt stops the installer from rewriting a unit that a
// canary drop-in is currently overriding.
//
// The two ways onto the binary are not composable. A drop-in leaves the
// committed unit alone and is rolled back by deleting one file; the installer
// writes the base unit with the binary's ExecStart baked in. A host that has
// both would delete the drop-in, believe it had rolled back, and still run the
// binary. Refusing keeps "delete one file" true wherever it was ever true.
func refuseWhenADropInOwnsIt(destination, unit string) error {
	directory := filepath.Join(destination, unit+".d")
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) == 0 {
		return nil
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".conf") {
			return failure.Validation(
				"%s is overridden by a drop-in in %s; rolling that back is deleting "+
					"the drop-in, and installing the unit here would make the rollback "+
					"silently do nothing. Remove the drop-in first if you want the "+
					"installed unit instead", unit, directory)
		}
	}
	return nil
}

func install(
	definitions []Definition, timer string, runner procx.Runner,
) ([]string, error) {
	if runner == nil {
		runner = procx.Run
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, failure.Validation("cannot resolve the home directory: %v", err)
	}
	destination := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return nil, err
	}
	for _, definition := range definitions {
		if err := refuseWhenADropInOwnsIt(destination, definition.Name); err != nil {
			return nil, err
		}
	}
	written := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		path := filepath.Join(destination, definition.Name)
		if err := os.WriteFile(path, []byte(definition.Content), 0o644); err != nil {
			return nil, err
		}
		written = append(written, path)
	}
	for _, argv := range [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", timer},
	} {
		result := runner(procx.Command{Argv: argv, Timeout: 60 * time.Second})
		if result.Err != nil || result.Code != 0 {
			return written, failure.Validation(
				"%s failed: %s", argv[len(argv)-1], result.Output())
		}
	}
	return written, nil
}
