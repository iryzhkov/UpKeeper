package author

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const sharedFileLimit int64 = 16 << 20
const sharedAggregateLimit int64 = 64 << 20

func sharedRead(path string, limit int64) ([]byte, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	raw, verify, closeInput, err := acquireSharedFile(path, limit, nil)
	if err != nil {
		return nil, nil, err
	}
	defer closeInput()
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, nil, errors.New("shared file identity changed")
	}
	if err := verify(); err != nil {
		return nil, nil, err
	}
	return raw, after, nil
}

func sharedSourceLimits() map[string]int64 {
	limits := map[string]int64{"owner.json": 256 << 10}
	for _, path := range SharedSourcePaths() {
		limits[path] = sharedFileLimit
	}
	return limits
}
func sharedOutputLimits() map[string]int64 {
	limits := map[string]int64{}
	for _, path := range policyInstructions {
		limits[path] = sharedFileLimit
	}
	return limits
}

// Inventory closure is checked before any data read. Snapshots use the same
// secure acquisition as policy inputs, with kind-specific bounds and a closed
// aggregate; unknown outputs cannot trigger an unbounded read.
func sharedTreeState(root string, limits map[string]int64, reserve int64) (map[string]treeEntry, error) {
	dirs := map[string]bool{".": true}
	for path := range limits {
		for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
			dirs[dir] = true
		}
	}
	state := map[string]treeEntry{}
	files := []string{}
	bindings := []func() error{}
	closers := []func(){}
	defer func() {
		for _, closeInput := range closers {
			closeInput()
		}
	}()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if len(state) >= 1024 {
			return errors.New("shared tree entry bound")
		}
		if entry.IsDir() {
			if !dirs[relative] {
				return errors.New("shared unexpected directory")
			}
			verify, closeInput, err := acquireSharedSnapshotDirectory(path)
			if err != nil {
				return err
			}
			bindings = append(bindings, verify)
			closers = append(closers, closeInput)
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			state[relative] = treeEntry{Mode: info.Mode()}
		} else {
			if _, ok := limits[relative]; !ok {
				return errors.New("shared unexpected file")
			}
			if !entry.Type().IsRegular() {
				return errors.New("shared file type")
			}
			files = append(files, relative)
			state[relative] = treeEntry{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) != len(limits) || len(state) != len(limits)+len(dirs) {
		return nil, errors.New("shared tree closure")
	}
	total := reserve
	if total < 0 || total > sharedAggregateLimit {
		return nil, errors.New("shared payload aggregate")
	}
	for _, relative := range files {
		limit := limits[relative]
		if relative != "owner.json" && sharedAggregateLimit-total < limit {
			limit = sharedAggregateLimit - total
		}
		raw, info, err := sharedRead(filepath.Join(root, relative), limit)
		if err != nil {
			return nil, err
		}
		// Actual descriptor metadata is bounded separately.
		if relative != "owner.json" {
			total += int64(len(raw))
			if total > sharedAggregateLimit {
				return nil, errors.New("shared payload aggregate")
			}
		}
		item := treeEntry{Mode: info.Mode(), Digest: digestOf(raw), Modified: info.ModTime().UnixNano()}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			item.Device, item.Inode = uint64(st.Dev), st.Ino
		}
		state[relative] = item
	}
	// Rebind every file after the complete traversal, so an earlier file cannot
	// change unnoticed while later files are being acquired.
	for _, relative := range files {
		raw, info, err := sharedRead(filepath.Join(root, relative), limits[relative])
		if err != nil {
			return nil, err
		}
		item := treeEntry{Mode: info.Mode(), Digest: digestOf(raw), Modified: info.ModTime().UnixNano()}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			item.Device, item.Inode = uint64(st.Dev), st.Ino
		}
		if item != state[relative] {
			return nil, errors.New("shared snapshot file changed")
		}
	}
	for _, verify := range bindings {
		if err := verify(); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func sharedPayloadReserve(source string, policy []byte) (int64, error) {
	total := int64(len(policy))
	for _, path := range SharedSourcePaths() {
		if !strings.HasPrefix(path, "config/claude/skills/") {
			continue
		}
		limit := sharedFileLimit
		if sharedAggregateLimit-total < limit {
			limit = sharedAggregateLimit - total
		}
		_, info, err := sharedRead(filepath.Join(source, path), limit)
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	if total > sharedAggregateLimit {
		return 0, errors.New("shared payload aggregate")
	}
	return total, nil
}

func sharedSourceState(source string) (map[string]treeEntry, error) {
	return sharedTreeState(source, sharedSourceLimits(), 0)
}
