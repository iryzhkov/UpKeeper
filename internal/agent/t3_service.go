package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// t3ServiceUnit is seeded only when no unit exists at all. A host that has
// edited its own unit keeps every one of its settings, including its bind
// address and credentials.
const t3ServiceUnit = `# Seeded by UpKeeper; yours to edit.
[Unit]
Description=T3 Code server (agent control surface)
Documentation=https://github.com/pingdotgg/t3code
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h
ExecStart=%h/.local/share/mise/shims/t3 serve --host 127.0.0.1 --port 7391 --no-browser
Environment=PATH=%h/.local/share/mise/shims:%h/.local/bin:/usr/local/bin:/usr/bin:/bin
Environment=HOME=%h
EnvironmentFile=-%h/.config/claude/oauth.env
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`

// SeedT3Service creates the T3 unit when the host has none.
//
// It reports whether it seeded anything. An existing unit, in any state, is
// left exactly as the host has it.
func SeedT3Service(c *Context, actions *[]string) (bool, error) {
	path := c.Path(".config/systemd/user/t3code.service")
	status, err := unitStatus(c, "t3code.service")
	if err != nil {
		return false, err
	}
	if pathExists(path) || status["LoadState"] != "not-found" {
		return false, nil
	}
	if !isRegularFile(c.Path(".config/claude/oauth.env")) {
		*actions = append(*actions, "warning: no ~/.config/claude/oauth.env; "+
			"provider authentication needs host provisioning")
	}
	if c.DryRun {
		*actions = append(*actions, "would seed t3code.service with loopback binding "+
			"and optional host-local oauth.env")
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := handle.WriteString(t3ServiceUnit); err != nil {
		_ = handle.Close()
		return false, err
	}
	if err := handle.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return false, err
	}
	if _, err := c.Must([]string{"systemctl", "--user", "daemon-reload"},
		0, "", "reload seeded T3 service"); err != nil {
		return false, err
	}
	if _, err := c.Must([]string{"systemctl", "--user", "enable", "--now", "t3code.service"},
		0, "", "start seeded T3 service"); err != nil {
		return false, err
	}
	*actions = append(*actions, "seeded t3code.service; future host edits are preserved")
	return true, nil
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		return false
	}
	return info.Mode().IsRegular()
}
