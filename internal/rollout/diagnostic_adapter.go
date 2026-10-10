package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// This projection matches Steward 3d55d6ff's authenticated admin read API.
// Unknown producer fields are allowed: admin worker documents include runtime,
// catalog and authorization fields irrelevant to this narrowly bounded predicate.
type diagnosticWorkers struct {
	Version string             `json:"version"`
	Kind    string             `json:"kind"`
	Workers []diagnosticWorker `json:"workers"`
}
type diagnosticWorker struct {
	Requirement *struct {
		WorkerID    string `json:"workerId"`
		WorkerEpoch string `json:"workerEpoch"`
	} `json:"requirement"`
	Health   string `json:"health"`
	State    string `json:"state"`
	Stale    *bool  `json:"stale"`
	Snapshot struct {
		WorkerID         string    `json:"workerId"`
		WorkerEpoch      string    `json:"workerEpoch"`
		CoordinatorEpoch int64     `json:"coordinatorEpoch"`
		Connected        bool      `json:"connected"`
		ObservedAt       time.Time `json:"observedAt"`
		ValidUntil       time.Time `json:"validUntil"`
		Inventory        struct {
			ID           string   `json:"id"`
			Health       string   `json:"health"`
			Capabilities []string `json:"capabilities"`
			Telemetry    *struct {
				RunningAttempts *int `json:"running_attempts"`
			} `json:"telemetry"`
			Providers []struct {
				InstanceID string   `json:"instanceId"`
				Available  bool     `json:"available"`
				Models     []string `json:"models"`
			} `json:"providers"`
		} `json:"inventory"`
		// Steward explicitly omits an empty assignment slice. With the required
		// reporting capability and a fresh full snapshot, omission means no entries.
		Assignments []struct {
			AssignmentID string    `json:"assignmentId"`
			State        string    `json:"state"`
			ObservedAt   time.Time `json:"observedAt"`
			Journal      *struct {
				Phase     string    `json:"phase"`
				TurnEnd   string    `json:"turnEnd"`
				UpdatedAt time.Time `json:"updatedAt"`
			} `json:"journal"`
		} `json:"assignments"`
	} `json:"snapshot"`
}
type diagnosticIdentity struct {
	Version       string `json:"version"`
	Kind          string `json:"kind"`
	CoordinatorID string `json:"coordinatorId"`
	Epoch         int64  `json:"epoch"`
	Health        string `json:"health"`
}

func decodeDiagnostic(p []byte, v any) error {
	if len(p) == 0 || len(p) > 4<<20 {
		return errors.New("empty or oversized producer document")
	}
	check := json.NewDecoder(bytes.NewReader(p))
	if err := diagnosticJSONValue(check, 0); err != nil {
		return err
	}
	if _, err := check.Token(); err != io.EOF {
		return errors.New("trailing producer JSON")
	}
	d := json.NewDecoder(bytes.NewReader(p))
	if err := d.Decode(v); err != nil {
		return errors.New("invalid producer JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing producer JSON")
	}
	return nil
}

// Duplicate keys must not let contradictory acceptance facts disappear.
func diagnosticJSONValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("producer JSON nesting refused")
	}
	token, err := d.Token()
	if err != nil {
		return errors.New("invalid producer JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return errors.New("invalid producer JSON")
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate producer JSON key")
			}
			seen[name] = true
			if err = diagnosticJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = diagnosticJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid producer JSON")
	}
	_, err = d.Token()
	return err
}
func diagnosticFresh(stamp time.Time, now time.Time) bool {
	return !stamp.IsZero() && !stamp.After(now) && now.Sub(stamp) <= 60*time.Second
}
func selectDiagnosticWorker(p []byte, host string, epoch int64, now time.Time) (*diagnosticWorker, error) {
	var doc diagnosticWorkers
	if err := decodeDiagnostic(p, &doc); err != nil {
		return nil, err
	}
	if doc.Version != "backlog.admin/v1" || doc.Kind != "workers" || doc.Workers == nil {
		return nil, errors.New("unsupported worker document")
	}
	var selected *diagnosticWorker
	for i := range doc.Workers {
		w := &doc.Workers[i]
		if w.Requirement != nil && w.Requirement.WorkerID == host {
			if selected != nil {
				return nil, errors.New("duplicate worker")
			}
			selected = w
		}
	}
	if selected == nil {
		return nil, errors.New("worker missing")
	}
	s := &selected.Snapshot
	if selected.Stale == nil || *selected.Stale || selected.Health != "ready" || (selected.State != "observed" && selected.State != "draining") || !s.Connected || s.WorkerID != host || selected.Requirement.WorkerEpoch == "" || s.WorkerEpoch == "" || s.WorkerEpoch != selected.Requirement.WorkerEpoch || s.CoordinatorEpoch != epoch || s.Inventory.ID != host || s.Inventory.Health != "ready" || !diagnosticFresh(s.ObservedAt, now) || !s.ValidUntil.After(now) || s.ValidUntil.Before(s.ObservedAt) || s.ValidUntil.Sub(s.ObservedAt) > 5*time.Minute {
		return nil, errors.New("worker identity, readiness or freshness refused")
	}
	return selected, nil
}

// Diagnostic is a read-only checked transformation. It never launches provider
// work, and activity is intentionally quiet-only rather than a heartbeat proxy.
func (b *ProductionBackend) Diagnostic(ctx context.Context, c Config, kind, host string, epoch int64) (any, error) {
	if (kind != "activity" && kind != "haiku") || epoch <= 0 || host == "" || c.Targets[host] == "" {
		return nil, errors.New("invalid diagnostic binding")
	}
	p, err := b.command(ctx, c, "", "t3-steward", "coordinator", "identity", "--json")
	if err != nil {
		return nil, errors.New("coordinator producer failed")
	}
	var id diagnosticIdentity
	if err = decodeDiagnostic(p, &id); err != nil {
		return nil, err
	}
	if id.Version != "backlog.admin/v1" || id.Kind != "coordinator-identity" || id.CoordinatorID == "" || id.Epoch != epoch || id.Health != "healthy" {
		return nil, errors.New("coordinator identity refused")
	}
	p, err = b.command(ctx, c, "", "t3-steward", "worker", "list", "--json")
	if err != nil {
		return nil, errors.New("worker producer failed")
	}
	now := b.Now()
	w, err := selectDiagnosticWorker(p, host, epoch, now)
	if err != nil {
		return nil, err
	}
	// Bind acceptance to a coordinator answer after the worker read as well.
	p, err = b.command(ctx, c, "", "t3-steward", "coordinator", "identity", "--json")
	if err != nil {
		return nil, errors.New("final coordinator producer failed")
	}
	var final diagnosticIdentity
	if err = decodeDiagnostic(p, &final); err != nil {
		return nil, err
	}
	if final != id {
		return nil, errors.New("coordinator changed during diagnostic")
	}
	// The row state can collapse recovery-required into draining. Identity and
	// reporting evidence must remain independently checked for both diagnostics.
	if !contains(w.Snapshot.Inventory.Capabilities, "turn-end-commands-v1") {
		return nil, errors.New("unsupported worker reporting capability")
	}
	finalNow := b.Now()
	if finalNow.Before(now) {
		return nil, errors.New("clock regressed during diagnostic")
	}
	if !diagnosticFresh(w.Snapshot.ObservedAt, finalNow) || !w.Snapshot.ValidUntil.After(finalNow) {
		return nil, errors.New("worker snapshot expired during diagnostic")
	}
	for _, a := range w.Snapshot.Assignments {
		if !diagnosticFresh(a.ObservedAt, finalNow) || a.Journal == nil || a.Journal.UpdatedAt.IsZero() || a.Journal.UpdatedAt.After(finalNow) {
			return nil, errors.New("assignment freshness refused")
		}
	}
	now = finalNow
	if kind == "haiku" {
		matches := 0
		available := false
		for _, provider := range w.Snapshot.Inventory.Providers {
			if provider.InstanceID == "claudeAgent" {
				matches++
				available = provider.Available && contains(provider.Models, "claude-haiku-5-5")
			}
		}
		if matches != 1 || !available {
			return nil, errors.New("literal available Haiku advertisement missing or duplicated")
		}
		return struct {
			WorkerID string `json:"worker_id"`
			Epoch    string `json:"coordinator_epoch"`
			Model    string `json:"model"`
		}{host, strconv.FormatInt(epoch, 10), "claudeAgent/claude-haiku-5-5"}, nil
	}
	// The supported producer uses draining for catalog reconciliation. Haiku
	// checks may precede named enrollment; activity acceptance needs observed.
	if w.State != "observed" && !(w.State == "draining" && contains(c.DrainedHosts, host)) {
		return nil, errors.New("activity requires reconciled observed worker")
	}
	if !contains(w.Snapshot.Inventory.Capabilities, "turn-end-commands-v1") || w.Snapshot.Inventory.Telemetry == nil || w.Snapshot.Inventory.Telemetry.RunningAttempts == nil || *w.Snapshot.Inventory.Telemetry.RunningAttempts != 0 {
		return nil, errors.New("quiet-only reporting capability or zero running-work evidence missing")
	}
	seen := map[string]bool{}
	for _, a := range w.Snapshot.Assignments {
		if a.AssignmentID == "" || seen[a.AssignmentID] || (a.State != "released" && a.State != "completed") {
			return nil, errors.New("active, unknown or duplicate assignment refused")
		}
		seen[a.AssignmentID] = true
		if !diagnosticFresh(a.ObservedAt, now) || a.Journal == nil || !contains([]string{"completed", "failed", "stopped"}, a.Journal.Phase) || a.Journal.UpdatedAt.IsZero() || a.Journal.UpdatedAt.After(now) {
			return nil, errors.New("assignment journal evidence missing")
		}
		// Any reported turn-end hold, including a malformed deferred note, refuses.
		if a.Journal.TurnEnd != "" {
			return nil, fmt.Errorf("collection or turn-end hold reported")
		}
	}
	return struct {
		Host     string `json:"host"`
		Epoch    string `json:"coordinator_epoch"`
		Deferred bool   `json:"collection_deferred"`
		Inactive bool   `json:"running_without_activity"`
	}{host, strconv.FormatInt(epoch, 10), false, false}, nil
}
