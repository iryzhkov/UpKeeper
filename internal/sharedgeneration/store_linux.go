//go:build linux

package sharedgeneration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"

	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

type Store struct {
	root               string
	rootStat, lockStat syscall.Stat_t
	h                  hooks
}
type transaction struct {
	s    *Store
	x    *filesystem
	root int
	lock *os.File
	dirs map[string]syscall.Stat_t
}

func Open(root string) (*Store, error) { return openStore(root, hooks{}) }
func openStore(root string, h hooks) (*Store, error) {
	x, e := newFilesystem(h)
	if e != nil {
		return nil, e
	}
	defer x.close()
	held, e := absolute(root, true)
	if e != nil {
		return nil, e
	}
	defer syscall.Close(held)
	rs, e := stat(held)
	if e != nil {
		return nil, e
	}
	if e = owned(rs, 0700, true); e != nil {
		return nil, e
	}
	names, e := x.names(held)
	if e != nil {
		return nil, e
	}
	creator := false
	var lock *os.File
	if len(names) == 0 {
		fd, e := syscall.Openat(held, "lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
		if e == nil {
			creator = true
			lock = os.NewFile(uintptr(fd), "lock")
			if e = syscall.Fchmod(fd, 0600); e != nil {
				lock.Close()
				return nil, recovery(e)
			}
		} else if !errors.Is(e, syscall.EEXIST) {
			return nil, e
		}
	}
	if lock == nil {
		fd, e := relative(held, "lock", false)
		if e != nil {
			return nil, recovery(e)
		}
		st, e := stat(fd)
		if e != nil {
			syscall.Close(fd)
			return nil, e
		}
		if e = owned(st, 0600, false); e != nil {
			syscall.Close(fd)
			return nil, e
		}
		lock, e = x.bridge(fd, syscall.O_RDWR)
		syscall.Close(fd)
		if e != nil {
			return nil, e
		}
	}
	defer lock.Close()
	ls, e := stat(int(lock.Fd()))
	if e != nil {
		return nil, e
	}
	if e = owned(ls, 0600, false); e != nil {
		return nil, e
	}
	if ls.Size != 0 {
		return nil, errors.New("lock size")
	}
	if creator {
		if e = x.point("lock-created"); e != nil {
			return nil, recovery(e)
		}
	}
	if e = x.point("before-flock"); e != nil {
		return nil, recovery(e)
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return nil, e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	s := &Store{root, rs, ls, h}
	t := &transaction{s, x, held, lock, map[string]syscall.Stat_t{}}
	if e = t.bind(); e != nil {
		return nil, recovery(e)
	}
	if creator {
		names, e = x.names(held)
		if e != nil || len(names) != 1 || names[0] != "lock" {
			return nil, recovery(errors.New("initializer root changed"))
		}
		if e = x.point("fsync:lock"); e != nil {
			return nil, recovery(e)
		}
		if e = lock.Sync(); e != nil {
			return nil, recovery(e)
		}
		if e = x.sync(held, "lock-entry"); e != nil {
			return nil, recovery(e)
		}
		b, e := canonjson.MarshalFile(storeIdentity())
		if e != nil {
			return nil, e
		}
		if e = x.write(held, "store-pending", b, 0600); e != nil {
			return nil, recovery(e)
		}
		if e = x.rename(held, "store-pending", "store.json"); e != nil {
			return nil, recovery(e)
		}
		if e = x.sync(held, "identity"); e != nil {
			return nil, recovery(e)
		}
		for _, d := range []string{"generations", "incomplete"} {
			if e = x.mkdir(held, d); e != nil {
				return nil, recovery(e)
			}
			fd, e := relative(held, d, true)
			if e != nil {
				return nil, recovery(e)
			}
			e = x.sync(fd, d)
			syscall.Close(fd)
			if e != nil {
				return nil, recovery(e)
			}
		}
		if e = x.sync(held, "namespace"); e != nil {
			return nil, recovery(e)
		}
	}
	if _, e = t.validate(); e != nil {
		return nil, recovery(e)
	}
	if !creator {
		// Reconfirm durability when reopening after a possible interrupted commit.
		if e = lock.Sync(); e != nil {
			return nil, recovery(e)
		}
		for _, d := range []string{"generations", "incomplete"} {
			fd, e := relative(held, d, true)
			if e != nil {
				return nil, recovery(e)
			}
			e = x.sync(fd, "reopen-"+d)
			syscall.Close(fd)
			if e != nil {
				return nil, recovery(e)
			}
		}
		if e = x.sync(held, "reopen-root"); e != nil {
			return nil, recovery(e)
		}
		if _, e = t.validate(); e != nil {
			return nil, recovery(e)
		}
	}
	return s, nil
}
func (s *Store) begin() (*transaction, error) {
	x, e := newFilesystem(s.h)
	if e != nil {
		return nil, e
	}
	fd, e := absolute(s.root, true)
	if e != nil {
		x.close()
		return nil, e
	}
	st, e := stat(fd)
	if e != nil || !sameInode(st, s.rootStat) {
		syscall.Close(fd)
		x.close()
		return nil, recovery(errors.New("store root replaced"))
	}
	lf, e := relative(fd, "lock", false)
	if e != nil {
		syscall.Close(fd)
		x.close()
		return nil, recovery(e)
	}
	ls, e := stat(lf)
	if e != nil || !sameInode(ls, s.lockStat) || owned(ls, 0600, false) != nil || ls.Size != 0 {
		syscall.Close(lf)
		syscall.Close(fd)
		x.close()
		return nil, recovery(errors.New("lock replaced"))
	}
	lock, e := x.bridge(lf, syscall.O_RDWR)
	syscall.Close(lf)
	if e != nil {
		syscall.Close(fd)
		x.close()
		return nil, e
	}
	t := &transaction{s, x, fd, lock, map[string]syscall.Stat_t{}}
	if e = x.point("before-flock"); e != nil {
		t.close()
		return nil, recovery(e)
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		t.close()
		return nil, e
	}
	if e = t.bind(); e != nil {
		t.close()
		return nil, recovery(e)
	}
	return t, nil
}
func (t *transaction) close() {
	syscall.Flock(int(t.lock.Fd()), syscall.LOCK_UN)
	t.lock.Close()
	syscall.Close(t.root)
	t.x.close()
}
func (t *transaction) bind() error {
	if e := physicalBind(t.s.root, t.root); e != nil {
		return e
	}
	rs, e := stat(t.root)
	if e != nil {
		return e
	}
	if e = owned(rs, 0700, true); e != nil {
		return e
	}
	fd, e := relative(t.root, "lock", false)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	ls, e := stat(fd)
	held, e2 := stat(int(t.lock.Fd()))
	if e != nil || e2 != nil || !same(ls, held) || !sameInode(ls, t.s.lockStat) || ls.Size != 0 || owned(ls, 0600, false) != nil {
		return errors.New("lock binding")
	}
	for p, old := range t.dirs {
		d, e := relative(t.root, p, true)
		if e != nil {
			return e
		}
		now, e := stat(d)
		syscall.Close(d)
		if e != nil || !sameInode(old, now) || owned(now, 0700, true) != nil {
			return errors.New("namespace binding")
		}
	}
	return nil
}
func (t *transaction) generation(path, id string) (Inspection, error) {
	fd, e := relative(t.root, path, true)
	if e != nil {
		return Inspection{}, e
	}
	defer syscall.Close(fd)
	b, bs, e := t.x.read(fd, "receipt.json", metadataLimit, 0600)
	if e != nil {
		return Inspection{}, e
	}
	var r Receipt
	if e = strictJSON(b, &r, true); e != nil {
		return Inspection{}, e
	}
	if e = validateReceipt(r); e != nil {
		return Inspection{}, e
	}
	if digest(b) != id {
		return Inspection{}, errors.New("generation id")
	}
	expected := map[string]uint32{"receipt.json": 0600}
	for _, f := range r.Files {
		expected[f.Path] = uint32(f.Mode)
	}
	data, e := t.x.tree(fd, expected)
	if e != nil {
		return Inspection{}, e
	}
	end, es, e := t.x.read(fd, "receipt.json", metadataLimit, 0600)
	if e != nil || !same(bs, es) || !bytes.Equal(end, b) || !bytes.Equal(data["receipt.json"], b) {
		return Inspection{}, errors.New("receipt changed")
	}
	for _, f := range r.Files {
		if digest(data[f.Path]) != f.SHA256 {
			return Inspection{}, errors.New("payload hash")
		}
		if f.Kind == "policy-input" {
			if e = validatePolicyBytes(data[f.Path]); e != nil {
				return Inspection{}, e
			}
		}
	}
	again, e := relative(t.root, path, true)
	if e != nil {
		return Inspection{}, e
	}
	defer syscall.Close(again)
	before, e := stat(fd)
	now, e2 := stat(again)
	if e != nil || e2 != nil || !same(before, now) {
		return Inspection{}, errors.New("generation binding")
	}
	label := "trusted-generated-source"
	if r.SchemaVersion == 2 {
		label += "; derivation-only"
	}
	return Inspection{id, &r, label}, nil
}
func (t *transaction) current() (string, error) {
	target, e := readlinkat(t.root, "candidate-current")
	if errors.Is(e, syscall.ENOENT) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	if !strings.HasPrefix(target, "generations/") || !pin(strings.TrimPrefix(target, "generations/"), 64) {
		return "", errors.New("candidate selector")
	}
	h, e := syscall.Openat(t.root, "candidate-current", oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return "", e
	}
	defer syscall.Close(h)
	st, e := stat(h)
	if e != nil || st.Mode&syscall.S_IFMT != syscall.S_IFLNK || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
		return "", errors.New("selector type or ownership")
	}
	end, e := readlinkat(t.root, "candidate-current")
	if e != nil || end != target {
		return "", errors.New("selector binding")
	}
	return strings.TrimPrefix(target, "generations/"), nil
}
func (t *transaction) validate() (Inspection, error) {
	if e := t.bind(); e != nil {
		return Inspection{}, e
	}
	names, e := t.x.names(t.root)
	if e != nil {
		return Inspection{}, e
	}
	required := map[string]bool{"lock": false, "store.json": false, "generations": false, "incomplete": false}
	for _, n := range names {
		if _, ok := required[n]; ok {
			required[n] = true
		} else if n != "candidate-current" {
			return Inspection{}, fmt.Errorf("unowned store entry %s", n)
		}
	}
	for n, ok := range required {
		if !ok {
			return Inspection{}, fmt.Errorf("initialization-incomplete: missing %s", n)
		}
	}
	b, bs, e := t.x.read(t.root, "store.json", metadataLimit, 0600)
	if e != nil {
		return Inspection{}, e
	}
	var ident identity
	if e = strictJSON(b, &ident, true); e != nil {
		return Inspection{}, e
	}
	if ident != storeIdentity() {
		return Inspection{}, errors.New("store identity")
	}
	for _, d := range []string{"generations", "incomplete"} {
		fd, e := relative(t.root, d, true)
		if e != nil {
			return Inspection{}, e
		}
		st, e := stat(fd)
		if e == nil {
			e = owned(st, 0700, true)
		}
		if e != nil {
			syscall.Close(fd)
			return Inspection{}, e
		}
		if old, ok := t.dirs[d]; ok && !sameInode(old, st) {
			syscall.Close(fd)
			return Inspection{}, errors.New("directory substituted")
		}
		t.dirs[d] = st
		children, e := t.x.names(fd)
		syscall.Close(fd)
		if e != nil {
			return Inspection{}, e
		}
		sort.Strings(children)
		for _, id := range children {
			if d == "incomplete" {
				if !pin(id, 32) {
					return Inspection{}, errors.New("unowned incomplete")
				}
				// Validate even retained complete stages, but never adopt or remove them.
				h, e := relative(t.root, d+"/"+id, true)
				if e != nil {
					return Inspection{}, e
				}
				rb, _, re := t.x.read(h, "receipt.json", metadataLimit, 0600)
				syscall.Close(h)
				if re != nil {
					return Inspection{}, fmt.Errorf("incomplete evidence: %w", re)
				}
				if _, e = t.generation(d+"/"+id, digest(rb)); e != nil {
					return Inspection{}, e
				}
				return Inspection{}, errors.New("preserved incomplete stage requires decision")
			}
			if !pin(id, 64) {
				return Inspection{}, errors.New("unowned generation")
			}
			if _, e = t.generation(d+"/"+id, id); e != nil {
				return Inspection{}, e
			}
		}
	}
	id, e := t.current()
	if e != nil {
		return Inspection{}, e
	}
	var observed Inspection
	if id != "" {
		observed, e = t.generation("generations/"+id, id)
		if e != nil {
			return Inspection{}, e
		}
	}
	end, e := t.current()
	if e != nil || end != id {
		return Inspection{}, errors.New("selector substituted")
	}
	final, fs, e := t.x.read(t.root, "store.json", metadataLimit, 0600)
	if e != nil || !same(bs, fs) || !bytes.Equal(b, final) {
		return Inspection{}, errors.New("store metadata changed")
	}
	if e = t.bind(); e != nil {
		return Inspection{}, e
	}
	return observed, nil
}
func (s *Store) Inspect() (Inspection, error) {
	t, e := s.begin()
	if e != nil {
		return Inspection{}, e
	}
	defer t.close()
	i, e := t.validate()
	if e != nil {
		return Inspection{}, recovery(e)
	}
	return i, nil
}

// Recover is observation and durability confirmation, never cleanup or fallback.
func (s *Store) Recover() (Inspection, error) {
	t, e := s.begin()
	if e != nil {
		return Inspection{}, e
	}
	defer t.close()
	i, e := t.validate()
	if e != nil {
		return Inspection{}, recovery(e)
	}
	for _, p := range []string{"incomplete", "generations"} {
		d, e := relative(t.root, p, true)
		if e != nil {
			return Inspection{}, recovery(e)
		}
		e = t.x.syncTree(d, p)
		syscall.Close(d)
		if e != nil {
			return Inspection{}, recovery(e)
		}
	}
	if e = t.x.sync(t.root, "recovery"); e != nil {
		return Inspection{}, recovery(e)
	}
	final, e := t.validate()
	if e != nil || final.GenerationID != i.GenerationID {
		return Inspection{}, recovery(errors.New("recovery binding"))
	}
	return final, nil
}
func (t *transaction) selectID(id string) (Inspection, error) {
	if !pin(id, 64) {
		return Inspection{}, errors.New("exact generation id required")
	}
	verified, e := t.generation("generations/"+id, id)
	if e != nil {
		return Inspection{}, e
	}
	cur, e := t.current()
	if e != nil {
		return Inspection{}, e
	}
	// A prior call may have returned uncertainty after a rename. Reconfirm both
	// publication parents even when using an already complete generation.
	for _, p := range []string{"incomplete", "generations"} {
		d, e := relative(t.root, p, true)
		if e != nil {
			return Inspection{}, recovery(e)
		}
		e = t.x.sync(d, "selection-"+p)
		syscall.Close(d)
		if e != nil {
			return Inspection{}, recovery(e)
		}
	}
	if cur == id {
		if e = t.x.sync(t.root, "selection-idempotent"); e != nil {
			return Inspection{}, recovery(e)
		}
		result, e := t.validate()
		if e != nil {
			return Inspection{}, recovery(e)
		}
		return result, nil
	}
	n, e := randomName()
	if e != nil {
		return Inspection{}, e
	}
	tmp := "selector-" + n
	if e = t.x.point("symlink:selector"); e != nil {
		return Inspection{}, e
	}
	if e = symlinkat("generations/"+id, t.root, tmp); e != nil {
		return Inspection{}, e
	}
	if e = t.bind(); e != nil {
		return Inspection{}, recovery(e)
	}
	if e = t.x.rename(t.root, tmp, "candidate-current"); e != nil {
		return Inspection{}, recovery(e)
	}
	if e = t.x.sync(t.root, "selection"); e != nil {
		return Inspection{}, recovery(e)
	}
	final, e := t.validate()
	if e != nil || final.GenerationID != verified.GenerationID {
		return Inspection{}, recovery(errors.New("postcommit verification"))
	}
	return final, nil
}
func (s *Store) SelectPrior(id string) (Inspection, error) {
	t, e := s.begin()
	if e != nil {
		return Inspection{}, e
	}
	defer t.close()
	if _, e = t.validate(); e != nil {
		return Inspection{}, recovery(e)
	}
	return t.selectID(id)
}
func (s *Store) Stage(repository, commit string) (Inspection, error) {
	return s.stage(repository, commit, nil)
}
func (s *Store) StageWithPolicy(options author.SharedPolicyOptions) (Inspection, error) {
	return s.stage(options.Repository, options.Commit, &options)
}
func (s *Store) stage(repository, commit string, options *author.SharedPolicyOptions) (Inspection, error) {
	t, e := s.begin()
	if e != nil {
		return Inspection{}, e
	}
	defer t.close()
	if _, e = t.validate(); e != nil {
		return Inspection{}, recovery(e)
	}
	repo, e := absolute(repository, true)
	if e != nil {
		return Inspection{}, e
	}
	defer syscall.Close(repo)
	if e = physicalBind(repository, repo); e != nil {
		return Inspection{}, e
	}
	var p *author.SharedPreparation
	if options == nil {
		p, e = author.PrepareShared(repository, commit)
	} else {
		p, e = author.PrepareSharedWithPolicy(*options)
	}
	if e != nil {
		return Inspection{}, e
	}
	defer p.Close()
	if s.h.prepared != nil {
		s.h.prepared()
	}
	r, payload, e := observePreparation(t.x, p)
	if e != nil {
		return Inspection{}, e
	}
	if e = physicalBind(repository, repo); e != nil {
		return Inspection{}, e
	}
	rb, e := canonjson.MarshalFile(r)
	if e != nil {
		return Inspection{}, e
	}
	if e = validateReceipt(r); e != nil {
		return Inspection{}, e
	}
	var decoded Receipt
	if e = strictJSON(rb, &decoded, true); e != nil {
		return Inspection{}, e
	}
	id := digest(rb)
	// Never overwrite an existing generation, even if the current selector differs.
	existing, e := relative(t.root, "generations/"+id, true)
	if e == nil {
		syscall.Close(existing)
		if _, e = t.generation("generations/"+id, id); e != nil {
			return Inspection{}, recovery(e)
		}
		return t.selectID(id)
	}
	if !errors.Is(e, syscall.ENOENT) {
		return Inspection{}, recovery(e)
	}
	n, e := randomName()
	if e != nil {
		return Inspection{}, e
	}
	stage := "incomplete/" + n
	if e = t.x.mkdir(t.root, stage); e != nil {
		return Inspection{}, e
	}
	fail := func(e error) (Inspection, error) { return Inspection{}, recovery(e) }
	sd, e := relative(t.root, stage, true)
	if e != nil {
		return fail(e)
	}
	defer syscall.Close(sd)
	dirs := map[string]bool{}
	for path := range payload {
		for d := filepath.Dir(path); d != "."; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	ordered := sortedKeys(dirs)
	for _, d := range ordered {
		if e = t.x.mkdir(sd, d); e != nil {
			return fail(e)
		}
	}
	modes := map[string]uint32{}
	for _, f := range r.Files {
		modes[f.Path] = uint32(f.Mode)
	}
	for _, path := range sortedKeys(payload) {
		if e = t.x.write(sd, path, payload[path], modes[path]); e != nil {
			return fail(e)
		}
	}
	if e = t.x.write(sd, "receipt.json", rb, 0600); e != nil {
		return fail(e)
	}
	if e = t.x.syncTree(sd, stage); e != nil {
		return fail(e)
	}
	incomplete, e := relative(t.root, "incomplete", true)
	if e != nil {
		return fail(e)
	}
	defer syscall.Close(incomplete)
	if e = t.x.sync(incomplete, "stage-entry"); e != nil {
		return fail(e)
	}
	if _, e = t.generation(stage, id); e != nil {
		return fail(e)
	}
	// Independently observe the still-open prepared source again after copying.
	again, other, e := observePreparation(t.x, p)
	if e != nil || !reflect.DeepEqual(r, again) || !reflect.DeepEqual(payload, other) {
		return fail(errors.New("prepared input changed"))
	}
	if e = t.bind(); e != nil {
		return fail(e)
	}
	if e = t.x.rename(t.root, stage, "generations/"+id); e != nil {
		return fail(e)
	}
	// Cross-directory rename needs durability in both parent directories.
	if e = t.x.sync(incomplete, "incomplete-finalization"); e != nil {
		return fail(e)
	}
	generations, e := relative(t.root, "generations", true)
	if e != nil {
		return fail(e)
	}
	defer syscall.Close(generations)
	if e = t.x.sync(generations, "generations-finalization"); e != nil {
		return fail(e)
	}
	if _, e = t.generation("generations/"+id, id); e != nil {
		return fail(e)
	}
	return t.selectID(id)
}
