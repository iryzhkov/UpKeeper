package agent

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
)

func init() {
	Register(fleet.Component, Handler{Observe: fleetObserve, Apply: fleetApply})
}

// fleetAdminReferencePattern is the coordinator-admin credential namespace.
// It is deliberately a different namespace from the worker protocol's, so that
// distributing one never distributes the other.
var fleetAdminReferencePattern = regexp.MustCompile(
	`^secretref:f03-admin/[a-z][a-z0-9-]{0,62}$`)

// fleetProjection decides what this host's fleet intent means for it, and
// writes the worker projection when it differs from what is there.
//
// Only the worker projection is written. The client projection's destination is
// a file t3-steward also authors, so UpKeeper renders it and reports its digest
// rather than giving one document two authors; the coordinator projection is a
// proposal and is never written anywhere by this program.
func fleetProjection(
	c *Context, component map[string]any, host, runID string, dryRun bool,
) (map[string]any, error) {
	document, err := fleet.Parse(component)
	if err != nil {
		return nil, err
	}
	if _, declared := document.Hosts[host]; !declared {
		return map[string]any{"state": "converged", "configured": false, "enrolled": false}, nil
	}
	payload, err := document.WorkerBytes(host)
	if err != nil {
		return nil, err
	}
	if err := fleetAgreesWithWorkerConfiguration(c, host, payload); err != nil {
		return nil, err
	}
	if err := fleetResolveCredentials(c, document.Hosts[host]); err != nil {
		return nil, err
	}
	target := c.Path(fleet.WorkerDestination)
	if err := workerPathIsInHome(c.Home, target); err != nil {
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
		if err := fleetWriteProjection(c, runID, target, payload, before); err != nil {
			return nil, err
		}
	}
	return fleetResult(document, host, payload, changed)
}

func fleetResult(
	document *fleet.Document, host string, payload []byte, changed bool,
) (map[string]any, error) {
	client, err := document.ClientBytes(host)
	if err != nil {
		return nil, err
	}
	catalog, err := document.CoordinatorBytes()
	if err != nil {
		return nil, err
	}
	sets, err := document.Models(host)
	if err != nil {
		return nil, err
	}
	state := "converged"
	if changed {
		state = "updated"
	}
	result := map[string]any{
		"state":          state,
		"configured":     true,
		"worker_sha256":  agentenv.Digest(payload),
		"client_sha256":  agentenv.Digest(client),
		"catalog_sha256": agentenv.Digest(catalog),
		"credential_ref": document.Hosts[host].CredentialRef,
		"desired_models": fleetModelMap(sets.Desired),
		// Convergence never enrolls. Applying a catalog is a deliberate
		// Steward admin operation with its own idempotency key and revision
		// fence, because an accidental re-enrollment invalidates in-flight
		// offers on a live fleet.
		"enrolled": false,
	}
	// A host with no observation and a host observed to have nothing are
	// different answers, so the absent case is absent rather than empty.
	if sets.HasObservation {
		result["observed_models"] = fleetModelMap(sets.Observed)
	}
	return result, nil
}

func fleetModelMap(values map[string][]string) map[string]any {
	converted := map[string]any{}
	for instance, allowed := range values {
		items := make([]any, 0, len(allowed))
		for _, model := range allowed {
			items = append(items, model)
		}
		converted[instance] = items
	}
	return converted
}

// fleetAgreesWithWorkerConfiguration refuses a release whose two sources for
// one file disagree.
//
// Both components project the same destination, and the fleet component
// converges second, so a disagreement would silently rewrite what the reviewed
// v1 entry published. Refusing says which release is wrong instead.
func fleetAgreesWithWorkerConfiguration(c *Context, host string, payload []byte) error {
	workers := c.Spec("worker-configuration")
	if workers == nil {
		return nil
	}
	hosts, _ := workers["hosts"].(map[string]any)
	entry, present := hosts[host].(map[string]any)
	if !present {
		return nil
	}
	published, err := agentenv.Canonical(entry)
	if err != nil {
		return err
	}
	if !bytes.Equal(published, payload) {
		return errors.New(
			"fleet intent and worker-configuration disagree about this host's bootstrap")
	}
	return nil
}

// fleetResolveCredentials proves both of this host's credential references
// resolve to a file the runtime may use. It reads metadata only: neither value
// nor a hash of one may ever reach a run record.
func fleetResolveCredentials(c *Context, host fleet.Host) error {
	if _, err := workerResolveSecretReference(c.Home, host.CredentialRef); err != nil {
		return err
	}
	_, err := fleetResolveAdminReference(c.Home, host.AdminCredentialRef)
	return err
}

func fleetResolveAdminReference(home, reference string) (string, error) {
	if !fleetAdminReferencePattern.MatchString(reference) {
		return "", errors.New("invalid f03-admin reference")
	}
	root := filepath.Join(home, workerSecretRoot)
	path := filepath.Join(root, reference[len("secretref:"):])
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
		return "", errors.New("secret reference requires a nonempty owner-only 0600 file")
	}
	return path, nil
}

// fleetWriteProjection retains what was there, stages the new document beside
// the target, validates the staged bytes, and only then activates it with one
// rename. A reader therefore sees the previous document or the complete new
// one, never a prefix and never something that does not parse.
func fleetWriteProjection(c *Context, runID, target string, payload, before []byte) error {
	if err := fleetRetainPrior(c, runID, before); err != nil {
		return err
	}
	staged, err := fleetStage(target, payload)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()
	if err := fleetValidateStaged(staged, payload); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		return err
	}
	_ = fsatomic.SyncDirectory(filepath.Dir(target))
	written, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(written, payload) {
		// The document that is live is not the one that was validated, so the
		// retained document is put back rather than left to a later run.
		if restoreErr := FleetRestorePrior(c.Home, runID); restoreErr != nil {
			return RolledBackf(
				"fleet projection failed verification and could not be restored: %v",
				restoreErr)
		}
		return RolledBackf("fleet projection failed verification; restored the previous document")
	}
	return nil
}

// fleetRetainPrior records the document that is about to be replaced, under a
// directory created exclusively so that two runs never share one.
func fleetRetainPrior(c *Context, runID string, before []byte) error {
	backup := c.Path(fleet.StateDirectory, runID)
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(backup, 0o700); err != nil {
		return err
	}
	prior := map[string]any{
		"existed":     before != nil,
		"content":     nil,
		"destination": fleet.WorkerDestination,
	}
	if before != nil {
		prior["content"] = string(before)
	}
	record, err := agentenv.Canonical(prior)
	if err != nil {
		return err
	}
	return agentenv.AtomicWrite(filepath.Join(backup, "prior.json"), record, 0o600)
}

// FleetRestorePrior puts back the document a run replaced.
//
// It is the rollback path for this component, and it is exercised rather than
// assumed: the run that replaced a document is named, its retained record is
// read, and the destination goes back to exactly what that run found, up to and
// including not existing at all.
func FleetRestorePrior(home, runID string) error {
	record, err := os.ReadFile(filepath.Join(home, fleet.StateDirectory, runID, "prior.json"))
	if err != nil {
		return err
	}
	prior, err := canonjson.DecodeObject(record)
	if err != nil {
		return err
	}
	destination, _ := prior["destination"].(string)
	if destination != fleet.WorkerDestination {
		return errors.New("retained fleet record names an unexpected destination")
	}
	target := filepath.Join(home, destination)
	if err := workerPathIsInHome(home, target); err != nil {
		return err
	}
	existed, _ := prior["existed"].(bool)
	if !existed {
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	content, ok := prior["content"].(string)
	if !ok {
		return errors.New("retained fleet record has no content to restore")
	}
	return agentenv.AtomicWrite(target, []byte(content), 0o600)
}

// fleetStage writes the candidate document beside its destination, so that the
// activation below is a rename within one directory and therefore atomic.
func fleetStage(target string, payload []byte) (string, error) {
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	handle, err := os.CreateTemp(directory, "."+filepath.Base(target)+"-*")
	if err != nil {
		return "", err
	}
	name := handle.Name()
	if err := handle.Chmod(0o600); err != nil {
		_ = handle.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := handle.Write(payload); err != nil {
		_ = handle.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := handle.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// fleetValidateStaged reads back what is actually on disk and checks that it is
// the document that was meant to be written and that it still satisfies the
// bootstrap contract. Validating the bytes rather than the intention is what
// makes a truncated or tampered staging file impossible to activate.
func fleetValidateStaged(staged string, payload []byte) error {
	written, err := os.ReadFile(staged)
	if err != nil {
		return err
	}
	if !bytes.Equal(written, payload) {
		return errors.New("staged fleet projection does not match what was rendered")
	}
	entry, err := canonjson.DecodeObject(written)
	if err != nil {
		return errors.New("staged fleet projection is not a JSON object")
	}
	if !workerSameFields(entry, workerEntryFields) {
		return errors.New("staged fleet projection is not a worker bootstrap document")
	}
	return nil
}

// fleetObserve projects in preview mode, which is also the observation: the
// same code decides what a real apply would change.
func fleetObserve(c *Context, spec map[string]any) (map[string]any, error) {
	return fleetProjection(c, spec, c.Host.Name, c.RunID, true)
}

func fleetApply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	result, err := fleetProjection(c, spec, c.Host.Name, c.RunID, c.DryRun)
	if err != nil {
		return "", err
	}
	if configured, _ := result["configured"].(bool); configured {
		*actions = append(*actions,
			"fleet worker projection applied; enrollment stays an explicit Steward operation")
	}
	state, _ := result["state"].(string)
	return state, nil
}
