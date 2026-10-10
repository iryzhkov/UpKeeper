// Package operatorhold keeps the holds an operator places on hosts.
//
// A scheduled hold marker (package selfconv) is written by a run that may have
// died half way, and only that run may write one. An operator hold is the other
// fence: a person says "do not consume a release on this host until I say so",
// typically while a release is being prepared, canaried or rolled back. Before
// it existed the only such fence was stopping upkeeper-self.timer on every host,
// and a reboot re-armed an enabled timer through OnStartupSec.
//
// A hold is one JSON file per host under the user's state directory, written
// atomically and synced, so it survives process death and reboot. Every set,
// clear and override is appended to a per-host log beside it, so a later
// reader can see who held a host, who released it and who pulled through it.
//
// A hold blocks automatic consumption: self-pull refreshes nothing, updates no
// binary and converges nothing, and scheduled or manual pulls do not dispatch
// to the host. An explicit operator pull may pass through it with
// --override-hold, which is logged. Dry runs and observations are never held.
package operatorhold

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// SchemaVersion is the version of the hold record and of each log line.
const SchemaVersion = 1

// Hold is one operator hold on one host.
type Hold struct {
	Host   string
	Reason string
	SetBy  string
	SetAt  time.Time
	// Until is when the hold stops applying, or the zero time for a hold that
	// lasts until it is cleared.
	Until time.Time
	// Path is the record the hold was read from or written to.
	Path string
}

// Active reports whether the hold still applies at now.
func (h Hold) Active(now time.Time) bool {
	return h.Until.IsZero() || now.Before(h.Until)
}

// Document is the hold as reports and the cross-host protocol carry it.
func (h Hold) Document(now time.Time) map[string]any {
	var until any
	if !h.Until.IsZero() {
		until = pytime.ISO8601Z(h.Until)
	}
	return map[string]any{
		"host":    h.Host,
		"reason":  h.Reason,
		"set_by":  h.SetBy,
		"set_at":  pytime.ISO8601Z(h.SetAt),
		"until":   until,
		"active":  h.Active(now),
		"path":    h.Path,
		"clear":   ClearCommand(h.Host),
		"version": SchemaVersion,
	}
}

// Describe is the hold in one line, for refusals and the journal.
func (h Hold) Describe() string {
	text := fmt.Sprintf("operator hold on %s set by %s at %s", h.Host, h.SetBy, pytime.ISO8601Z(h.SetAt))
	if !h.Until.IsZero() {
		text += " until " + pytime.ISO8601Z(h.Until)
	}
	return text + ": " + h.Reason
}

// ClearCommand is the command that releases a host's hold.
func ClearCommand(host string) string {
	return "upkeeper hold clear --hosts " + host + " --reason TEXT"
}

// Directory is where the holds of the user whose home is given live. It sits
// beside the scheduled hold markers, in state that is neither a runtime
// directory nor a cache, so nothing but an operator removes it.
func Directory(home string) string {
	return filepath.Join(home, ".local", "state", "dev-fleet", "operator-holds")
}

// DefaultDirectory is Directory for the current user.
func DefaultDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", failure.Validation("cannot resolve the home directory: %v", err)
	}
	return Directory(home), nil
}

// RecordPath is the hold record of one host.
func RecordPath(directory, host string) string {
	return filepath.Join(directory, host+".json")
}

// LogPath is the append-only history of one host's holds.
func LogPath(directory, host string) string {
	return filepath.Join(directory, host+".log")
}

// record is the stored form of a hold.
type record struct {
	SchemaVersion int     `json:"schema_version"`
	Host          string  `json:"host"`
	Reason        string  `json:"reason"`
	SetBy         string  `json:"set_by"`
	SetAt         string  `json:"set_at"`
	Until         *string `json:"until"`
}

// Set records a hold, replacing any hold the host already had, and logs it.
// The log line is written before the record, so a set that dies in between
// leaves an explained attempt rather than an unexplained hold.
func Set(directory string, hold Hold) (Hold, error) {
	if _, err := safety.ValidateHostName(hold.Host, nil); err != nil {
		return Hold{}, err
	}
	if strings.TrimSpace(hold.Reason) == "" {
		return Hold{}, failure.Validation("a hold is set with a reason")
	}
	if hold.SetAt.IsZero() {
		return Hold{}, failure.Validation("a hold needs the time it was set")
	}
	if !hold.Until.IsZero() && !hold.Until.After(hold.SetAt) {
		return Hold{}, failure.Validation("a hold must end after it is set")
	}
	if strings.TrimSpace(hold.SetBy) == "" {
		hold.SetBy = "unknown"
	}
	hold.SetAt = hold.SetAt.UTC().Truncate(time.Second)
	if !hold.Until.IsZero() {
		hold.Until = hold.Until.UTC().Truncate(time.Second)
	}
	stored := record{
		SchemaVersion: SchemaVersion, Host: hold.Host, Reason: hold.Reason,
		SetBy: hold.SetBy, SetAt: pytime.ISO8601Z(hold.SetAt),
	}
	if !hold.Until.IsZero() {
		until := pytime.ISO8601Z(hold.Until)
		stored.Until = &until
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return Hold{}, err
	}
	if err := fsatomic.EnsureDirectory(directory, 0o700); err != nil {
		return Hold{}, failure.Validation("cannot create the operator hold directory: %v", err)
	}
	if err := appendEvent(directory, hold.Host, map[string]any{
		"event": "set", "at": stored.SetAt, "user": hold.SetBy,
		"reason": hold.Reason, "until": nullable(stored.Until),
	}); err != nil {
		return Hold{}, err
	}
	hold.Path = RecordPath(directory, hold.Host)
	if err := fsatomic.WriteFile(hold.Path, append(encoded, '\n'), 0o600); err != nil {
		return Hold{}, failure.Validation("cannot write the operator hold %s: %v", hold.Path, err)
	}
	_ = fsatomic.SyncDirectory(directory)
	return hold, nil
}

// Clear removes a host's hold and logs who released it and why. It reports
// false, and changes nothing, when the host has no hold.
func Clear(directory, host, reason, user string, now time.Time) (Hold, bool, error) {
	if _, err := safety.ValidateHostName(host, nil); err != nil {
		return Hold{}, false, err
	}
	if strings.TrimSpace(reason) == "" {
		return Hold{}, false, failure.Validation("a hold is cleared with a reason")
	}
	path := RecordPath(directory, host)
	hold, present, err := Read(directory, host)
	if err != nil {
		// A record that cannot be parsed still holds the host; clearing it is
		// how an operator recovers, so the bytes go into the log instead.
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return Hold{}, false, err
		}
		hold = Hold{Host: host, Path: path, Reason: "unreadable record: " + string(raw)}
		present = true
	}
	if !present {
		return Hold{}, false, nil
	}
	if err := appendEvent(directory, host, map[string]any{
		"event": "clear", "at": pytime.ISO8601Z(now), "user": user, "reason": reason,
		"held_reason": hold.Reason, "held_by": hold.SetBy,
	}); err != nil {
		return Hold{}, false, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Hold{}, false, failure.Validation("cannot remove the operator hold %s: %v", path, err)
	}
	_ = fsatomic.SyncDirectory(directory)
	return hold, true, nil
}

// Read loads one host's hold. An absent record is no hold.
func Read(directory, host string) (Hold, bool, error) {
	path := RecordPath(directory, host)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Hold{}, false, nil
	}
	if err != nil {
		return Hold{}, false, failure.Validation("cannot read the operator hold %s: %v", path, err)
	}
	var stored record
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return Hold{}, false, failure.Validation("the operator hold %s is not a hold record: %v", path, err)
	}
	if stored.SchemaVersion != SchemaVersion || stored.Host != host {
		return Hold{}, false, failure.Validation(
			"the operator hold %s is for %q at schema %d, not for %s", path, stored.Host, stored.SchemaVersion, host)
	}
	setAt, err := time.Parse(time.RFC3339, stored.SetAt)
	if err != nil {
		return Hold{}, false, failure.Validation("the operator hold %s has no valid set_at: %v", path, err)
	}
	hold := Hold{Host: host, Reason: stored.Reason, SetBy: stored.SetBy, SetAt: setAt, Path: path}
	if stored.Until != nil {
		if hold.Until, err = time.Parse(time.RFC3339, *stored.Until); err != nil {
			return Hold{}, false, failure.Validation("the operator hold %s has no valid until: %v", path, err)
		}
	}
	return hold, true, nil
}

// List reads every hold under a directory, expired ones included, in host
// order. An absent directory is an empty list.
func List(directory string) ([]Hold, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return []Hold{}, nil
	}
	if err != nil {
		return nil, failure.Validation("cannot read the operator hold directory: %v", err)
	}
	holds := []Hold{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		hold, present, err := Read(directory, strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		if present {
			holds = append(holds, hold)
		}
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].Host < holds[j].Host })
	return holds, nil
}

// Check answers whether a host is held at now. It fails closed: a record that
// exists but cannot be read holds the host, with the read error as its reason,
// because a hold nobody can read is still somebody's instruction to wait.
func Check(directory, host string, now time.Time) (Hold, bool) {
	hold, present, err := Read(directory, host)
	if err != nil {
		return Hold{
			Host: host, Path: RecordPath(directory, host), SetBy: "unknown",
			Reason: err.Error() + "; inspect it, then " + ClearCommand(host),
		}, true
	}
	if !present || !hold.Active(now) {
		return Hold{}, false
	}
	return hold, true
}

// RecordOverride logs that an explicit operator run passed through a hold.
func RecordOverride(directory string, hold Hold, user, command, runID string, now time.Time) error {
	return appendEvent(directory, hold.Host, map[string]any{
		"event": "override", "at": pytime.ISO8601Z(now), "user": user,
		"command": command, "run_id": runID,
		"held_reason": hold.Reason, "held_by": hold.SetBy,
	})
}

// UserName is the operator's account name for the log, or "unknown".
func UserName() string {
	for _, key := range []string{"USER", "LOGNAME"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return "unknown"
}

func appendEvent(directory, host string, event map[string]any) error {
	event["schema_version"] = SchemaVersion
	event["host"] = host
	line, err := canonjson.Compact(event)
	if err != nil {
		return err
	}
	if err := fsatomic.EnsureDirectory(directory, 0o700); err != nil {
		return failure.Validation("cannot create the operator hold directory: %v", err)
	}
	path := LogPath(directory, host)
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return failure.Validation("cannot record the hold event in %s: %v", path, err)
	}
	if _, err := handle.Write(append(line, '\n')); err != nil {
		_ = handle.Close()
		return failure.Validation("cannot record the hold event in %s: %v", path, err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return failure.Validation("cannot record the hold event in %s: %v", path, err)
	}
	return handle.Close()
}

func nullable(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
