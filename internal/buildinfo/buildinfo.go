// Package buildinfo lets a binary say exactly which source commit produced it.
//
// A release artifact that cannot name its own commit is not auditable, and the
// publication gate requires it, so the value is baked in at link time and falls
// back to the version control stamp the Go toolchain embeds.
package buildinfo

import (
	"runtime/debug"
	"sync"
)

// Version is the release version, set with -ldflags at link time.
var Version = "0.0.0-dev"

// Commit is the exact source commit, set with -ldflags at link time.
var Commit = ""

var resolve = sync.OnceValue(func() string {
	if Commit != "" {
		return Commit
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	revision := "unknown"
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if modified {
		return revision + "-dirty"
	}
	return revision
})

// SourceCommit returns the commit this binary was built from.
func SourceCommit() string { return resolve() }

// Summary is the single line `upkeeper version` prints.
func Summary() string {
	return "upkeeper " + Version + " (commit " + SourceCommit() + ")"
}
