package agent

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

func fleetSameBytes(a, b []byte) bool { return bytes.Equal(a, b) && (a == nil) == (b == nil) }

// Open each ancestor relative to the preceding descriptor, never following a
// symlink. Rename/removal then stays in the checked directory even if an
// ancestor is concurrently replaced.
func fleetOpenParent(home, target string) (*os.File, error) {
	rel, err := filepath.Rel(home, filepath.Dir(target))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, errors.New("fleet path escapes home")
	}
	fd, err := syscall.Open(filepath.Clean(home), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), filepath.Dir(target)), nil
}

func fleetReadAt(directory *os.File, name string) ([]byte, error) {
	fd, err := syscall.Openat(int(directory.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > fleetCoordinatorMaxBytes {
		return nil, errors.New("unsafe fleet projection")
	}
	b, err := io.ReadAll(io.LimitReader(f, fleetCoordinatorMaxBytes+1))
	if len(b) > fleetCoordinatorMaxBytes {
		return nil, errors.New("fleet projection exceeds bound")
	}
	return b, err
}

func fleetRetainedBytes(entry map[string]any) ([]byte, error) {
	content, ok := entry["content"].(string)
	if !ok {
		return nil, errors.New("retained fleet record has no content to restore")
	}
	encoding, present := entry["encoding"]
	if !present {
		return []byte(content), nil
	} // Historical UTF-8 records.
	if encoding != "base64/v1" {
		return nil, errors.New("unsupported fleet retention encoding")
	}
	b, err := base64.StdEncoding.Strict().DecodeString(content)
	if err != nil || base64.StdEncoding.EncodeToString(b) != content {
		return nil, errors.New("invalid fleet retention encoding")
	}
	return b, nil
}

// Only changes made by this invocation are eligible for automatic rollback.
// Refuse subsequent operator drift rather than claiming ownership over it.
func fleetRestoreEffects(c *Context, documents []*fleetDocument) error {
	var failures []error
	for _, p := range documents {
		if !p.effected {
			continue
		}
		directory, err := fleetOpenParent(c.Home, p.target)
		if err == nil {
			var current []byte
			current, err = fleetReadAt(directory, filepath.Base(p.target))
			directory.Close()
			if err == nil && !fleetSameBytes(current, p.payload) {
				err = errors.New("projection drifted after activation; preserved")
			}
		}
		if err == nil {
			rollback := &fleetDocument{destination: p.destination, target: p.target, before: p.payload, payload: p.before, remove: p.before == nil, restorePolicy: true}
			err = fleetActivate(rollback)
		}
		if err == nil {
			if p.destination == fleet.RoutePolicyDestination {
				err = fleetReconcilePolicyReceipt(c)
			}
			if p.destination == fleet.ClientDestination {
				err = fleetReconcileClientAfterPolicyRollback(c)
			}
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
