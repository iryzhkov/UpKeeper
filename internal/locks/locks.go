// Package locks implements the deployment lock that keeps two controllers from
// converging the same host at once.
//
// The lock is a directory created with mkdir, which is atomic on every
// filesystem the fleet uses, plus an owner record created with O_EXCL. Release
// is exact: a run may only release the lock it owns, so a stale process can
// never free a live deployment's lock. A lock is declared stale only when the
// owning controller is *known* to be gone; uncertainty keeps the lock.
package locks

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
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

// Metadata identifies the run that owns a lock.
type Metadata struct {
	SchemaVersion         int      `json:"schema_version"`
	RunID                 string   `json:"run_id"`
	Controller            string   `json:"controller"`
	ControllerPID         int      `json:"controller_pid"`
	DesiredManifestCommit string   `json:"desired_manifest_commit"`
	Components            []string `json:"components"`
	StartedAt             string   `json:"started_at"`
}

// NewMetadata validates and builds a lock owner record.
func NewMetadata(
	runID, controller string,
	controllerPID int,
	desiredManifestCommit string,
	components []string,
	now time.Time,
) (Metadata, error) {
	if controllerPID <= 0 {
		return Metadata{}, failure.Value("controller_pid must be positive")
	}
	for _, component := range components {
		if _, err := safety.ValidateComponentName(component, nil); err != nil {
			return Metadata{}, err
		}
	}
	validatedRunID, err := safety.ValidateRunID(runID)
	if err != nil {
		return Metadata{}, err
	}
	validatedController, err := safety.ValidateHostName(controller, nil)
	if err != nil {
		return Metadata{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	return Metadata{
		SchemaVersion:         exitcode.SchemaVersion,
		RunID:                 validatedRunID,
		Controller:            validatedController,
		ControllerPID:         controllerPID,
		DesiredManifestCommit: desiredManifestCommit,
		Components:            uniqueSorted(components),
		StartedAt:             pytime.ISO8601Z(now),
	}, nil
}

// Inspection is what a lock directory currently says.
type Inspection struct {
	Host     string
	State    string // absent, held or invalid
	Metadata *Metadata
	Reason   string
}

// OrderedHosts returns a deterministic acquisition order and refuses a host set
// that names the same host twice.
func OrderedHosts(hosts []string) ([]string, error) {
	checked := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		name, err := safety.ValidateHostName(host, nil)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, failure.Value("host lock set contains duplicates")
		}
		seen[name] = true
		checked = append(checked, name)
	}
	sort.Strings(checked)
	return checked, nil
}

// AliveCheck reports whether a controller process is still running. A nil
// result means the question could not be answered.
type AliveCheck func(controller string, pid int) *bool

// StaleConfirmed reports whether a lock may be broken.
//
// Age alone is never enough. The owning controller must be positively known to
// be gone, because breaking a live deployment's lock is worse than waiting.
func StaleConfirmed(
	metadata Metadata, now time.Time, maximumAge time.Duration, alive AliveCheck,
) (bool, error) {
	if maximumAge <= 0 {
		return false, failure.Value("maximum_age_seconds must be positive")
	}
	started, err := pytime.ParseISO8601Z(metadata.StartedAt)
	if err != nil {
		return false, failure.Value("lock owner record has an unreadable started_at")
	}
	if now.UTC().Sub(started.UTC()) <= maximumAge {
		return false, nil
	}
	running := alive(metadata.Controller, metadata.ControllerPID)
	return running != nil && !*running, nil
}

// Store is the on-disk lock directory.
type Store struct {
	Root string
}

// Default returns the lock store every fleet host uses.
func Default() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, failure.Validation("cannot resolve the home directory: %v", err)
	}
	return &Store{Root: filepath.Join(home, ".local", "state", "dev-fleet", "locks")}, nil
}

func (s *Store) directory(host string) (string, error) {
	name, err := safety.ValidateHostName(host, nil)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.Root, name+".lock"), nil
}

// Inspect reads a lock without taking or breaking it.
func (s *Store) Inspect(host string) (Inspection, error) {
	directory, err := s.directory(host)
	if err != nil {
		return Inspection{}, err
	}
	info, err := os.Stat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return Inspection{Host: host, State: "absent"}, nil
	}
	if err != nil {
		return Inspection{Host: host, State: "invalid", Reason: "OSError"}, nil
	}
	if !info.IsDir() {
		return Inspection{Host: host, State: "invalid", Reason: "not-directory"}, nil
	}
	data, err := os.ReadFile(filepath.Join(directory, "owner.json"))
	if err != nil {
		return Inspection{Host: host, State: "invalid", Reason: "OSError"}, nil
	}
	var metadata Metadata
	if err := decodeMetadata(data, &metadata); err != nil {
		return Inspection{Host: host, State: "invalid", Reason: "ValueError"}, nil
	}
	if _, err := safety.ValidateRunID(metadata.RunID); err != nil {
		return Inspection{Host: host, State: "invalid", Reason: "ValueError"}, nil
	}
	return Inspection{Host: host, State: "held", Metadata: &metadata}, nil
}

// Acquire takes the lock for a host, or reports contention.
func (s *Store) Acquire(host string, metadata Metadata) (string, error) {
	if err := fsatomic.EnsureDirectory(s.Root, 0o700); err != nil {
		return "", err
	}
	directory, err := s.directory(host)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", failure.LockContention("%s is already locked", host)
		}
		return "", err
	}
	generic, err := canonjson.Normalize(metadata)
	if err != nil {
		_ = os.Remove(directory)
		return "", err
	}
	payload, err := canonjson.MarshalSpaced(redact.Value(generic))
	if err != nil {
		_ = os.Remove(directory)
		return "", err
	}
	owner := filepath.Join(directory, "owner.json")
	if err := fsatomic.CreateExclusive(owner, append(payload, '\n'), 0o600); err != nil {
		_ = os.Remove(owner)
		_ = os.Remove(directory)
		return "", err
	}
	return directory, nil
}

// Release drops a lock this run owns, and refuses to touch anyone else's.
func (s *Store) Release(host, runID string) error {
	expected, err := safety.ValidateRunID(runID)
	if err != nil {
		return err
	}
	inspection, err := s.Inspect(host)
	if err != nil {
		return err
	}
	if inspection.State != "held" || inspection.Metadata == nil {
		return failure.LockContention("%s has no valid owned lock", host)
	}
	if inspection.Metadata.RunID != expected {
		return failure.LockContention("%s lock belongs to another run", host)
	}
	directory, err := s.directory(host)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(directory, "owner.json")); err != nil {
		return err
	}
	return os.Remove(directory)
}

// AcquireAll takes every lock or none: a partial acquisition is rolled back
// before the error is returned.
func (s *Store) AcquireAll(hosts []string, metadata Metadata) ([]string, error) {
	ordered, err := OrderedHosts(hosts)
	if err != nil {
		return nil, err
	}
	acquired := make([]string, 0, len(ordered))
	for _, host := range ordered {
		if _, err := s.Acquire(host, metadata); err != nil {
			for index := len(acquired) - 1; index >= 0; index-- {
				_ = s.Release(acquired[index], metadata.RunID)
			}
			return nil, err
		}
		acquired = append(acquired, host)
	}
	return acquired, nil
}

func decodeMetadata(data []byte, metadata *Metadata) error {
	object, err := canonjson.DecodeObject(data)
	if err != nil {
		return err
	}
	// The Python reader built the dataclass by keyword, so an owner record with
	// unexpected or missing fields was invalid rather than partially accepted.
	expected := map[string]bool{
		"schema_version": true, "run_id": true, "controller": true,
		"controller_pid": true, "desired_manifest_commit": true,
		"components": true, "started_at": true,
	}
	if len(object) != len(expected) {
		return failure.Value("lock owner record has unexpected fields")
	}
	for key := range object {
		if !expected[key] {
			return failure.Value("lock owner record has unexpected fields")
		}
	}
	return decodeInto(object, metadata)
}

func decodeInto(object map[string]any, metadata *Metadata) error {
	encoded, err := canonjson.Compact(object)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	return decoder.Decode(metadata)
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}
