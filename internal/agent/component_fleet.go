package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

// fleetDocument is one file this component owns on the host. A document with no
// payload is one this host must not have: it is removed rather than written.
type fleetDocument struct {
	destination string
	target      string
	payload     []byte
	remove      bool
	before      []byte
	changed     bool
}

// fleetProjection decides what this host's fleet intent means for it, and
// writes the documents that differ from what is there.
//
// Worker and client projections are scoped to worker hosts. The coordinator
// receives the authored catalog input even when it is not itself a worker.
func fleetProjection(
	c *Context, component map[string]any, host, runID string, dryRun bool,
) (map[string]any, error) {
	if component == nil {
		return fleetRetireCoordinator(c, runID, dryRun)
	}
	document, err := fleet.Parse(component)
	if err != nil {
		return nil, err
	}
	_, declared := document.Hosts[host]
	coordinator := host == document.Coordinator.Host
	if !declared && !coordinator {
		return fleetRetireCoordinator(c, runID, dryRun)
	}
	var worker, catalog []byte
	if declared {
		worker, err = document.WorkerBytes(host)
		if err != nil {
			return nil, err
		}
		if err := fleetAgreesWithWorkerConfiguration(c, host, worker); err != nil {
			return nil, err
		}
	}
	if coordinator {
		catalog, err = document.CoordinatorBytes()
		if err != nil {
			return nil, err
		}
	}
	// The coordinator administers itself over its own local socket, and its
	// runtime refuses to start while a remote client endpoint is configured.
	// It gets its worker bootstrap like any other host and no client document,
	// and one left over from when it was only a worker is removed.
	var client []byte
	if declared && document.NeedsClientProjection(host) {
		rendered, err := document.ClientBytes(host)
		if err != nil {
			return nil, err
		}
		client = rendered
	}
	if declared {
		if err := fleetResolveCredentials(c, document.Hosts[host], client != nil); err != nil {
			return nil, err
		}
	}
	documents, err := fleetPlanDocuments(c, worker, client, catalog)
	if err != nil {
		return nil, err
	}
	changed := false
	for _, planned := range documents {
		changed = changed || planned.changed
	}
	if changed && !dryRun {
		if err := fleetWriteProjection(c, runID, documents); err != nil {
			return nil, err
		}
	}
	if !declared {
		state := "converged"
		if changed {
			state = "updated"
		}
		return map[string]any{
			"state": state, "configured": true, "enrolled": false,
			"coordinator_projection": map[string]any{
				"purpose": fleet.CoordinatorPurpose, "sha256": agentenv.Digest(catalog),
			},
		}, nil
	}
	return fleetResult(document, host, worker, client, changed)
}

// fleetRetireCoordinator removes only the private coordinator projection when
// historical or current intent no longer assigns this host that ownership.
// Other host documents are outside this transition. Unknown formats and unsafe
// filesystem objects require inspection; their contents are never discarded.
func fleetRetireCoordinator(c *Context, runID string, dryRun bool) (map[string]any, error) {
	result := map[string]any{"state": "converged", "configured": false, "enrolled": false}
	path := c.Path(fleet.CoordinatorDestination)
	if err := workerPathIsInHome(c.Home, path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || int(owner.Uid) != os.Getuid() {
		return nil, errors.New("refusing to retire an unsafe coordinator fleet document")
	}
	before, err := fleetReadRetirementCoordinator(path, info)
	if err != nil {
		return nil, err
	}
	entry, err := canonjson.DecodeObject(before)
	if err != nil || !workerSameFields(entry, []string{"kind", "schema_version", "coordinator_id", "workers", "projects"}) ||
		entry["kind"] != fleet.CoordinatorKind || entry["schema_version"] != json.Number("1") ||
		!fleetKnownCoordinatorChildren(entry) {
		return nil, errors.New("refusing to retire an unknown coordinator fleet document")
	}
	result["state"] = "updated"
	if dryRun {
		return result, nil
	}
	planned := &fleetDocument{destination: fleet.CoordinatorDestination, target: path,
		remove: true, before: before, changed: true}
	if err := fleetWriteProjection(c, runID, []*fleetDocument{planned}); err != nil {
		return nil, err
	}
	return result, nil
}

// fleetPlanDocuments works out what each owned file would become, in a stable
// order, before anything is written. Deciding first and writing second is what
// lets an observation run exactly the code an apply does.
func fleetPlanDocuments(c *Context, worker, client []byte, catalog ...[]byte) ([]*fleetDocument, error) {
	documents := []*fleetDocument{
		{destination: fleet.ClientDestination, payload: client, remove: client == nil},
	}
	if worker != nil {
		documents = append(documents, &fleetDocument{destination: fleet.WorkerDestination, payload: worker})
	}
	if len(catalog) > 0 {
		documents = append(documents, &fleetDocument{
			destination: fleet.CoordinatorDestination,
			payload:     catalog[0], remove: catalog[0] == nil,
		})
	}
	sort.Slice(documents, func(left, right int) bool {
		return documents[left].destination < documents[right].destination
	})
	for _, planned := range documents {
		planned.target = c.Path(planned.destination)
		if err := workerPathIsInHome(c.Home, planned.target); err != nil {
			return nil, err
		}
		before, err := os.ReadFile(planned.target)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			before = nil
		}
		planned.before = before
		if planned.remove {
			planned.changed = before != nil
			continue
		}
		planned.changed = before == nil || !bytes.Equal(before, planned.payload)
	}
	return documents, nil
}

func fleetResult(
	document *fleet.Document, host string, worker, client []byte, changed bool,
) (map[string]any, error) {
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
		"worker_sha256":  agentenv.Digest(worker),
		"client_sha256":  nil,
		"credential_ref": document.Hosts[host].CredentialRef,
		"desired_models": fleetModelMap(sets.Desired),
		// This document digest is not the enrollment revision. The coordinator
		// computes that revision after loading the projection and local bindings.
		"coordinator_projection": map[string]any{
			"purpose": fleet.CoordinatorPurpose,
			"sha256":  agentenv.Digest(catalog),
		},
		// Convergence never enrolls. Applying a catalog is a deliberate
		// Steward admin operation with its own idempotency key and revision
		// fence, because an accidental re-enrollment invalidates in-flight
		// offers on a live fleet.
		"enrolled": false,
	}
	if client != nil {
		result["client_sha256"] = agentenv.Digest(client)
		result["admin_credential_ref"] = document.Hosts[host].AdminCredentialRef
	} else {
		result["client"] = "not written; the coordinator administers itself locally"
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

// fleetResolveCredentials proves this host's credential references resolve to
// files the runtime may use. It reads metadata only: neither value nor a hash
// of one may ever reach a run record.
//
// The admin reference is checked only for a host that is given a client
// document. The coordinator has no remote client, so requiring it to hold an
// admin credential for itself would refuse a host that is configured correctly.
func fleetResolveCredentials(c *Context, host fleet.Host, client bool) error {
	if _, err := fleetResolveSecretReference(
		c.Home, host.CredentialRef, workerReferencePattern); err != nil {
		return err
	}
	if !client {
		return nil
	}
	_, err := fleetResolveSecretReference(
		c.Home, host.AdminCredentialRef, fleetAdminReferencePattern)
	return err
}

// fleetResolveSecretReference resolves a credential reference to the file that
// holds it and proves that file is safe to hand to the runtime.
//
// The check refuses a symlink outright rather than following one. A stat that
// follows links approves the target's mode and owner while the runtime, which
// opens with O_NOFOLLOW, refuses the link: the host would converge clean and
// then fail at first use, which is the worst place to find out. The file is
// opened to prove the path that was checked is the path that would be used,
// and its content is never read.
func fleetResolveSecretReference(
	home, reference string, pattern *regexp.Regexp,
) (string, error) {
	if !pattern.MatchString(reference) {
		return "", errors.New("invalid secret reference for this credential namespace")
	}
	root := filepath.Join(home, workerSecretRoot)
	path := filepath.Join(root, strings.TrimPrefix(reference, "secretref:"))
	if err := workerPathIsContained(root, path); err != nil {
		return "", err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !linked.Mode().IsRegular() {
		return "", errors.New(
			"secret reference must be a regular file, not a symlink or a device")
	}
	handle, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", errors.New("secret reference could not be opened without following a link")
	}
	defer func() { _ = handle.Close() }()
	opened, err := handle.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(linked, opened) {
		return "", errors.New("secret reference changed while it was being checked")
	}
	system, described := opened.Sys().(*syscall.Stat_t)
	if !opened.Mode().IsRegular() || !described ||
		int(system.Uid) != os.Getuid() || opened.Mode().Perm() != 0o600 || opened.Size() == 0 {
		return "", errors.New("secret reference requires a nonempty owner-only 0600 file")
	}
	return path, nil
}

// fleetWriteProjection retains every owned document as it is now, stages each
// new one beside its destination, validates the staged bytes, and only then
// activates it with one rename. A reader therefore sees the previous document
// or the complete new one, never a prefix and never something that does not
// parse.
//
// Both documents are retained even when only one changes, so that restoring a
// run puts the whole of what it found back rather than half of it.
func fleetWriteProjection(c *Context, runID string, documents []*fleetDocument) error {
	if err := fleetRetainPrior(c, runID, documents); err != nil {
		return err
	}
	for _, planned := range documents {
		if !planned.changed {
			continue
		}
		activationErr := fleetActivate(planned)
		if activationErr == nil {
			continue
		}
		// Any failure on any document leaves the host with some of this run
		// applied and some of it not, whether the cause was a full disk, a
		// read-only filesystem or a document that did not survive its own
		// rename. All of them are put back, and all of them report as a
		// rollback rather than as a bare error, because "failed" and "failed
		// and half applied" are different things to wake up to.
		if restoreErr := FleetRestorePrior(c.Home, runID); restoreErr != nil {
			// Not a rollback: the machine is not back in the state the run
			// found it in. The "rollback failed" prefix is what the agent
			// classifies as rollback-failed, which is the loudest outcome a
			// host can report, and it is the honest one here.
			return Failf(
				"rollback failed: %s could not be activated (%v) and the previous "+
					"documents could not be restored (%v); this host is in a mixed "+
					"state and needs the retained record under %s",
				planned.destination, activationErr, restoreErr,
				filepath.Join(fleet.StateDirectory, runID))
		}
		return RolledBackf(
			"%s could not be activated (%v); every document this run touched was "+
				"restored, so the host is left consistent with what it had before",
			planned.destination, activationErr)
	}
	return nil
}

// fleetActivate stages one document, validates the bytes that are on disk, and
// publishes them with a single rename. It reports a failure and never repairs
// one: repairing is the caller's job, because a repair has to put back every
// document this run touched and not only this one.
func fleetActivate(planned *fleetDocument) error {
	if planned.remove {
		if err := os.Remove(planned.target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return fsatomic.SyncDirectory(filepath.Dir(planned.target))
	}
	staged, err := fleetStage(planned.target, planned.payload)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()
	if err := fleetValidateStaged(staged, planned); err != nil {
		return err
	}
	if err := os.Rename(staged, planned.target); err != nil {
		return err
	}
	_ = fsatomic.SyncDirectory(filepath.Dir(planned.target))
	written, err := os.ReadFile(planned.target)
	if err != nil {
		return err
	}
	if !bytes.Equal(written, planned.payload) {
		return errors.New("the activated document is not the one that was validated")
	}
	return nil
}

// fleetRetainPrior records the documents that are about to be replaced, under a
// directory created exclusively so that two different runs never share one.
//
// A retry of the same run is not a different run. Convergence is retried after
// a transport failure often enough that an exclusive create alone would turn
// the second attempt into an immediate refusal, so an existing directory that
// already holds this run's record is accepted and the record is left exactly as
// the first attempt wrote it: that record, not a later one, says what the run
// found. An existing directory with no record is a real collision and is
// diagnosed rather than overwritten.
func fleetRetainPrior(c *Context, runID string, documents []*fleetDocument) error {
	backup := c.Path(fleet.StateDirectory, runID)
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return err
	}
	recordPath := filepath.Join(backup, "prior.json")
	switch err := os.Mkdir(backup, 0o700); {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		info, statErr := os.Stat(recordPath)
		if statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return nil
		}
		return errors.New(
			"fleet retention directory " + filepath.Join(fleet.StateDirectory, runID) +
				" exists without a retained record; inspect it before retrying, " +
				"because another run may already own this run ID")
	default:
		return err
	}
	retained := make([]any, 0, len(documents))
	for _, planned := range documents {
		entry := map[string]any{
			"destination": planned.destination,
			"existed":     planned.before != nil,
			"content":     nil,
		}
		if planned.before != nil {
			entry["content"] = string(planned.before)
		}
		retained = append(retained, entry)
	}
	record, err := agentenv.Canonical(map[string]any{"documents": retained})
	if err != nil {
		return err
	}
	// The retention is written at least as durably as the activation it
	// protects: its directory entry is flushed too, so a power loss cannot
	// leave the new projection on disk and lose the document it replaced.
	return fsatomic.WritePrivateFile(recordPath, record)
}

// fleetOwnedDestinations are the only paths a retained record may name. A
// record is replayed by the restore path, so the paths in it are checked again
// rather than trusted because this program wrote them.
var fleetOwnedDestinations = map[string]bool{
	fleet.WorkerDestination:      true,
	fleet.ClientDestination:      true,
	fleet.CoordinatorDestination: true,
}

// FleetRestorePrior puts back the documents a run replaced.
//
// It is the rollback path for this component, and it is exercised rather than
// assumed: the run that replaced a document is named, its retained record is
// read, and every destination goes back to exactly what that run found, up to
// and including not existing at all.
func FleetRestorePrior(home, runID string) error {
	record, err := os.ReadFile(filepath.Join(home, fleet.StateDirectory, runID, "prior.json"))
	if err != nil {
		return err
	}
	prior, err := canonjson.DecodeObject(record)
	if err != nil {
		return err
	}
	documents, ok := prior["documents"].([]any)
	if !ok || len(documents) == 0 {
		return errors.New("retained fleet record names no documents")
	}
	for _, item := range documents {
		entry, isObject := item.(map[string]any)
		if !isObject {
			return errors.New("retained fleet record is malformed")
		}
		if err := fleetRestoreDocument(home, entry); err != nil {
			return err
		}
	}
	return nil
}

func fleetRestoreDocument(home string, entry map[string]any) error {
	destination, _ := entry["destination"].(string)
	if !fleetOwnedDestinations[destination] {
		return errors.New("retained fleet record names an unexpected destination")
	}
	target := filepath.Join(home, destination)
	if err := workerPathIsInHome(home, target); err != nil {
		return err
	}
	existed, _ := entry["existed"].(bool)
	if !existed {
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	content, ok := entry["content"].(string)
	if !ok {
		return errors.New("retained fleet record has no content to restore")
	}
	// The same staging discipline as the forward path, and for a reason the
	// forward path does not have: worker-bootstrap.json is also written by
	// worker-configuration v1, whose helper stages through one fixed temporary
	// name created with O_EXCL. A crash that leaves that name behind would
	// block both v1 convergence and this restore until somebody removed it by
	// hand. A unique temporary name cannot be blocked by a corpse.
	staged, err := fleetStage(target, []byte(content))
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()
	if err := os.Rename(staged, target); err != nil {
		return err
	}
	return fsatomic.SyncDirectory(filepath.Dir(target))
}

// fleetStage writes the candidate document beside its destination, so that the
// activation above is a rename within one directory and therefore atomic.
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
	abandon := func(cause error) (string, error) {
		_ = handle.Close()
		_ = os.Remove(name)
		return "", cause
	}
	if err := handle.Chmod(0o600); err != nil {
		return abandon(err)
	}
	if _, err := handle.Write(payload); err != nil {
		return abandon(err)
	}
	if err := handle.Sync(); err != nil {
		return abandon(err)
	}
	if err := handle.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// fleetValidateStaged reads back what is actually on disk and checks that it is
// the document that was meant to be written and that it still satisfies its
// own closed contract. Validating the bytes rather than the intention is what
// makes a truncated or tampered staging file impossible to activate.
func fleetValidateStaged(staged string, planned *fleetDocument) error {
	written, err := os.ReadFile(staged)
	if err != nil {
		return err
	}
	if !bytes.Equal(written, planned.payload) {
		return errors.New("staged fleet projection does not match what was rendered")
	}
	entry, err := canonjson.DecodeObject(written)
	if err != nil {
		return errors.New("staged fleet projection is not a JSON object")
	}
	switch planned.destination {
	case fleet.WorkerDestination:
		if !workerSameFields(entry, workerEntryFields) {
			return errors.New("staged fleet projection is not a worker bootstrap document")
		}
	case fleet.ClientDestination:
		if !workerSameFields(entry, fleet.ClientFields) {
			return errors.New("staged fleet projection is not a coordinator client document")
		}
	case fleet.CoordinatorDestination:
		if !workerSameFields(entry, []string{"kind", "schema_version", "coordinator_id", "workers", "projects"}) || entry["kind"] != fleet.CoordinatorKind {
			return errors.New("staged fleet projection is not a coordinator fleet document")
		}
	default:
		return errors.New("staged fleet projection has no owned destination")
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
			"host-scoped fleet projections applied; "+
				"enrollment stays an explicit Steward operation")
	}
	state, _ := result["state"].(string)
	return state, nil
}
