package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/release"
)

// t3TransactionName is the only retained transaction reconciliation accepts:
// the directory t3Begin creates beside the package, named, never a path.
var t3TransactionName = regexp.MustCompile(`^\.upkeeper-t3-[0-9]+$`)

const t3JournalLimit = 1 << 20

// t3Journal is the part of a retained transaction record reconciliation
// proves. Fields journaled only by newer agents are optional: a legacy record
// without them is reconciled against live evidence alone, never against an
// inferred value.
type t3Journal struct {
	Phase            string `json:"phase"`
	Prefix           string `json:"prefix"`
	Package          string `json:"package"`
	Link             string `json:"link"`
	State            string `json:"state"`
	OldDigest        string `json:"old_digest"`
	OldLink          string `json:"old_link"`
	OldStateDigest   string `json:"old_state_digest"`
	HadPackage       bool   `json:"had_package"`
	HadLink          bool   `json:"had_link"`
	HadState         bool   `json:"had_state"`
	NewDigest        string `json:"new_digest"`
	NewStateDigest   string `json:"new_state_digest"`
	PackageMoved     bool   `json:"package_moved"`
	PackageActivated bool   `json:"package_activated"`
	StateActivated   bool   `json:"state_activated"`
	Service          string `json:"service"`
	BindingSHA256    string `json:"binding_sha256"`
	PriorActive      *bool  `json:"prior_active"`
	PriorServed      any    `json:"prior_served"`
	// Dependents is the PartOf census before any effect; nil for a legacy record.
	Dependents map[string]string `json:"dependents"`
}

// t3ReconcileRetained is the explicit resume of one retained transaction left
// at files-restored: rollback put the original files back but could not verify
// the previous service. It changes no package, launcher, provenance, backup or
// unit and restarts nothing. It records the transaction as restored only when
// the live original tree, launcher and provenance, the retained failed tree and
// provenance, the unit binding and the healthy prior serving identity all prove
// unchanged; any drift or unknown state refuses with the journal untouched.
// A dry run performs every check and reports without writing.
func t3ReconcileRetained(c *Context, spec map[string]any, name string, actions *[]string) (string, error) {
	if !t3TransactionName.MatchString(name) {
		return "", Failf("retained T3 transaction name is not a transaction directory: %q", name)
	}
	// The journal is read, checked and published under the host's target lock
	// the run took, which serializes this command with every UpKeeper run. A
	// dry run writes nothing and may observe without it.
	if !c.DryRun && !c.lockHeld {
		return "", Failf("retained T3 transaction %s not reconciled: the host's target lock is not held", name)
	}
	service := adapterSpecString(spec, "service")
	if _, _, err := stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
		return "", err
	}
	fresh, err := t3Observe(c, spec)
	if err != nil {
		return "", err
	}
	prefix := adapterSpecString(fresh, "npm_prefix")
	if prefix == "" {
		return "", Failf("unknown T3 npm prefix; retained transaction not reconciled")
	}
	dir := filepath.Join(prefix, "lib/node_modules", name)
	info, err := os.Lstat(dir)
	if err != nil {
		return "", Failf("retained T3 transaction %s is missing", name)
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !owned || stat.Uid != uint32(os.Getuid()) {
		return "", Failf("retained T3 transaction %s is missing or not an owner-only directory", name)
	}
	// The journal's identity is taken before its bytes, so a replacement between
	// the two reads differs in bytes or identity at every later fence.
	journalPath := filepath.Join(dir, "transaction.json")
	journalInfo, err := os.Lstat(journalPath)
	if err != nil || !journalInfo.Mode().IsRegular() {
		return "", Failf("retained T3 transaction journal unreadable")
	}
	body, err := release.T3ReadFile(journalPath, t3JournalLimit)
	if err != nil {
		return "", Failf("retained T3 transaction journal unreadable: %v", err)
	}
	var j t3Journal
	var record map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if json.Unmarshal(body, &j) != nil || decoder.Decode(&record) != nil {
		return "", Failf("retained T3 transaction journal is not a transaction record")
	}
	if j.Phase == "restored" && record["reconciliation"] != nil {
		*actions = append(*actions, "retained T3 transaction "+name+" already reconciled; nothing changed")
		return "converged", nil
	}
	if j.Phase != "files-restored" {
		return "", Failf("retained T3 transaction %s is at phase %q; only files-restored reconciles", name, j.Phase)
	}
	state := filepath.Join(c.Home, release.T3StatePath, "current")
	if j.Prefix != prefix || j.Package != filepath.Join(prefix, "lib/node_modules/t3") || j.Link != filepath.Join(prefix, "bin/t3") ||
		j.State != state || (j.Service != "" && j.Service != service) {
		return "", Failf("retained T3 transaction %s does not own the current prefix, launcher, provenance or unit", name)
	}
	tx := &t3Transaction{c: c, prefix: prefix, packagePath: j.Package, link: j.Link, state: state, dir: dir,
		journalInfo: journalInfo, journalBytes: body}
	failedState := filepath.Join(filepath.Dir(state), "failed-"+name)
	// The record this reconciliation read: the same inode with the same bytes,
	// so neither a replacement nor an in-place change by another writer is
	// overwritten with conclusions drawn from the older record.
	journalUnchanged := func(path string) error {
		current, e := os.Lstat(path)
		if e != nil || !os.SameFile(current, journalInfo) {
			return errors.New("retained T3 transaction journal was replaced")
		}
		if now, e := release.T3ReadFile(path, t3JournalLimit); e != nil || !bytes.Equal(now, body) {
			return errors.New("retained T3 transaction journal changed")
		}
		return nil
	}
	// Every boundary the transaction owned, read fresh each time it is asked:
	// the transaction directory and its journal first, then the trees.
	unchanged := func() error {
		if current, e := os.Lstat(dir); e != nil || !os.SameFile(current, info) || !current.IsDir() || current.Mode().Perm() != 0700 {
			return errors.New("retained T3 transaction directory was replaced or changed")
		}
		if e := journalUnchanged(journalPath); e != nil {
			return e
		}
		if e := tx.parentsUnchanged(); e != nil {
			return e
		}
		digest, e := release.T3TreeDigest(j.Package)
		if j.HadPackage && (e != nil || digest != j.OldDigest) {
			return errors.New("live T3 tree differs from the transaction's original")
		}
		if !j.HadPackage && !t3Absent(j.Package) {
			return errors.New("T3 package appeared where the transaction found none")
		}
		if link, e := t3LinkValue(j.Link); e != nil || link != j.OldLink {
			return errors.New("live T3 launcher differs from the transaction's original")
		}
		digest, e = release.T3TreeDigest(state)
		if j.HadState && (e != nil || digest != j.OldStateDigest) {
			return errors.New("live T3 provenance differs from the transaction's original")
		}
		if !j.HadState && !t3Absent(state) {
			return errors.New("T3 provenance appeared where the transaction found none")
		}
		if !t3Absent(state + "-previous") {
			return errors.New("T3 recovery backup exists")
		}
		if j.PackageActivated {
			digest, e = release.T3TreeDigest(filepath.Join(dir, "failed"))
			if j.NewDigest == "" || e != nil || digest != j.NewDigest {
				return errors.New("retained failed T3 tree differs from the transaction's new tree")
			}
		}
		if j.PackageMoved && !t3Absent(filepath.Join(dir, "previous")) {
			return errors.New("transaction backup was not restored")
		}
		if j.StateActivated {
			digest, e = release.T3TreeDigest(failedState)
			if j.NewStateDigest == "" || e != nil || digest != j.NewStateDigest {
				return errors.New("retained failed T3 provenance or receipts differ")
			}
		}
		return nil
	}
	refuse := func(e error) (string, error) {
		return "", Failf("retained T3 transaction %s not reconciled; journal and every retained tree unchanged: %s", name, t3EvidenceText(e.Error()))
	}
	if err = unchanged(); err != nil {
		return refuse(err)
	}
	binding, err := t3UnitBinding(c, service, prefix)
	if err != nil {
		return refuse(err)
	}
	bindingDigest := release.T3Digest([]byte(binding))
	if j.BindingSHA256 != "" && j.BindingSHA256 != bindingDigest {
		return refuse(errors.New("T3 unit binding differs from the transaction's"))
	}
	checkBinding := func() error {
		current, e := t3UnitBinding(c, service, prefix)
		if e != nil || current != binding {
			return errors.New("T3 service launcher mapping changed or unsupported")
		}
		return nil
	}
	// The prior serving identity is the journaled one when there is one, and
	// for a legacy record the version the unchanged original tree installs.
	priorServed := j.PriorServed
	if priorServed == nil {
		priorServed = fresh["installed"]
	}
	priorActive := j.PriorActive == nil || *j.PriorActive
	priorSpec := map[string]any{"service": service, "version": priorServed}
	priorUpstream := release.VerifyT3Upstream(c.Home, prefix, adapterSpecString(priorSpec, "version")) == nil
	var priorRetained *release.T3Retained
	if j.HadState && !priorUpstream {
		if priorRetained, err = release.ReadT3Retained(c.Home); err != nil {
			return refuse(err)
		}
	}
	// Dependents active when the transaction began are restoration obligations:
	// each must be active now. A legacy record did not journal them, so the
	// current census must be known and is recorded, but it is not required.
	obligations := map[string]string{}
	dependentEvidence := "journaled dependents active"
	if record["dependents"] != nil {
		obligations = t3DependentObligations(j.Dependents, nil)
	} else {
		census, e := t3Dependents(c, service)
		if e != nil {
			return refuse(e)
		}
		censusJSON, _ := json.Marshal(census)
		dependentEvidence = "dependents not journaled by the transaction; current census " + string(censusJSON)
	}
	if priorActive {
		err = tx.awaitReady("reconciliation", func() (t3PredicateSample, []string, bool) {
			sample := t3PredicateSample{}
			if e := checkBinding(); e != nil {
				sample["unit_binding"] = "changed"
				return sample, []string{"unit_binding"}, true
			}
			failed := t3PriorServing(c, spec, priorSpec, prefix, priorServed, priorRetained, priorUpstream, sample)
			return sample, append(failed, t3DependentsReturned(c, obligations, sample)...), false
		})
	} else if active, e := t3UnitState(c, service); e != nil || active {
		err = errors.Join(errors.New("T3 service active or unknown where the transaction found it inactive"), e)
	} else if failed := t3DependentsReturned(c, obligations, t3PredicateSample{}); len(failed) > 0 {
		err = fmt.Errorf("T3 dependents not active: %s", strings.Join(failed, ", "))
	}
	if err != nil {
		return refuse(err)
	}
	// Fence every owned boundary again after the health samples.
	if err = errors.Join(unchanged(), checkBinding()); err != nil {
		return refuse(err)
	}
	if c.DryRun {
		*actions = append(*actions, "would reconcile retained T3 transaction "+name+": original tree, launcher, provenance, failed tree, unit and serving identity verified")
		return "updated", nil
	}
	identity := "ordinary"
	if priorUpstream {
		identity = "upstream"
	} else if priorRetained != nil {
		identity = "controlled"
	}
	bindingEvidence := "journaled binding matched"
	if j.BindingSHA256 == "" {
		bindingEvidence = "binding not journaled by the transaction; current binding selects the owned launcher"
	}
	record["phase"] = "restored"
	record["reconciliation"] = map[string]any{
		"at":                  c.now().UTC().Format(time.RFC3339Nano),
		"run_id":              c.RunID,
		"result":              "files and previous service verified restored by explicit reconciliation",
		"live_digest":         j.OldDigest,
		"live_link":           j.OldLink,
		"live_state_digest":   j.OldStateDigest,
		"failed_digest":       j.NewDigest,
		"failed_state_digest": j.NewStateDigest,
		"binding_sha256":      bindingDigest,
		"binding_evidence":    bindingEvidence,
		"prior_identity":      identity,
		"prior_served":        priorServed,
		"prior_active":        priorActive,
		"dependents":          obligations,
		"dependent_evidence":  dependentEvidence,
		"failed_samples":      tx.samples,
	}
	record["reconciled_from"] = map[string]any{"sha256": release.T3Digest(body), "journal": json.RawMessage(body)}
	next, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	// Publish only over the exact record read (the transaction's journal
	// snapshot), in the directory first opened, with every owned boundary
	// unchanged.
	tx.publishFence = func(string) error { return unchanged() }
	if err = tx.persistJournal("reconciled", next); err != nil {
		return "", fmt.Errorf("retained T3 transaction %s still files-restored; reconciliation journal failed: %w", name, &t3JournalError{err})
	}
	*actions = append(*actions, "reconciled retained T3 transaction "+name+" as restored; original cause, failed tree, provenance and backups retained at "+dir)
	return "updated", nil
}
