package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

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
	destination    string
	target         string
	payload        []byte
	remove         bool
	before         []byte
	changed        bool
	effected       bool
	validatePolicy func(string) error
	restorePolicy  bool
	policyReceipt  *fleetClientReceipt
}

// fleetProjection decides what this host's fleet intent means for it, and writes
// the owned documents that differ from what is there. Workers, client-only hosts,
// and the coordinator each receive only their declared projection.
func fleetProjection(
	c *Context, component map[string]any, host, runID string, dryRun bool,
) (map[string]any, error) {
	if component == nil {
		return fleetRetireOwnedProjections(c, runID, dryRun)
	}
	document, err := fleet.Parse(component)
	if err != nil {
		return nil, err
	}
	_, workerDeclared := document.Hosts[host]
	clientDeclared := document.NeedsClientProjection(host)
	coordinator := host == document.Coordinator.Host
	if !workerDeclared && !clientDeclared && !coordinator {
		return fleetRetireOwnedProjections(c, runID, dryRun)
	}
	var worker, catalog []byte
	if workerDeclared {
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
	if clientDeclared {
		client, err = document.ClientBytes(host)
		if err != nil {
			return nil, err
		}
	}
	if workerDeclared {
		if err := fleetResolveCredentials(c, document.Hosts[host], client != nil); err != nil {
			return nil, err
		}
	} else if clientDeclared {
		if err := fleetResolveClientCredential(c, document.Clients[host].AdminCredentialRef); err != nil {
			return nil, err
		}
	}
	documents, err := fleetPlanDocuments(c, worker, client, catalog)
	if err != nil {
		return nil, err
	}
	policy, err := fleetPlanPolicy(c, document.RoutePolicyBytes())
	if err != nil {
		return nil, err
	}
	if policy != nil {
		documents = append(documents, policy)
	}
	changed := false
	for _, planned := range documents {
		changed = changed || planned.changed
	}
	var reload any
	if !dryRun {
		if changed {
			if err := fleetWriteProjection(c, runID, documents); err != nil {
				return nil, err
			}
			if coordinator && fleetCatalogChanged(documents) {
				reload, err = fleetReloadCoordinator(c, runID)
				if err != nil {
					if policy != nil && policy.changed {
						if restoreErr := fleetRestoreEffects(c, []*fleetDocument{policy}); restoreErr != nil {
							return nil, Failf("rollback failed: policy after reload failure: %v; %v", restoreErr, err)
						}
						if restoreErr := fleetReconcilePolicyReceipt(c); restoreErr != nil {
							return nil, Failf("rollback failed: policy receipt: %v; %v", restoreErr, err)
						}
					}
					return nil, err
				}
			}
		} else {
			// Establish ownership even when the explicit desired client already
			// matches. Its original bytes stay the rollback boundary.
			receipt, err := fleetPrepareClientReceipt(c, documents)
			if err != nil {
				return nil, err
			}
			if receipt != nil {
				if err := fleetCommitClientReceipt(c, receipt); err != nil {
					return nil, err
				}
			}
			if policy != nil {
				if err := fleetPreparePolicyReceipt(c, documents); err != nil {
					return nil, err
				}
				if err := fleetCommitPolicyReceipt(c, documents); err != nil {
					return nil, err
				}
			}
		}
	}
	if clientDeclared && !workerDeclared {
		return fleetPolicyResult(fleetClientResult(document.Clients[host], client, changed), policy), nil
	}
	if !workerDeclared {
		state := "converged"
		if changed {
			state = "updated"
		}
		result := map[string]any{
			"state": state, "configured": true, "enrolled": false,
			"coordinator_projection": map[string]any{
				"purpose": fleet.CoordinatorPurpose, "sha256": agentenv.Digest(catalog),
			},
		}
		if reload != nil {
			result["coordinator_reload"] = reload
		}
		return fleetPolicyResult(result, policy), nil
	}
	result, err := fleetResult(document, host, worker, client, changed)
	if err != nil {
		return nil, err
	}
	if reload != nil {
		result["coordinator_reload"] = reload
	}
	return fleetPolicyResult(result, policy), nil
}

// stewardCoordinatorUnit is the unit that runs the coordinator on its host. It
// re-reads its configuration, including the catalog input this component
// writes, on SIGHUP.
const stewardCoordinatorUnit = "t3-steward.service"

// fleetCatalogChanged reports whether this run rewrote the coordinator catalog
// input, as opposed to only the worker or client documents.
func fleetCatalogChanged(documents []*fleetDocument) bool {
	for _, planned := range documents {
		if planned.destination == fleet.CoordinatorDestination && planned.changed {
			return true
		}
	}
	return false
}

// fleetReloadReceiptPath is where the coordinator writes its verdict on every
// SIGHUP (t3-steward contract 2), relative to the home directory. UpKeeper
// only reads it: the coordinator owns the verdict, and this component never
// decides the outcome or restarts the unit to force one.
const fleetReloadReceiptPath = ".local/state/t3-steward/coordinator/reload-receipt.json"

// fleetReloadWait bounds how long the component waits for a receipt newer
// than its signal, polling every fleetReloadPoll.
const (
	fleetReloadWait = 30 * time.Second
	fleetReloadPoll = 500 * time.Millisecond
)

// fleetReloadReceipt is the receipt the coordinator writes. Times are RFC 3339
// strings on the wire and are compared as instants.
type fleetReloadReceipt struct {
	RequestedAt         string               `json:"requestedAt"`
	CompletedAt         string               `json:"completedAt"`
	Outcome             string               `json:"outcome"`
	Error               string               `json:"error"`
	ConfigurationDigest string               `json:"configurationDigest"`
	PreviousDigest      string               `json:"previousDigest"`
	Release             string               `json:"release"`
	Blockers            []fleetReloadBlocker `json:"blockers"`
}

// fleetReloadBlocker is one retained assignment the coordinator named as the
// reason it refused a catalog change.
type fleetReloadBlocker struct {
	WorkerID     string `json:"workerId"`
	AssignmentID string `json:"assignmentId"`
	AttemptID    string `json:"attemptId"`
	Progress     string `json:"progress"`
	Control      string `json:"control"`
	JournalPhase string `json:"journalPhase"`
	Unblock      string `json:"unblock"`
}

// fleetReloadCoordinator asks a running coordinator to re-read its
// configuration after its catalog input changed on disk, then waits for the
// receipt the coordinator writes for that signal. Without the signal, a
// projection that changes while the binary does not is never loaded: the
// steward component only restarts the unit when it installs a new release,
// and even then it restarts before this component has written the file.
//
// The coordinator owns the verdict. An accepted or unchanged receipt converges
// the component and reports the digest; a rejected one fails it with the
// coordinator's error and blockers, after putting back the projection this run
// replaced; no receipt requested at or after the signal within fleetReloadWait
// fails it as unverified, naming the command that shows the verdict on the
// host. An inactive unit reads the projection when it next starts, and is
// reported as such rather than signalled.
func fleetReloadCoordinator(c *Context, runID string) (any, error) {
	if !c.UnitActive(stewardCoordinatorUnit) {
		return "unit inactive; the projection is read at the next start", nil
	}
	// The signal time is taken just before the signal, so a receipt requested
	// at or after it can only answer this signal or a later one.
	signalledAt := c.now()
	if _, err := c.Must([]string{"systemctl", "--user", "kill", "-s", "HUP", stewardCoordinatorUnit},
		0, "", "reload coordinator configuration"); err != nil {
		return nil, err
	}
	receipt, existed, found := fleetAwaitReloadReceipt(c, signalledAt)
	if !found {
		why := "no receipt file exists; the coordinator may predate the reload receipt (t3-steward before rc.69)"
		if existed {
			why = "a receipt file exists but its requestedAt is older than the signal"
		}
		// The projection stays on disk here, deliberately. Without a verdict
		// this component does not know whether the coordinator loaded it, and
		// putting back the previous document would silently revert a catalog
		// the coordinator may be running: the next restart would then load the
		// older configuration with nothing saying so. An unverified reload is
		// answered by reading the verdict on the host, not by guessing it. The
		// unknown outcome below is the same case for the same reason.
		return nil, Failf("coordinator reload unverified: no receipt requested at or after %s appeared at %s within %s (%s); run `t3-steward coordinator reload --json` on %s to see the verdict",
			signalledAt.UTC().Format(time.RFC3339), c.Path(fleetReloadReceiptPath), fleetReloadWait, why, c.Host.Name)
	}
	switch receipt.Outcome {
	case "accepted", "unchanged":
		return map[string]any{
			"outcome":      receipt.Outcome,
			"digest":       receipt.ConfigurationDigest,
			"requested_at": receipt.RequestedAt,
			"completed_at": receipt.CompletedAt,
		}, nil
	case "rejected":
		lines := []string{"coordinator rejected the reload: " + receipt.Error}
		for _, blocker := range receipt.Blockers {
			state := blocker.Progress
			if blocker.Control != "" {
				state += "/" + blocker.Control
			}
			if blocker.JournalPhase != "" {
				state += ", worker journal " + blocker.JournalPhase
			}
			lines = append(lines, "  worker "+blocker.WorkerID+" assignment "+blocker.AssignmentID+
				" attempt "+blocker.AttemptID+" ("+state+"): "+blocker.Unblock)
		}
		lines = append(lines, "  the effective configuration and its digest are unchanged ("+receipt.ConfigurationDigest+")")
		// The coordinator refused these bytes and kept the configuration it is
		// running, so the file and the running process now disagree. Left
		// alone, the refused projection is what the coordinator reads the next
		// time it starts, and a projection it has already refused is one it
		// cannot load at all: the unit crash-loops, `t3-steward check` fails
		// with it, and the pair gate that would install a steward able to read
		// the file needs that check to observe the running version. So the
		// document this run replaced goes back, and a refused projection never
		// outlives the run that wrote it. The component still fails: the
		// release is wrong and the operator has to fix it.
		existed, restoreErr := fleetRestoreRetainedDocument(c.Home, runID, fleet.CoordinatorDestination)
		if restoreErr != nil {
			// The "rollback failed" prefix is what the agent classifies as
			// rollback-failed, the loudest outcome a host can report, and it is
			// the honest one: the refused projection is still the one the
			// coordinator would read at its next start.
			return nil, Failf(
				"rollback failed: %s\n  the refused projection could not be replaced by the document this run found (%v); "+
					"%s is the projection the coordinator refused and it is what the coordinator would read at its next "+
					"start, so put back the retained document under %s before that unit is restarted",
				strings.Join(lines, "\n"), restoreErr, c.Path(fleet.CoordinatorDestination),
				filepath.Join(fleet.StateDirectory, runID))
		}
		if existed {
			lines = append(lines, "  the previous projection was put back, so the coordinator reads the configuration it is running when it next starts")
		} else {
			lines = append(lines, "  this run created the projection, so it was removed; the coordinator reads the configuration it is running when it next starts")
		}
		return nil, Failf("%s", strings.Join(lines, "\n"))
	default:
		return nil, Failf("coordinator reload receipt at %s carries an unknown outcome %q; run `t3-steward coordinator reload --json` on %s",
			c.Path(fleetReloadReceiptPath), receipt.Outcome, c.Host.Name)
	}
}

// fleetAwaitReloadReceipt polls the receipt file until one requested at or
// after since appears, or fleetReloadWait elapses by the context's clock. It
// reports whether any receipt file existed at all, so an old steward that
// writes none can be told apart from a slow one.
func fleetAwaitReloadReceipt(c *Context, since time.Time) (receipt fleetReloadReceipt, existed, found bool) {
	path := c.Path(fleetReloadReceiptPath)
	deadline := since.Add(fleetReloadWait)
	for {
		if current, ok := fleetReadReloadReceipt(path); ok {
			existed = true
			if requested, err := time.Parse(time.RFC3339Nano, current.RequestedAt); err == nil && !requested.Before(since) {
				return current, true, true
			}
		} else if _, err := os.Lstat(path); err == nil {
			existed = true
		}
		if !c.now().Before(deadline) {
			return fleetReloadReceipt{}, existed, false
		}
		c.sleep(fleetReloadPoll)
	}
}

// fleetReadReloadReceipt decodes the receipt file. A missing or malformed file
// is simply not a receipt yet.
func fleetReadReloadReceipt(path string) (fleetReloadReceipt, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fleetReloadReceipt{}, false
	}
	var receipt fleetReloadReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.RequestedAt == "" || receipt.Outcome == "" {
		return fleetReloadReceipt{}, false
	}
	return receipt, true
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
		var before []byte
		var err error
		if planned.destination == fleet.ClientDestination {
			before, err = fleetReadPrivateBounded(c, fleet.ClientDestination, fleetCoordinatorMaxBytes)
		} else {
			before, err = os.ReadFile(planned.target)
		}
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

func fleetClientResult(client fleet.Client, projection []byte, changed bool) map[string]any {
	state := "converged"
	if changed {
		state = "updated"
	}
	return map[string]any{
		"state":                state,
		"configured":           true,
		"enrolled":             false,
		"client_sha256":        agentenv.Digest(projection),
		"admin_credential_ref": client.AdminCredentialRef,
	}
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
func fleetResolveClientCredential(c *Context, reference string) error {
	_, err := fleetResolveSecretReference(c.Home, reference, fleetAdminReferencePattern)
	return err
}

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
	if err := fleetPreparePolicyReceipt(c, documents); err != nil {
		return err
	}
	receipt, err := fleetPrepareClientReceipt(c, documents)
	if err != nil {
		return err
	}
	if err := fleetWriteDocuments(c, runID, documents); err != nil {
		return err
	}
	if receipt != nil {
		if err := fleetCommitClientReceipt(c, receipt); err != nil {
			return fleetProjectionCommitFailure(c, runID, documents, err)
		}
	}
	if err := fleetCommitPolicyReceipt(c, documents); err != nil {
		return fleetProjectionCommitFailure(c, runID, documents, err)
	}
	return nil
}

func fleetWriteDocuments(c *Context, runID string, documents []*fleetDocument) error {
	if err := fleetRetainPrior(c, runID, documents); err != nil {
		return err
	}
	for _, planned := range documents {
		planned.effected = false
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
		if restoreErr := fleetRestoreEffects(c, documents); restoreErr != nil {
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
	home := strings.TrimSuffix(planned.target, planned.destination)
	if home == planned.target {
		return errors.New("projection target does not match destination")
	}
	if err := fleetOwnedPathSafe(home, planned.target); err != nil {
		return err
	}
	initialDirectory, err := fleetOpenParent(home, planned.target)
	if errors.Is(err, os.ErrNotExist) && !planned.remove {
		if err = os.MkdirAll(filepath.Dir(planned.target), 0755); err != nil {
			return err
		}
		initialDirectory, err = fleetOpenParent(home, planned.target)
	}
	if err != nil {
		return err
	}
	defer initialDirectory.Close()
	initialInfo, err := initialDirectory.Stat()
	if err != nil {
		return err
	}
	var staged string
	if !planned.remove {
		var err error
		staged, err = fleetStage(planned.target, planned.payload)
		if err != nil {
			return err
		}
		defer func() { _ = syscall.Unlinkat(int(initialDirectory.Fd()), filepath.Base(staged)) }()
		if !planned.restorePolicy {
			if err := fleetValidateStaged(staged, planned); err != nil {
				return err
			}
		}
		// External validation is a trust boundary. Check staging again as well.
		written, err := fleetReadAt(initialDirectory, filepath.Base(staged))
		if err != nil {
			return err
		}
		if !bytes.Equal(written, planned.payload) {
			return errors.New("validator changed staged projection")
		}
		if planned.destination == fleet.RoutePolicyDestination {
			fd, err := syscall.Openat(int(initialDirectory.Fd()), filepath.Base(staged), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			handle := os.NewFile(uintptr(fd), staged)
			err = handle.Chmod(0644)
			handle.Close()
			if err != nil {
				return err
			}
		}
	}
	// Pin every ancestor with O_NOFOLLOW after the external validator returns.
	directory, err := fleetOpenParent(home, planned.target)
	if err != nil {
		return err
	}
	defer directory.Close()
	finalInfo, err := directory.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(initialInfo, finalInfo) {
		return errors.New("projection directory changed during validation")
	}
	current, err := fleetReadAt(directory, filepath.Base(planned.target))
	if err != nil {
		return err
	}
	if !fleetSameBytes(current, planned.before) {
		return errors.New("projection changed during validation")
	}
	if planned.remove {
		err = syscall.Unlinkat(int(directory.Fd()), filepath.Base(planned.target))
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	} else {
		err = syscall.Renameat(int(directory.Fd()), filepath.Base(staged), int(directory.Fd()), filepath.Base(planned.target))
	}
	if err != nil {
		return err
	}
	planned.effected = true
	if err := directory.Sync(); err != nil {
		return err
	}
	written, err := fleetReadAt(directory, filepath.Base(planned.target))
	if err != nil {
		return err
	}
	if !fleetSameBytes(written, planned.payload) {
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
			entry["encoding"] = "base64/v1"
			entry["content"] = base64.StdEncoding.EncodeToString(planned.before)
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
	fleet.RoutePolicyDestination: true,
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
	policyRestored := false
	for _, item := range documents {
		entry, isObject := item.(map[string]any)
		if !isObject {
			return errors.New("retained fleet record is malformed")
		}
		if err := fleetRestoreDocument(home, entry); err != nil {
			return err
		}
		policyRestored = policyRestored || entry["destination"] == fleet.RoutePolicyDestination
	}
	if policyRestored {
		return fleetReconcilePolicyReceipt(&Context{Home: home})
	}
	return nil
}

// fleetRestoreRetainedDocument puts one destination back to what the named run
// found, and reports whether that run found anything there at all. It reads
// the same record FleetRestorePrior replays, written before the run wrote
// anything, and it checks the paths in that record rather than trusting them.
//
// It is deliberately narrower than FleetRestorePrior, which puts back every
// document a run touched. A coordinator that refuses a reload is passing a
// verdict on the catalog input alone: the worker bootstrap the same run may
// have written is shared with worker-configuration, which converged earlier in
// the same run and would be contradicted by reverting it, and the client
// document has an ownership receipt whose active hash is committed before this
// point and would no longer describe a reverted file.
func fleetRestoreRetainedDocument(home, runID, destination string) (bool, error) {
	return fleetRestoreRetainedExpected(home, runID, destination, nil, false)
}

func fleetRestoreRetainedExpected(home, runID, destination string, expected []byte, guarded bool) (bool, error) {
	record, err := os.ReadFile(filepath.Join(home, fleet.StateDirectory, runID, "prior.json"))
	if err != nil {
		return false, err
	}
	prior, err := canonjson.DecodeObject(record)
	if err != nil {
		return false, err
	}
	documents, ok := prior["documents"].([]any)
	if !ok || len(documents) == 0 {
		return false, errors.New("retained fleet record names no documents")
	}
	for _, item := range documents {
		entry, isObject := item.(map[string]any)
		if !isObject {
			return false, errors.New("retained fleet record is malformed")
		}
		if named, _ := entry["destination"].(string); named != destination {
			continue
		}
		existed, _ := entry["existed"].(bool)
		if guarded {
			entry["expected"] = expected
		}
		return existed, fleetRestoreDocument(home, entry)
	}
	return false, errors.New("retained fleet record does not name " + destination)
}

func fleetRestoreDocument(home string, entry map[string]any) error {
	destination, _ := entry["destination"].(string)
	if !fleetOwnedDestinations[destination] {
		return errors.New("retained fleet record names an unexpected destination")
	}
	target := filepath.Join(home, destination)
	if err := fleetOwnedPathSafe(home, target); err != nil {
		return err
	}
	existed, _ := entry["existed"].(bool)
	if expected, guarded := entry["expected"].([]byte); guarded {
		var payload []byte
		var err error
		if existed {
			payload, err = fleetRetainedBytes(entry)
			if err != nil {
				return err
			}
		}
		return fleetActivate(&fleetDocument{destination: destination, target: target, before: expected, payload: payload, remove: !existed, restorePolicy: true})
	}
	directory, err := fleetOpenParent(home, target)
	if errors.Is(err, os.ErrNotExist) && !existed {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	if !existed {
		if err := syscall.Unlinkat(int(directory.Fd()), filepath.Base(target)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return directory.Sync()
	}
	content, err := fleetRetainedBytes(entry)
	if err != nil {
		return err
	}
	// The same staging discipline as the forward path, and for a reason the
	// forward path does not have: worker-bootstrap.json is also written by
	// worker-configuration v1, whose helper stages through one fixed temporary
	// name created with O_EXCL. A crash that leaves that name behind would
	// block both v1 convergence and this restore until somebody removed it by
	// hand. A unique temporary name cannot be blocked by a corpse.
	staged, err := fleetStage(target, content)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()
	if destination == fleet.RoutePolicyDestination {
		if err := os.Chmod(staged, 0o644); err != nil {
			return err
		}
	}
	if err := syscall.Renameat(int(directory.Fd()), filepath.Base(staged), int(directory.Fd()), filepath.Base(target)); err != nil {
		return err
	}
	return directory.Sync()
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
	if planned.destination == fleet.RoutePolicyDestination {
		if planned.restorePolicy {
			return nil
		}
		if planned.validatePolicy == nil {
			return errors.New("policy staging requires configured-catalog validation")
		}
		return planned.validatePolicy(staged)
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
	if reload, _ := result["coordinator_reload"].(map[string]any); reload != nil {
		digest, _ := reload["digest"].(string)
		*actions = append(*actions, "coordinator reloaded its configuration (digest "+digest+")")
	}
	state, _ := result["state"].(string)
	return state, nil
}
