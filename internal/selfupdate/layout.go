// Package selfupdate replaces this host's own UpKeeper binary with the one a
// gated release pins, and can put the previous one back.
//
// The release manifest is the only authority. A pin reaches a host through
// the CI-gated, fast-forward-only refresh, and names the exact size and SHA-256
// of the binary for each architecture; the checksum file published beside the
// download authorizes nothing. A downloaded binary is verified against the pin,
// asked for its version with an empty PATH and a disposable HOME, and only then
// installed as a generation:
//
//	~/.local/lib/upkeeper/generations/<sha256>/upkeeper   the binary, 0500
//	~/.local/lib/upkeeper/generations/<sha256>/.durable   its publication was flushed
//	~/.local/lib/upkeeper/current  -> generations/<name>  relative symlink
//	~/.local/lib/upkeeper/previous -> generations/<name>  relative symlink
//	~/.local/bin/upkeeper -> ../lib/upkeeper/current/upkeeper
//	~/.local/lib/upkeeper/switch.json                     the last switch
//
// Every link is replaced by creating a fresh symlink beside it, renaming it
// over the old one and syncing the directory, so a crash at any point leaves
// current naming a complete generation. A generation is published only once
// its files, its directory and its name have all been flushed, which its
// .durable marker records; nothing switches to a generation without it until
// those flushes have been made again. Three generations are kept, and the
// current and previous ones are never removed.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Locations relative to the home directory.
const (
	LibDirectory = ".local/lib/upkeeper"
	BinaryPath   = ".local/bin/upkeeper"
)

// Keep is how many generations survive a prune.
const Keep = 3

// binaryName is the file inside every generation.
const binaryName = "upkeeper"

// binaryLink is what ~/.local/bin/upkeeper points at once the host is managed.
const binaryLink = "../lib/upkeeper/current/" + binaryName

var (
	releasedName    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	adoptedName     = regexp.MustCompile(`^adopted-([0-9a-f]{64})$`)
	adoptedLinkName = regexp.MustCompile(`^adopted-link-([0-9a-f]{64})$`)
)

// Layout names every path of one home directory's installation.
type Layout struct{ Home string }

// Root is the installation directory.
func (l Layout) Root() string { return filepath.Join(l.Home, LibDirectory) }

// Generations holds one directory per installed binary.
func (l Layout) Generations() string { return filepath.Join(l.Root(), "generations") }

// Current is the link naming the active generation.
func (l Layout) Current() string { return filepath.Join(l.Root(), "current") }

// Previous is the link naming the generation a rollback restores.
func (l Layout) Previous() string { return filepath.Join(l.Root(), "previous") }

// Staging is where a download waits until it has been verified.
func (l Layout) Staging() string { return filepath.Join(l.Root(), "staging") }

// Journal is the append-only record of every switch.
func (l Layout) Journal() string { return filepath.Join(l.Root(), "journal.jsonl") }

// Hold, while it exists, stops Apply: a rollback writes it, so that the next
// scheduled self-pull does not reinstall what was just rolled back.
func (l Layout) Hold() string { return filepath.Join(l.Root(), "held") }

// Switch records the last switch, so that the rollback of a failed update
// can tell whether anything changed the installation after it.
func (l Layout) Switch() string { return filepath.Join(l.Root(), "switch.json") }

// Lock serializes every change to the installation.
func (l Layout) Lock() string { return filepath.Join(l.Root(), ".lock") }

// Binary is the path the systemd units and a person both run.
func (l Layout) Binary() string { return filepath.Join(l.Home, BinaryPath) }

// Generation is the directory of one generation.
func (l Layout) Generation(name string) string { return filepath.Join(l.Generations(), name) }

// GenerationBinary is the binary inside one generation.
func (l Layout) GenerationBinary(name string) string {
	return filepath.Join(l.Generation(name), binaryName)
}

// linkedGeneration reads which generation a link names, or "" when the link
// is absent or names anything other than a generation directory.
func (l Layout) linkedGeneration(link string) string {
	target, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	directory, name := filepath.Split(filepath.Clean(target))
	if filepath.Clean(directory) != "generations" || !validName(name) {
		return ""
	}
	return name
}

// CurrentGeneration names the active generation, or "".
func (l Layout) CurrentGeneration() string { return l.linkedGeneration(l.Current()) }

// PreviousGeneration names the generation a rollback restores, or "".
func (l Layout) PreviousGeneration() string { return l.linkedGeneration(l.Previous()) }

// ActiveGeneration names the generation ~/.local/bin/upkeeper runs on this
// home directory, or "" when the binary is not managed by generations.
func ActiveGeneration(home string) string {
	layout := Layout{Home: home}
	if target, err := os.Readlink(layout.Binary()); err != nil || target != binaryLink {
		return ""
	}
	return layout.CurrentGeneration()
}

func validName(name string) bool {
	return releasedName.MatchString(name) || adoptedName.MatchString(name) ||
		adoptedLinkName.MatchString(name)
}

// validGeneration reports whether a generation holds exactly what its name
// says: the bytes whose digest it is named after, or for an adopted link, the
// link target whose digest it is named after.
func (l Layout) validGeneration(name string) bool {
	if !validName(name) {
		return false
	}
	path := l.GenerationBinary(name)
	if match := adoptedLinkName.FindStringSubmatch(name); match != nil {
		target, err := os.Readlink(path)
		return err == nil && digestOf([]byte(target)) == match[1]
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	digest, err := fileDigest(path)
	if err != nil {
		return false
	}
	return digest == strings.TrimPrefix(name, "adopted-")
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileDigest(path string) (string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = handle.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
