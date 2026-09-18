package selfconv

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/runstore"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// ErrNoHold is returned when a host has no marker to clear: nothing to do,
// which the CLI reports as a malformed request rather than as a failure.
var ErrNoHold = errors.New("no hold marker")

// holdRunPattern finds the run identifier a marker names.
var holdRunPattern = regexp.MustCompile(`run ([0-9]{8}T[0-9]{6}Z-[0-9a-f]{8})`)

// HoldMarker is one scheduled hold as an operator sees it.
type HoldMarker struct {
	Host    string
	Path    string
	Content string
	// ModifiedAt is when the marker was written; Age is measured from the
	// caller's clock so a listing and a test agree on it.
	ModifiedAt time.Time
	Age        time.Duration
	// RunID is the run named inside the marker, or "" for a marker written
	// before markers named their run.
	RunID string
	// RunRecord and RunResult come from the run store when it holds that run;
	// both are empty when it does not.
	RunRecord string
	RunResult string
}

// ListHolds reads every marker under a hold directory. An absent directory
// is an empty list: no scheduled dispatch has ever run from this host.
func ListHolds(directory string, store *runstore.Store, now time.Time) ([]HoldMarker, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return []HoldMarker{}, nil
	}
	if err != nil {
		return nil, failure.Validation("cannot read the scheduled hold directory: %v", err)
	}
	markers := []HoldMarker{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".hold") || entry.IsDir() {
			continue
		}
		marker, err := readHold(directory, strings.TrimSuffix(name, ".hold"), store, now)
		if err != nil {
			return nil, err
		}
		markers = append(markers, marker)
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].Host < markers[j].Host })
	return markers, nil
}

func readHold(directory, host string, store *runstore.Store, now time.Time) (HoldMarker, error) {
	path := HoldPath(directory, host)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return HoldMarker{}, ErrNoHold
		}
		return HoldMarker{}, failure.Validation("cannot read the hold marker %s: %v", path, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return HoldMarker{}, failure.Validation("cannot read the hold marker %s: %v", path, err)
	}
	marker := HoldMarker{
		Host: host, Path: path, Content: string(content),
		ModifiedAt: info.ModTime(), Age: now.Sub(info.ModTime()),
	}
	if match := holdRunPattern.FindStringSubmatch(marker.Content); match != nil {
		marker.RunID = match[1]
		if store != nil {
			if record, err := store.Read(marker.RunID); err == nil {
				marker.RunRecord = store.Path(marker.RunID)
				marker.RunResult, _ = record["result"].(string)
			}
		}
	}
	return marker, nil
}

// ClearHold removes exactly one host's marker and appends why to the sibling
// log <host>.hold.cleared, so a later reader can see who cleared it, when,
// and what the marker said. The log is written before the marker goes: a
// clear that dies in between leaves the marker, never an unexplained gap.
func ClearHold(
	directory, host, reason, user string, now time.Time,
) (HoldMarker, string, error) {
	if _, err := safety.ValidateHostName(host, nil); err != nil {
		return HoldMarker{}, "", err
	}
	if strings.TrimSpace(reason) == "" {
		return HoldMarker{}, "", failure.Validation("a hold is cleared with a reason")
	}
	marker, err := readHold(directory, host, nil, now)
	if err != nil {
		return HoldMarker{}, "", err
	}
	entry := map[string]any{
		"schema_version": 1,
		"host":           host,
		"cleared_at":     pytime.ISO8601Z(now),
		"user":           user,
		"reason":         reason,
		"marker":         marker.Path,
		"marker_written": pytime.ISO8601Z(marker.ModifiedAt),
		"run_id":         marker.RunID,
		"content":        marker.Content,
	}
	line, err := canonjson.Compact(entry)
	if err != nil {
		return HoldMarker{}, "", err
	}
	logPath := marker.Path + ".cleared"
	if err := appendLine(logPath, line); err != nil {
		return HoldMarker{}, "", failure.Validation("cannot record the clear in %s: %v", logPath, err)
	}
	if err := os.Remove(marker.Path); err != nil {
		return HoldMarker{}, "", failure.Validation("cannot remove the hold marker %s: %v", marker.Path, err)
	}
	_ = fsatomic.SyncDirectory(directory)
	return marker, logPath, nil
}

func appendLine(path string, line []byte) error {
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := handle.Write(append(line, '\n')); err != nil {
		_ = handle.Close()
		return err
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return err
	}
	return handle.Close()
}

// UserName is the operator's account name for the cleared log, or "unknown".
func UserName() string {
	for _, key := range []string{"USER", "LOGNAME"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return "unknown"
}
