package agent

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// fleetDocument is one file this component owns on the host.
type fleetDocument struct {
	destination string
	target      string
	payload     []byte
	before      []byte
	changed     bool
}

// fleetProjection decides what this host's fleet intent means for it, and
// writes the documents that differ from what is there.
//
// Two files are owned: the worker bootstrap the runtime already reads, and the
// coordinator client it reads the same way. The proposed catalog is a third
// projection and is never written anywhere by this program.
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
	worker, err := document.WorkerBytes(host)
	if err != nil {
		return nil, err
	}
	if err := fleetAgreesWithWorkerConfiguration(c, host, worker); err != nil {
		return nil, err
	}
	client, err := document.ClientBytes(host)
	if err != nil {
		return nil, err
	}
	if err := fleetResolveCredentials(c, document.Hosts[host]); err != nil {
		return nil, err
	}
	documents, err := fleetPlanDocuments(c, worker, client)
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
	return fleetResult(document, host, worker, client, changed)
}

// fleetPlanDocuments works out what each owned file would become, in a stable
// order, before anything is written. Deciding first and writing second is what
// lets an observation run exactly the code an apply does.
func fleetPlanDocuments(c *Context, worker, client []byte) ([]*fleetDocument, error) {
	documents := []*fleetDocument{
		{destination: fleet.WorkerDestination, payload: worker},
		{destination: fleet.ClientDestination, payload: client},
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
		"state":                state,
		"configured":           true,
		"worker_sha256":        agentenv.Digest(worker),
		"client_sha256":        agentenv.Digest(client),
		"catalog_sha256":       agentenv.Digest(catalog),
		"credential_ref":       document.Hosts[host].CredentialRef,
		"admin_credential_ref": document.Hosts[host].AdminCredentialRef,
		"desired_models":       fleetModelMap(sets.Desired),
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
		if err := fleetActivate(c, runID, planned); err != nil {
			return err
		}
	}
	return nil
}

func fleetActivate(c *Context, runID string, planned *fleetDocument) error {
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
	if err == nil && bytes.Equal(written, planned.payload) {
		return nil
	}
	// The document that is live is not the one that was validated, so every
	// document this run touched goes back rather than being left half applied.
	if restoreErr := FleetRestorePrior(c.Home, runID); restoreErr != nil {
		return RolledBackf(
			"fleet projection failed verification and could not be restored: %v", restoreErr)
	}
	return RolledBackf(
		"fleet projection failed verification; restored the previous documents")
}

// fleetRetainPrior records the documents that are about to be replaced, under a
// directory created exclusively so that two runs never share one.
func fleetRetainPrior(c *Context, runID string, documents []*fleetDocument) error {
	backup := c.Path(fleet.StateDirectory, runID)
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(backup, 0o700); err != nil {
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
	return agentenv.AtomicWrite(filepath.Join(backup, "prior.json"), record, 0o600)
}

// fleetOwnedDestinations are the only paths a retained record may name. A
// record is replayed by the restore path, so the paths in it are checked again
// rather than trusted because this program wrote them.
var fleetOwnedDestinations = map[string]bool{
	fleet.WorkerDestination: true,
	fleet.ClientDestination: true,
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
	return agentenv.AtomicWrite(target, []byte(content), 0o600)
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
			"fleet worker and client projections applied; "+
				"enrollment stays an explicit Steward operation")
	}
	state, _ := result["state"].(string)
	return state, nil
}
