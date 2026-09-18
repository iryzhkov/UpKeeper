// Package runstore keeps the append-only local record of every mutating run.
//
// Read-only commands deliberately never write one: doctor and status must be
// safe to run against a fleet that is mid-deployment. A record is redacted
// before it reaches the disk, so a run that handled a secret cannot leave it in
// the state directory.
package runstore

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/redact"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// Record is one run as it is persisted.
type Record struct {
	SchemaVersion         int            `json:"schema_version"`
	RunID                 string         `json:"run_id"`
	Command               string         `json:"command"`
	Controller            string         `json:"controller"`
	DesiredManifestCommit *string        `json:"desired_manifest_commit"`
	Components            []string       `json:"components"`
	Hosts                 []string       `json:"hosts"`
	Phase                 string         `json:"phase"`
	Result                string         `json:"result"`
	CreatedAt             string         `json:"created_at"`
	Details               map[string]any `json:"details"`
}

// New starts a record in its "created"/"running" state.
func New(
	runID, command, controller string,
	desiredManifestCommit *string,
	components, hosts []string,
) (*Record, error) {
	if _, err := safety.ValidateRunID(runID); err != nil {
		return nil, err
	}
	return &Record{
		SchemaVersion:         exitcode.SchemaVersion,
		RunID:                 runID,
		Command:               command,
		Controller:            controller,
		DesiredManifestCommit: desiredManifestCommit,
		Components:            nonNil(components),
		Hosts:                 nonNil(hosts),
		Phase:                 "created",
		Result:                "running",
		CreatedAt:             pytime.ISO8601Z(time.Now()),
		Details:               map[string]any{},
	}, nil
}

// Store writes records below an explicit root. Writing is always a deliberate
// call; constructing a store changes nothing on disk.
type Store struct {
	Root string
}

// Default returns the store every fleet host uses.
func Default() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, failure.Validation("cannot resolve the home directory: %v", err)
	}
	return &Store{Root: filepath.Join(home, ".local", "state", "dev-fleet", "runs")}, nil
}

// Write persists a redacted record and returns the path it now occupies.
func (s *Store) Write(record *Record) (string, error) {
	if _, err := safety.ValidateRunID(record.RunID); err != nil {
		return "", err
	}
	if err := fsatomic.EnsureDirectory(s.Root, 0o700); err != nil {
		return "", err
	}
	generic, err := canonjson.Normalize(record)
	if err != nil {
		return "", err
	}
	payload, err := canonjson.MarshalFile(redact.Value(generic))
	if err != nil {
		return "", err
	}
	target := filepath.Join(s.Root, record.RunID+".json")
	if err := fsatomic.WritePrivateFile(target, payload); err != nil {
		return "", err
	}
	return target, nil
}

// Path is where a record with this run identifier lives, whether or not it
// has been written.
func (s *Store) Path(runID string) string {
	return filepath.Join(s.Root, runID+".json")
}

// Read returns a previously written record as a decoded document.
func (s *Store) Read(runID string) (map[string]any, error) {
	if _, err := safety.ValidateRunID(runID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.Root, runID+".json"))
	if err != nil {
		return nil, err
	}
	return canonjson.DecodeObject(data)
}

// ListIDs returns every valid run identifier in the store, oldest first.
func (s *Store) ListIDs() []string {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return nil
	}
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		stem := name[:len(name)-len(".json")]
		if _, err := safety.ValidateRunID(stem); err != nil {
			continue
		}
		values = append(values, stem)
	}
	sort.Strings(values)
	return values
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
