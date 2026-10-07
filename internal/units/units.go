// Package units installs the systemd user timers that converge a host without
// anybody logging in.
//
// The unit text is embedded in the binary rather than read from the checkout,
// so that a host which has the released binary can install its own timer with
// nothing else present. That is the property the migration is for: a supported
// host needs the binary, Git, OpenSSH and systemd, and no language runtime.
package units

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
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

// legacyDropInName is the file name of the canary drop-ins UpKeeper shipped
// while the Python units were still the base units.
const legacyDropInName = "10-upkeeper-binary.conf"

// legacySelfDropIn is scripts/upkeeper-self.service.d/10-upkeeper-binary.conf
// as it was last shipped, byte for byte (sha256
// 4e466672a17fbd9e886119f9c6f1b62ad56e517f7c02bedd806181400022dd0a).
const legacySelfDropIn = "# Canary drop-in: run the compiled UpKeeper binary instead of the Python\n" +
	"# launcher, without editing the committed unit.\n" +
	"#\n" +
	"# Install it as ~/.config/systemd/user/upkeeper-self.service.d/10-upkeeper-binary.conf\n" +
	"# and run `systemctl --user daemon-reload`. Rolling back is `rm` of this file\n" +
	"# plus another daemon-reload: the committed unit and scripts/upkeeper-self-pull\n" +
	"# are never touched, so the Python path stays exactly one command away.\n" +
	"[Service]\n" +
	"ExecStart=\n" +
	"ExecStart=%h/.local/bin/upkeeper self-pull\n"

// legacyPullDropIn is scripts/upkeeper-pull.service.d/10-upkeeper-binary.conf
// as it was last shipped, byte for byte (sha256
// 9c9db7f1322c079f65aec263fc5de39e0d8c10f0ac41e82cfd81b966ef2da0da).
const legacyPullDropIn = "# Canary drop-in for the controller timer on normandy: run the compiled\n" +
	"# UpKeeper binary instead of the Python launcher, without editing the committed\n" +
	"# unit.\n" +
	"#\n" +
	"# Install it as ~/.config/systemd/user/upkeeper-pull.service.d/10-upkeeper-binary.conf\n" +
	"# and run `systemctl --user daemon-reload`. Rolling back is `rm` of this file\n" +
	"# plus another daemon-reload.\n" +
	"[Service]\n" +
	"ExecStart=\n" +
	"ExecStart=%h/.local/bin/upkeeper scheduled-pull\n"

// legacyDropIns lists, per unit, every content of legacyDropInName that
// UpKeeper itself ever shipped for that unit. Only these exact bytes are
// UpKeeper's to remove; any other drop-in belongs to an operator.
var legacyDropIns = map[string][]string{
	"upkeeper-self.service": {legacySelfDropIn},
	"upkeeper-pull.service": {legacyPullDropIn},
}

// Definition is one installable unit.
type Definition struct {
	Name    string
	Content string
}

// Installation is what an install changed on this host: the unit files it
// wrote and the legacy canary drop-ins (and emptied drop-in directories) it
// removed.
type Installation struct {
	Written []string
	Removed []string
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
func InstallSelfTimer(root string, runner procx.Runner) (Installation, error) {
	if err := verifyCheckout(root, runner, SelfCheckout, ""); err != nil {
		return Installation{}, err
	}
	return install(SelfTimer(), "upkeeper-self.timer", runner)
}

// InstallFleetTimer installs and starts the controller's daily convergence.
func InstallFleetTimer(root string, runner procx.Runner) (Installation, error) {
	if err := verifyCheckout(root, runner, FleetCheckout, "normandy"); err != nil {
		return Installation{}, err
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

// legacyDropInsOf returns the drop-ins of unit that the installer may remove,
// and refuses when the unit has any drop-in it did not ship.
//
// The Python canary rollback has ended: a host leaves a canary drop-in by
// installing the Go base unit, and rolls back with `upkeeper self rollback`.
// UpKeeper's own legacy drop-in only repeated the binary's ExecStart, so it is
// removed. Any other drop-in would silently change the unit the installer is
// about to write, so the installer stops before any effect and names the file.
func legacyDropInsOf(destination, unit string) ([]string, error) {
	directory := filepath.Join(destination, unit+".d")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, failure.Validation(
			"cannot inspect the drop-ins of %s in %s: %v", unit, directory, err)
	}
	var legacy []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		if entry.Name() == legacyDropInName && isLegacyDropIn(unit, path) {
			legacy = append(legacy, path)
			continue
		}
		return nil, failure.Validation(
			"%s has a drop-in UpKeeper did not ship: %s. It would override the unit "+
				"this installer writes, so nothing was installed. Inspect that file; "+
				"move it out of %s if the embedded unit should run as written, then "+
				"run the installer again", unit, path, directory)
	}
	return legacy, nil
}

// isLegacyDropIn reports whether path holds, byte for byte, a drop-in
// UpKeeper shipped for unit. A file that cannot be read is not UpKeeper's.
func isLegacyDropIn(unit, path string) bool {
	known := legacyDropIns[unit]
	if len(known) == 0 {
		return false
	}
	limit := 0
	for _, content := range known {
		limit = max(limit, len(content))
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return false
	}
	for _, candidate := range known {
		if bytes.Equal(content, []byte(candidate)) {
			return true
		}
	}
	return false
}

// writeUnit replaces path through a rename, so that an interrupted install
// leaves either the previous unit or the new one and never half of one, and
// so that a symbolic link at path is replaced rather than written through.
func writeUnit(path, content string) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(name)
		}
	}()
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// install writes the base units, then removes the legacy drop-ins, then
// reloads systemd. Every drop-in is judged before the first write, so a refusal
// changes nothing. Writing the base unit before removing the drop-in means an
// interruption leaves the unit running the binary through one or both, and a
// rerun recognises the same drop-in and finishes.
func install(
	definitions []Definition, timer string, runner procx.Runner,
) (Installation, error) {
	var done Installation
	if runner == nil {
		runner = procx.Run
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return done, failure.Validation("cannot resolve the home directory: %v", err)
	}
	destination := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return done, err
	}
	legacy := map[string][]string{}
	for _, definition := range definitions {
		paths, err := legacyDropInsOf(destination, definition.Name)
		if err != nil {
			return done, err
		}
		legacy[definition.Name] = paths
	}
	for _, definition := range definitions {
		path := filepath.Join(destination, definition.Name)
		if err := writeUnit(path, definition.Content); err != nil {
			return done, err
		}
		done.Written = append(done.Written, path)
	}
	for _, definition := range definitions {
		for _, path := range legacy[definition.Name] {
			err := os.Remove(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return done, failure.Validation(
					"wrote %s but could not remove the legacy drop-in %s: %v; "+
						"run the installer again once it can be removed",
					definition.Name, path, err)
			}
			done.Removed = append(done.Removed, path)
		}
		if len(legacy[definition.Name]) > 0 {
			directory := filepath.Join(destination, definition.Name+".d")
			if info, err := os.Lstat(directory); err == nil && info.IsDir() {
				// A directory that still holds anything is left as it is.
				if os.Remove(directory) == nil {
					done.Removed = append(done.Removed, directory)
				}
			}
		}
	}
	for _, argv := range [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", timer},
	} {
		result := runner(procx.Command{Argv: argv, Timeout: 60 * time.Second})
		if result.Err != nil || result.Code != 0 {
			return done, failure.Validation(
				"%s failed: %s", argv[len(argv)-1], result.Output())
		}
	}
	return done, nil
}
