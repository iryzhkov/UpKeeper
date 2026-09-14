// Package safety validates every value that could reach a command line, a
// filesystem path or a remote host.
//
// Nothing in a manifest, an inventory or an operator's arguments is trusted.
// The patterns and the refusal messages are ported unchanged from the Python
// implementation so that existing operator expectations still hold.
package safety

import (
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/pyfmt"
)

// The shapes every externally supplied identifier must have.
var (
	ComponentPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	HostPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	HostnamePattern  = regexp.MustCompile(
		`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*` +
			`[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	GitCommitPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	MachineHashPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	SSHFingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{42,44}=?$`)
	RunIDPattern          = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)
	scpGitPattern         = regexp.MustCompile(`^git@([A-Za-z0-9.-]+):([A-Za-z0-9._/-]+\.git)$`)
)

// AllowedSourceHosts are the only Git hosts a manifest may name.
var AllowedSourceHosts = map[string]bool{"github.com": true, "git.ryzhkov.dev": true}

// AllowedHomeRoots are the only home subtrees an ordinary component may touch.
var AllowedHomeRoots = map[string]bool{".config": true, ".local": true}

// ValidateComponentName accepts a component name, optionally checking it
// against the set of components that actually exist.
func ValidateComponentName(name string, known map[string]bool) (string, error) {
	if !ComponentPattern.MatchString(name) {
		return "", failure.Validation("invalid component name: %s", pyfmt.Repr(name))
	}
	if known != nil && !known[name] {
		return "", failure.Validation("unknown component: %s", name)
	}
	return name, nil
}

// ValidateHostName accepts an inventory host selector.
func ValidateHostName(name string, known map[string]bool) (string, error) {
	if !HostPattern.MatchString(name) {
		return "", failure.Validation("invalid host selector: %s", pyfmt.Repr(name))
	}
	if known != nil && !known[name] {
		return "", failure.Validation("unknown host: %s", name)
	}
	return name, nil
}

// ParseHostSelector expands a --limit value. An absent selector means every
// known host; patterns and ranges are deliberately not supported.
func ParseHostSelector(raw *string, known map[string]bool) ([]string, error) {
	if raw == nil {
		return SortedKeys(known), nil
	}
	value := *raw
	if value == "" || strings.TrimSpace(value) != value {
		return nil, failure.Validation("host selector may not be empty or padded")
	}
	parts := strings.Split(value, ",")
	selected := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	duplicated := false
	for _, part := range parts {
		if part == "" {
			return nil, failure.Validation("host selector contains an empty entry")
		}
		name, err := ValidateHostName(part, known)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			duplicated = true
		}
		seen[name] = true
		selected = append(selected, name)
	}
	if duplicated {
		return nil, failure.Validation("host selector contains duplicates")
	}
	return selected, nil
}

// ValidateGitCommit insists on a full, lowercase commit identity. Short refs
// and branch names are never immutable enough to deploy from.
func ValidateGitCommit(value string) (string, error) {
	if !GitCommitPattern.MatchString(value) {
		return "", failure.Validation("git refs must be full lowercase 40-character commits")
	}
	return value, nil
}

// ValidateHomeRelativePath accepts a normalized path below one of the allowed
// home roots. Absolute paths, traversal and unnormalized spellings are refused.
func ValidateHomeRelativePath(value string, roots map[string]bool) (string, error) {
	allowed := roots
	if allowed == nil {
		allowed = AllowedHomeRoots
	}
	if value == "" || hasControlCharacter(value) {
		return "", failure.Validation("path is empty or contains control characters")
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return "", failure.Validation(
			"path must be a normalized home-relative path: %s", pyfmt.Repr(value))
	}
	// PurePosixPath, which the Python validator built first, drops "."
	// components before any of the checks below run. A path containing one is
	// therefore refused as unnormalized rather than as traversal, and the
	// refusal strings are part of the contract.
	parts := make([]string, 0, strings.Count(value, "/")+1)
	for _, part := range strings.Split(value, "/") {
		if part != "." {
			parts = append(parts, part)
		}
	}
	for _, part := range parts {
		if part == "" || part == ".." {
			return "", failure.Validation("path traversal is forbidden: %s", pyfmt.Repr(value))
		}
	}
	if len(parts) == 0 {
		// Nothing but "." components. Python indexed parts[0] here and raised
		// IndexError, which escaped as an unexpected failure; refusing gives
		// the same answer without the traceback.
		return "", failure.Validation(
			"path must stay below %s: %s", strings.Join(SortedKeys(allowed), ", "), pyfmt.Repr(value))
	}
	if !allowed[parts[0]] {
		return "", failure.Validation(
			"path must stay below %s: %s", strings.Join(SortedKeys(allowed), ", "), pyfmt.Repr(value))
	}
	if strings.Join(parts, "/") != value {
		return "", failure.Validation("path is not normalized: %s", pyfmt.Repr(value))
	}
	return value, nil
}

// ValidateSourceURL accepts only the Git sources the fleet is allowed to fetch
// from, in either the https or the scp-like spelling.
func ValidateSourceURL(value string) (string, error) {
	refuse := failure.Validation("source URL is not allowed: %s", pyfmt.Repr(value))
	if match := scpGitPattern.FindStringSubmatch(value); match != nil {
		if !AllowedSourceHosts[match[1]] || strings.Contains(match[2], "..") {
			return "", refuse
		}
		return value, nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", refuse
	}
	if parsed.Scheme != "https" || !AllowedSourceHosts[strings.ToLower(parsed.Hostname())] {
		return "", refuse
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", refuse
	}
	if !strings.HasSuffix(parsed.Path, ".git") {
		return "", refuse
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if part == ".." {
			return "", refuse
		}
	}
	return value, nil
}

// ValidateAddress accepts a literal IP address or a DNS name.
func ValidateAddress(value string) (string, error) {
	if net.ParseIP(value) != nil {
		return value, nil
	}
	if len(value) > 253 || !HostnamePattern.MatchString(value) {
		return "", failure.Validation("invalid host address: %s", pyfmt.Repr(value))
	}
	return value, nil
}

// ValidateRunID accepts the sortable run identifier every durable record uses.
func ValidateRunID(value string) (string, error) {
	if !RunIDPattern.MatchString(value) {
		return "", failure.Validation("invalid run ID: %s", pyfmt.Repr(value))
	}
	return value, nil
}

// ValidateUnixUser accepts a short, alphanumeric login name.
func ValidateUnixUser(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	stripped := strings.NewReplacer("_", "", "-", "").Replace(value)
	if stripped == "" {
		return false
	}
	for _, r := range stripped {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

// SortedKeys returns the keys of a set in the order reports present them.
func SortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func hasControlCharacter(value string) bool {
	for _, r := range value {
		if r < 32 {
			return true
		}
	}
	return false
}
