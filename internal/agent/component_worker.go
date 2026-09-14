package agent

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
)

func init() {
	Register("worker-configuration",
		Handler{Observe: workerObserve, Apply: workerApply})
}

const (
	// workerDestination is the only file this component owns.
	workerDestination = ".config/t3-steward/worker-bootstrap.json"
	// workerSecretRoot holds the target-local credentials a worker bootstrap
	// points at. The agent resolves metadata here and never reads a value.
	workerSecretRoot = ".config/upkeeper/secrets"
)

var (
	workerNamePattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	workerReferencePattern  = regexp.MustCompile(`^secretref:f02-protocol/[a-z][a-z0-9-]{0,62}$`)
	workerIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	// workerEntryFields is the complete bootstrap contract. Anything else, in
	// particular a catalog or an inline secret, is refused rather than ignored.
	workerEntryFields = []string{
		"schema_version",
		"worker_id",
		"coordinator_id",
		"transport",
		"capabilities",
		"provider_routes",
		"credential_ref",
	}
)

// workerSameFields reports whether an object has exactly the expected keys.
func workerSameFields(object map[string]any, expected []string) bool {
	if len(object) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, present := object[key]; !present {
			return false
		}
	}
	return true
}

// workerValidate refuses a worker-configuration component that carries anything
// beyond the opaque bootstrap contract.
func workerValidate(component map[string]any) error {
	if !workerSameFields(component, []string{"kind", "hosts"}) ||
		adapterSpecString(component, "kind") != "worker-configuration" {
		return errors.New("worker-configuration must contain only kind and hosts")
	}
	hosts, ok := component["hosts"].(map[string]any)
	if !ok {
		return errors.New("worker hosts must be an object")
	}
	for _, host := range SortedNames(hosts) {
		entry, isObject := hosts[host].(map[string]any)
		if !workerNamePattern.MatchString(host) || !isObject {
			return errors.New("invalid worker host")
		}
		if !workerSameFields(entry, workerEntryFields) {
			return errors.New(
				"unknown worker bootstrap fields; catalogs and secrets are forbidden")
		}
		schema, numbered := adapterSpecInt(entry, "schema_version")
		if !numbered || schema != 1 || adapterSpecString(entry, "worker_id") != host {
			return errors.New("worker identity must match its inventory host")
		}
		if !workerNamePattern.MatchString(adapterSpecString(entry, "coordinator_id")) {
			return errors.New("invalid coordinator identity")
		}
		if adapterSpecString(entry, "transport") != "ssh" {
			return errors.New("remote worker bootstrap transport must be ssh")
		}
		if adapterSpecString(entry, "credential_ref") != "secretref:f02-protocol/"+host {
			return errors.New(
				"worker needs its own opaque f02-protocol secret reference")
		}
		if err := workerValidateIdentifierLists(entry); err != nil {
			return err
		}
		capabilities := adapterSpecStrings(entry, "capabilities")
		if !workerContains(capabilities, "git") || !workerContains(capabilities, "huyang") {
			return errors.New("worker must declare git and huyang")
		}
	}
	return nil
}

func workerValidateIdentifierLists(entry map[string]any) error {
	for _, field := range []string{"capabilities", "provider_routes"} {
		items, isList := entry[field].([]any)
		if !isList {
			return errors.New("capabilities and provider routes must be sorted and unique")
		}
		values := make([]string, 0, len(items))
		for _, item := range items {
			text, isText := item.(string)
			if !isText {
				return errors.New("invalid capability or route identifier")
			}
			values = append(values, text)
		}
		if !workerSortedAndUnique(values) {
			return errors.New(
				"capabilities and provider routes must be sorted and unique")
		}
		for _, value := range values {
			if !workerIdentifierPattern.MatchString(value) {
				return errors.New("invalid capability or route identifier")
			}
		}
	}
	return nil
}

func workerSortedAndUnique(values []string) bool {
	return sort.SliceIsSorted(values, func(left, right int) bool {
		return values[left] < values[right]
	}) && !workerHasDuplicate(values)
}

func workerHasDuplicate(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return true
		}
	}
	return false
}

func workerContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// workerResolveSecretReference resolves a credential reference to the file that
// holds it, and proves that file is safe to hand to the runtime.
//
// It resolves metadata only. Neither the value nor a hash of it may ever reach
// a run record, so this function never opens the file.
func workerResolveSecretReference(home, reference string) (string, error) {
	if !workerReferencePattern.MatchString(reference) {
		return "", errors.New("invalid f02-protocol reference")
	}
	root := filepath.Join(home, workerSecretRoot)
	path := filepath.Join(root, strings.TrimPrefix(reference, "secretref:"))
	if err := workerPathIsContained(root, path); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	system, described := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !described ||
		int(system.Uid) != os.Getuid() || info.Mode().Perm() != 0o600 || info.Size() == 0 {
		return "", errors.New(
			"secret reference requires a nonempty owner-only 0600 file")
	}
	return path, nil
}

// workerPathIsContained refuses a symlink at the leaf and any resolved path
// that leaves the target-local store.
func workerPathIsContained(root, path string) error {
	escape := errors.New("secret reference escapes its target-local store")
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return escape
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = filepath.Clean(root)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		resolvedParent = filepath.Clean(filepath.Dir(path))
	}
	resolved := filepath.Join(resolvedParent, filepath.Base(path))
	if resolved != resolvedRoot &&
		!strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator)) {
		return escape
	}
	return nil
}

// workerProjection writes this host's worker bootstrap, and reports what it
// found or changed.
//
// Enrollment is not part of this: the file says which coordinator to talk to
// and which opaque reference holds the credential, and the runtime does the
// rest.
func workerProjection(
	c *Context, component map[string]any, host, runID string, dryRun bool,
) (map[string]any, error) {
	if err := workerValidate(component); err != nil {
		return nil, err
	}
	hosts, _ := component["hosts"].(map[string]any)
	entry, present := hosts[host].(map[string]any)
	if !present {
		return map[string]any{"state": "converged", "configured": false}, nil
	}
	reference := adapterSpecString(entry, "credential_ref")
	if _, err := workerResolveSecretReference(c.Home, reference); err != nil {
		return nil, err
	}
	target := c.Path(workerDestination)
	if err := workerPathIsInHome(c.Home, target); err != nil {
		return nil, err
	}
	payload, err := agentenv.Canonical(entry)
	if err != nil {
		return nil, err
	}
	before, err := os.ReadFile(target)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		before = nil
	}
	changed := before == nil || !bytes.Equal(before, payload)
	if changed && !dryRun {
		if err := workerWriteProjection(c, runID, target, payload, before); err != nil {
			return nil, err
		}
	}
	state := "converged"
	if changed {
		state = "updated"
	}
	return map[string]any{
		"state":                state,
		"configured":           true,
		"configuration_sha256": agentenv.Digest(payload),
		"credential_ref":       reference,
		"enrolled":             false,
	}, nil
}

// workerWriteProjection records what was there and then writes the new file.
func workerWriteProjection(
	c *Context, runID, target string, payload, before []byte,
) error {
	backup := c.Path(".local/state/dev-fleet/worker-config", runID)
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return err
	}
	// The backup directory is created exclusively: two runs never share one.
	if err := os.Mkdir(backup, 0o700); err != nil {
		return err
	}
	prior := map[string]any{"existed": before != nil, "content": nil}
	if before != nil {
		prior["content"] = string(before)
	}
	record, err := agentenv.Canonical(prior)
	if err != nil {
		return err
	}
	if err := agentenv.AtomicWrite(filepath.Join(backup, "prior.json"), record, 0o600); err != nil {
		return err
	}
	if err := agentenv.AtomicWrite(target, payload, 0o600); err != nil {
		return err
	}
	written, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(written, payload) {
		return errors.New("worker bootstrap failed verification")
	}
	return nil
}

// workerPathIsInHome refuses a destination that is a symlink or that resolves
// outside the home directory.
func workerPathIsInHome(home, target string) error {
	unsafe := errors.New("unsafe worker bootstrap target")
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return unsafe
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		resolvedHome = filepath.Clean(home)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		resolvedParent = filepath.Clean(filepath.Dir(target))
	}
	resolved := filepath.Join(resolvedParent, filepath.Base(target))
	if resolved != resolvedHome &&
		!strings.HasPrefix(resolved, resolvedHome+string(os.PathSeparator)) {
		return unsafe
	}
	return nil
}

// workerObserve projects in preview mode, which is also the observation: the
// same code decides what a real apply would change.
func workerObserve(c *Context, spec map[string]any) (map[string]any, error) {
	return workerProjection(c, spec, c.Host.Name, c.RunID, true)
}

func workerApply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	result, err := workerProjection(c, spec, c.Host.Name, c.RunID, c.DryRun)
	if err != nil {
		return "", err
	}
	if configured, _ := result["configured"].(bool); configured {
		*actions = append(*actions, "worker bootstrap projected; enrollment belongs to S4")
	}
	state, _ := result["state"].(string)
	return state, nil
}
