//go:build linux

package sharedgeneration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

func command(t *testing.T, root string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", root}, args...)...)
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}
func fixture(t *testing.T, variant string) (string, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join("..", "..", author.SharedDirectory)
	e := filepath.WalkDir(source, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(source, path)
		if e != nil {
			return e
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		out := filepath.Join(root, author.SharedDirectory, rel)
		if e = os.MkdirAll(filepath.Dir(out), 0755); e != nil {
			return e
		}
		if rel == "config/claude/CLAUDE.md" {
			b = append(b, []byte("\n"+variant+"\n")...)
		}
		return os.WriteFile(out, b, st.Mode().Perm())
	})
	if e != nil {
		t.Fatal(e)
	}
	command(t, root, "init", "-q")
	command(t, root, "config", "user.name", "fixture")
	command(t, root, "config", "user.email", "fixture@example.invalid")
	command(t, root, "remote", "add", "origin", "https://github.com/iryzhkov/UpKeeper.git")
	command(t, root, "add", ".")
	command(t, root, "commit", "-qm", "approved fixture")
	return root, command(t, root, "rev-parse", "HEAD")
}
func scratch(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	if e := os.Chmod(p, 0700); e != nil {
		t.Fatal(e)
	}
	return p
}
func mustOpen(t *testing.T) *Store {
	t.Helper()
	s, e := Open(scratch(t))
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func mustStage(t *testing.T, s *Store, repo, commit string) Inspection {
	t.Helper()
	i, e := s.Stage(repo, commit)
	if e != nil {
		t.Fatal(e)
	}
	if i.TrustLabel != "trusted-generated-source" || !pin(i.GenerationID, 64) {
		t.Fatal("missing inert identity")
	}
	return i
}
func lockInode(t *testing.T, p string) syscall.Stat_t {
	t.Helper()
	var st syscall.Stat_t
	if e := syscall.Lstat(filepath.Join(p, "lock"), &st); e != nil {
		t.Fatal(e)
	}
	return st
}
func TestCompleteDeterministicRollbackAndNoImports(t *testing.T) {
	repo, a := fixture(t, "first approved source")
	other, b := fixture(t, "second approved source")
	home := scratch(t)
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	s := mustOpen(t)
	first := mustStage(t, s, repo, a)
	inode := lockInode(t, s.root)
	s.h.fault = func(p string) error {
		if strings.HasPrefix(p, "write:") {
			return errors.New("unexpected recopy")
		}
		return nil
	}
	repeated, e := s.Stage(repo, a)
	if e != nil || !reflect.DeepEqual(repeated, first) {
		t.Fatalf("idempotence: %v", e)
	}
	s.h = hooks{}
	separate := mustOpen(t)
	same := mustStage(t, separate, repo, a)
	one, _ := os.ReadFile(filepath.Join(s.root, "generations", first.GenerationID, "receipt.json"))
	two, _ := os.ReadFile(filepath.Join(separate.root, "generations", same.GenerationID, "receipt.json"))
	if !bytes.Equal(one, two) || first.GenerationID != same.GenerationID {
		t.Fatal("nondeterministic receipt")
	}
	second := mustStage(t, s, other, b)
	if second.GenerationID == first.GenerationID {
		t.Fatal("source identities collapsed")
	}
	restored, e := s.SelectPrior(first.GenerationID)
	if e != nil || !reflect.DeepEqual(restored, first) {
		t.Fatalf("rollback: %v", e)
	}
	if _, e = s.generationForTest(second.GenerationID); e != nil {
		t.Fatal("rollback lost second generation", e)
	}
	if !sameInode(inode, lockInode(t, s.root)) {
		t.Fatal("lock replaced")
	}
	entries, e := os.ReadDir(home)
	if e != nil || len(entries) != 0 {
		t.Fatal("HOME/import mutation")
	}
	if i, e := s.Recover(); e != nil || i.GenerationID != first.GenerationID {
		t.Fatalf("recovery %v", e)
	}
}
func (s *Store) generationForTest(id string) (Inspection, error) {
	t, e := s.begin()
	if e != nil {
		return Inspection{}, e
	}
	defer t.close()
	return t.generation("generations/"+id, id)
}
func TestSourceRefusalAndObservedClosure(t *testing.T) {
	repo, commit := fixture(t, "approved")
	s := mustOpen(t)
	for _, ref := range []string{"HEAD", commit[:12], strings.Repeat("0", 40)} {
		if _, e := s.Stage(repo, ref); e == nil {
			t.Fatal("accepted floating/missing ref")
		}
	}
	link := filepath.Join(scratch(t), "repository")
	if e := os.Symlink(repo, link); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Stage(link, commit); e == nil {
		t.Fatal("source parent symlink")
	}
	command(t, repo, "remote", "set-url", "origin", "https://example.invalid/UpKeeper.git")
	if _, e := s.Stage(repo, commit); e == nil {
		t.Fatal("refused source accepted")
	}
	command(t, repo, "remote", "set-url", "origin", "https://github.com/iryzhkov/UpKeeper.git")
	x, e := newFilesystem(hooks{})
	if e != nil {
		t.Fatal(e)
	}
	defer x.close()
	tests := []string{"closed", "modified", "hash", "owner", "output", "instruction-mode", "source-mode", "symlink", "fifo", "extra", "duplicate", "path"}
	for _, kind := range tests {
		t.Run(kind, func(t *testing.T) {
			p, e := author.PrepareShared(repo, commit)
			if e != nil {
				t.Fatal(e)
			}
			defer p.Close()
			src := filepath.Dir(filepath.Dir(filepath.Dir(p.Sources.SkillsRoot)))
			skill := filepath.Join(p.Sources.SkillsRoot, "huyang/SKILL.md")
			switch kind {
			case "closed":
				p.Close()
			case "modified":
				os.WriteFile(skill, []byte("modified"), 0644)
			case "hash":
				p.SourceSHA256["owner.json"] = strings.Repeat("0", 64)
			case "owner":
				p.Owner.Owner = "unknown"
			case "output":
				p.OutputSHA256[p.Owner.InstructionFiles[0]] = strings.Repeat("0", 64)
			case "instruction-mode":
				f := p.Sources.Instructions[p.Owner.InstructionFiles[0]]
				f.Permissions = 0600
				p.Sources.Instructions[p.Owner.InstructionFiles[0]] = f
			case "source-mode":
				os.Chmod(skill, 0600)
			case "symlink":
				os.Remove(skill)
				os.Symlink("/dev/null", skill)
			case "fifo":
				os.Remove(skill)
				if e := syscall.Mkfifo(skill, 0644); e != nil {
					t.Fatal(e)
				}
			case "extra":
				os.WriteFile(filepath.Join(src, "extra"), []byte("extra"), 0644)
			case "duplicate":
				p.Owner.SourceFiles = append(p.Owner.SourceFiles, p.Owner.SourceFiles[0])
			case "path":
				p.Owner.InstructionFiles[0] = "../escape"
			}
			if _, _, e := observePreparation(x, p); e == nil {
				t.Fatal("accepted changed preparation")
			}
		})
	}
	// A poisoned working tree is not a source; the exact Git object remains stable.
	os.WriteFile(filepath.Join(repo, author.SharedDirectory, "config/claude/CLAUDE.md"), []byte("working poison"), 0644)
	i := mustStage(t, s, repo, commit)
	for _, f := range i.Receipt.Files {
		b, e := os.ReadFile(filepath.Join(s.root, "generations", i.GenerationID, f.Path))
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(b, []byte("working poison")) {
			t.Fatal("floating capture")
		}
	}
}
func TestBoundedPhysicalAcquisition(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "hardlink", "size", "mode", "substitute", "grow", "procfs", "device"} {
		t.Run(kind, func(t *testing.T) {
			root := scratch(t)
			p := filepath.Join(root, "file")
			if e := os.WriteFile(p, []byte("original"), 0644); e != nil {
				t.Fatal(e)
			}
			opened := false
			h := hooks{dataOpen: func(string) { opened = true }}
			switch kind {
			case "symlink":
				os.Remove(p)
				os.Symlink("/dev/null", p)
			case "fifo":
				os.Remove(p)
				if e := syscall.Mkfifo(p, 0644); e != nil {
					t.Fatal(e)
				}
			case "directory":
				os.Remove(p)
				os.Mkdir(p, 0700)
			case "hardlink":
				if e := os.Link(p, filepath.Join(root, "alias")); e != nil {
					t.Fatal(e)
				}
			case "size":
				if e := os.Truncate(p, fileLimit+1); e != nil {
					t.Fatal(e)
				}
			case "mode":
				os.Chmod(p, 0600)
			case "substitute":
				h.afterCapture = func(string) { os.Rename(p, p+".old"); os.WriteFile(p, []byte("original"), 0644) }
			case "grow":
				h.afterRead = func(string) { os.WriteFile(p, []byte("growth"), 0644) }
			case "procfs":
				h.procPath = root
			case "device":
				os.Remove(p)
				if e := syscall.Mknod(p, syscall.S_IFCHR|0644, 0x103); e != nil {
					t.Skipf("device fixture: CAP_MKNOD unavailable: %v", e)
				}
			}
			x, e := newFilesystem(h)
			if kind == "procfs" {
				if e == nil {
					x.close()
					t.Fatal("unvalidated procfs")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			defer x.close()
			fd, e := absolute(root, true)
			if e != nil {
				t.Fatal(e)
			}
			defer syscall.Close(fd)
			if _, _, e = x.read(fd, "file", fileLimit, 0644); e == nil {
				t.Fatal("unsafe input accepted")
			}
			if kind != "substitute" && kind != "grow" && opened {
				t.Fatal("nonregular/invalid inode data-opened")
			}
		})
	}
	for _, p := range []string{"../x", "a/../x", "a//b", "a\\b", "/a", "a/./b", "a\n", strings.Repeat("a", 256), strings.Repeat("a/", 256) + "b"} {
		if validPath(p) {
			t.Fatalf("accepted path %q", p)
		}
	}
	// Parent substitution/symlinks are rejected before any data open.
	root := scratch(t)
	os.Mkdir(filepath.Join(root, "real"), 0700)
	os.Symlink("real", filepath.Join(root, "parent"))
	if fd, e := absolute(filepath.Join(root, "parent"), true); e == nil {
		syscall.Close(fd)
		t.Fatal("parent symlink")
	}
}
func TestStoreAndGenerationCorruption(t *testing.T) {
	repo, commit := fixture(t, "approved")
	for _, kind := range []string{"identity", "identity-symlink", "identity-fifo", "extra-root", "root-mode", "lock-symlink", "lock-hardlink", "lock-replaced", "receipt", "receipt-symlink", "payload", "payload-mode", "payload-fifo", "payload-symlink", "payload-extra", "dir-symlink", "current-absolute", "current-escape", "current-file", "current-missing", "current-fifo"} {
		t.Run(kind, func(t *testing.T) {
			s := mustOpen(t)
			i := mustStage(t, s, repo, commit)
			gen := filepath.Join(s.root, "generations", i.GenerationID)
			receipt := filepath.Join(gen, "receipt.json")
			payload := filepath.Join(gen, i.Receipt.Files[0].Path)
			identity := filepath.Join(s.root, "store.json")
			lock := filepath.Join(s.root, "lock")
			current := filepath.Join(s.root, "candidate-current")
			switch kind {
			case "identity":
				os.WriteFile(identity, []byte("{}\n"), 0600)
			case "identity-symlink":
				os.Remove(identity)
				os.Symlink(receipt, identity)
			case "identity-fifo":
				os.Remove(identity)
				syscall.Mkfifo(identity, 0600)
			case "extra-root":
				os.WriteFile(filepath.Join(s.root, "unowned"), []byte("preserve"), 0600)
			case "root-mode":
				os.Chmod(s.root, 0755)
			case "lock-symlink":
				os.Rename(lock, lock+".saved")
				os.Symlink("lock.saved", lock)
			case "lock-hardlink":
				os.Link(lock, lock+".alias")
			case "lock-replaced":
				os.Rename(lock, lock+".saved")
				os.WriteFile(lock, nil, 0600)
			case "receipt":
				os.WriteFile(receipt, []byte("{}\n"), 0600)
			case "receipt-symlink":
				os.Remove(receipt)
				os.Symlink(identity, receipt)
			case "payload":
				os.WriteFile(payload, []byte("corrupt"), 0644)
			case "payload-mode":
				os.Chmod(payload, 0600)
			case "payload-fifo":
				os.Remove(payload)
				syscall.Mkfifo(payload, 0644)
			case "payload-symlink":
				os.Remove(payload)
				os.Symlink("/dev/null", payload)
			case "payload-extra":
				os.WriteFile(filepath.Join(gen, "extra"), []byte("preserve"), 0644)
			case "dir-symlink":
				os.Rename(filepath.Join(gen, "skills"), filepath.Join(gen, "saved-skills"))
				os.Symlink("saved-skills", filepath.Join(gen, "skills"))
			case "current-absolute":
				os.Remove(current)
				os.Symlink(gen, current)
			case "current-escape":
				os.Remove(current)
				os.Symlink("../generations/"+i.GenerationID, current)
			case "current-file":
				os.Remove(current)
				os.WriteFile(current, []byte("generations/"+i.GenerationID), 0600)
			case "current-fifo":
				os.Remove(current)
				syscall.Mkfifo(current, 0600)
			case "current-missing":
				os.Remove(current)
				os.Symlink("generations/"+strings.Repeat("0", 64), current)
			}
			if _, e := s.Inspect(); e == nil {
				t.Fatal("corruption accepted")
			}
			if _, e := s.SelectPrior(i.GenerationID); e == nil {
				t.Fatal("rollback accepted invalid store")
			}
			if _, e := s.Recover(); e == nil {
				t.Fatal("corruption recovered by fallback")
			}
			if kind == "extra-root" {
				b, e := os.ReadFile(filepath.Join(s.root, "unowned"))
				if e != nil || string(b) != "preserve" {
					t.Fatal("evidence deleted")
				}
			}
		})
	}
}
func TestStrictSchemaAndCanonicalID(t *testing.T) {
	repo, commit := fixture(t, "approved")
	s := mustOpen(t)
	i := mustStage(t, s, repo, commit)
	b, e := canonjson.MarshalFile(i.Receipt)
	if e != nil {
		t.Fatal(e)
	}
	if digest(b) != i.GenerationID {
		t.Fatal("ID not external receipt digest")
	}
	cases := [][]byte{
		[]byte("{\"schema_version\":1,\"schema_version\":1}"),
		[]byte(strings.Repeat("[", 13) + "0" + strings.Repeat("]", 13)),
		[]byte("[" + strings.Repeat("0,", 1024) + "0]"),
		[]byte(strings.Repeat(" ", metadataLimit+1)),
		append(append([]byte(nil), b...), []byte("{}")...),
		bytes.Replace(b, []byte("\"schema_version\": 1"), []byte("\"schema_version\": \"1\""), 1),
		bytes.Replace(b, []byte("\"owner\": \"upkeeper\""), []byte("\"owner\": \"upkeeper\", \"unknown\": true"), 1),
	}
	for _, bad := range cases {
		var r Receipt
		if e := strictJSON(bad, &r, true); e == nil {
			t.Fatal("invalid schema accepted")
		}
	}
	for _, mutate := range []func(*Receipt){
		func(r *Receipt) { r.Files = append(r.Files, r.Files[0]) },
		func(r *Receipt) { r.Files[0].Path = "../escape" },
		func(r *Receipt) { r.Files[0].Mode = 0600 },
		func(r *Receipt) { r.Files[0].Kind = "active" },
		func(r *Receipt) { r.SourceCommit = "HEAD" },
		func(r *Receipt) { r.SourceSHA256["owner.json"] = strings.Repeat("0", 64) },
		func(r *Receipt) { r.Files = r.Files[:1] },
		func(r *Receipt) { r.SourceSHA256["config/claude/skills/extra/SKILL.md"] = strings.Repeat("0", 64) },
	} {
		var r Receipt
		if e := json.Unmarshal(b, &r); e != nil {
			t.Fatal(e)
		}
		mutate(&r)
		if e := validateReceipt(r); e == nil {
			t.Fatal("invalid receipt accepted")
		}
	}
}
func TestInitializationFaultsAndNoAdoption(t *testing.T) {
	for _, kind := range []string{"nonempty", "lock-only", "mode", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := scratch(t)
			switch kind {
			case "nonempty":
				os.WriteFile(filepath.Join(root, "unowned"), []byte("keep"), 0600)
			case "lock-only":
				os.WriteFile(filepath.Join(root, "lock"), nil, 0600)
			case "mode":
				os.Chmod(root, 0755)
			case "symlink":
				link := filepath.Join(scratch(t), "root")
				os.Symlink(root, link)
				root = link
			}
			if _, e := Open(root); e == nil {
				t.Fatal("unowned adoption")
			}
			if kind == "nonempty" {
				b, e := os.ReadFile(filepath.Join(root, "unowned"))
				if e != nil || string(b) != "keep" {
					t.Fatal("unowned content lost")
				}
			}
		})
	}
	events := []string{}
	if _, e := openStore(scratch(t), hooks{event: func(s string) { events = append(events, s) }}); e != nil {
		t.Fatal(e)
	}
	for _, boundary := range events {
		t.Run(boundary, func(t *testing.T) {
			root := scratch(t)
			hit := false
			_, e := openStore(root, hooks{fault: func(s string) error {
				if s == boundary {
					hit = true
					return errors.New("injected initialization crash")
				}
				return nil
			}})
			if !hit || e == nil {
				t.Fatal("false initialization success")
			}
			old := lockInode(t, root)
			next, e := Open(root)
			if next != nil && e != nil {
				t.Fatal("invalid open result")
			}
			if e == nil {
				if _, e = next.Recover(); e != nil {
					t.Fatal(e)
				}
			}
			if !sameInode(old, lockInode(t, root)) {
				t.Fatal("lock replaced after interrupted initialization")
			}
		})
	}
}
func normalize(s string) string {
	for _, prefix := range []string{"incomplete/", "selector-"} {
		for {
			idx := strings.Index(s, prefix)
			if idx < 0 {
				break
			}
			start := idx + len(prefix)
			if len(s) < start+32 {
				break
			}
			if !pin(s[start:start+32], 32) {
				break
			}
			s = s[:start] + "STAGE" + s[start+32:]
		}
	}
	return s
}
func TestEveryWriteSyncRenameFaultAndRestart(t *testing.T) {
	repo, a := fixture(t, "first")
	other, b := fixture(t, "second")
	baseline := mustOpen(t)
	mustStage(t, baseline, repo, a)
	events := []string{}
	baseline.h.event = func(p string) { events = append(events, normalize(p)) }
	mustStage(t, baseline, other, b)
	for _, boundary := range events {
		t.Run(boundary, func(t *testing.T) {
			s := mustOpen(t)
			old := mustStage(t, s, repo, a)
			before := lockInode(t, s.root)
			hit := false
			s.h.fault = func(p string) error {
				if normalize(p) == boundary && !hit {
					hit = true
					return errors.New("injected I/O failure")
				}
				return nil
			}
			got, e := s.Stage(other, b)
			if !hit || e == nil || got.GenerationID != "" {
				t.Fatalf("false durable success at %s: %v", boundary, e)
			}
			target, re := os.Readlink(filepath.Join(s.root, "candidate-current"))
			if re != nil {
				t.Fatal(re)
			}
			if !strings.HasSuffix(boundary, "selection") && !strings.Contains(boundary, "->candidate-current") && target != "generations/"+old.GenerationID {
				t.Fatal("precommit selection changed")
			}
			// Reopen either verifies durable old/new or refuses retained evidence. No GC.
			reopened, e := Open(s.root)
			if e == nil {
				observed, e := reopened.Recover()
				if e != nil {
					t.Fatal(e)
				}
				if observed.GenerationID == "" {
					t.Fatal("selection lost")
				}
			}
			if !sameInode(before, lockInode(t, s.root)) {
				t.Fatal("persistent lock changed")
			}
		})
	}
}
func child(t *testing.T, root, repo, commit, action, boundary string) *exec.Cmd {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$", "-test.v")
	c.Env = append(os.Environ(), "SG_CHILD=1", "SG_ROOT="+root, "SG_REPO="+repo, "SG_COMMIT="+commit, "SG_ACTION="+action, "SG_BOUNDARY="+boundary)
	return c
}
func TestProcessHelper(t *testing.T) {
	if os.Getenv("SG_CHILD") != "1" {
		return
	}
	root, repo, commit := os.Getenv("SG_ROOT"), os.Getenv("SG_REPO"), os.Getenv("SG_COMMIT")
	boundary := os.Getenv("SG_BOUNDARY")
	action := os.Getenv("SG_ACTION")
	h := hooks{}
	if action == "early" || action == "revalidate" {
		h.event = func(p string) {
			if action == "early" && p == "lock-created" || action == "revalidate" && p == "before-flock" {
				fmt.Println("CREATOR")
				var b [1]byte
				if _, e := os.Stdin.Read(b[:]); e != nil {
					os.Exit(9)
				}
			}
		}
	}
	if action == "crash-init" || action == "crash-stage" {
		h.event = func(p string) {
			if normalize(p) == boundary {
				os.Exit(42)
			}
		}
	}
	s, e := openStore(root, h)
	if action == "contender" || action == "revalidate" {
		if !errors.Is(e, ErrRecoveryRequired) {
			t.Fatalf("early contender: %v", e)
		}
		return
	}
	if e != nil {
		t.Fatal(e)
	}
	if action == "stage" || action == "crash-stage" {
		for n := 0; n < 3; n++ {
			i, e := s.Stage(repo, commit)
			if e != nil {
				t.Fatal(e)
			}
			fmt.Println("ID", i.GenerationID)
		}
	}
}
func TestIndependentProcessElectionEarlyContender(t *testing.T) {
	root := scratch(t)
	creator := child(t, root, "", "", "early", "")
	stdin, e := creator.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	stdout, e := creator.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	var stderr bytes.Buffer
	creator.Stderr = &stderr
	if e = creator.Start(); e != nil {
		t.Fatal(e)
	}
	reader := bufio.NewReader(stdout)
	for {
		line, e := reader.ReadString('\n')
		if e != nil {
			t.Fatal("creator readiness", e)
		}
		if strings.TrimSpace(line) == "CREATOR" {
			break
		}
	}
	before := lockInode(t, root)
	contender := child(t, root, "", "", "contender", "")
	if b, e := contender.CombinedOutput(); e != nil {
		t.Fatalf("contender %v: %s", e, b)
	}
	stdin.Write([]byte("x"))
	stdin.Close()
	io.Copy(io.Discard, reader)
	if e = creator.Wait(); e != nil {
		t.Fatal(e, stderr.String())
	}
	if !sameInode(before, lockInode(t, root)) {
		t.Fatal("election replaced lock")
	}
	if _, e = Open(root); e != nil {
		t.Fatal(e)
	}
}
func TestIndependentProcessConcurrency(t *testing.T) {
	repo, a := fixture(t, "one")
	other, b := fixture(t, "two")
	s := mustOpen(t)
	expectedA := mustStage(t, mustOpen(t), repo, a)
	expectedB := mustStage(t, mustOpen(t), other, b)
	before := lockInode(t, s.root)
	children := []*exec.Cmd{}
	outputs := []*bytes.Buffer{}
	for n := 0; n < 4; n++ {
		r, c := repo, a
		if n%2 == 1 {
			r, c = other, b
		}
		cmd := child(t, s.root, r, c, "stage", "")
		out := new(bytes.Buffer)
		cmd.Stdout = out
		cmd.Stderr = out
		children = append(children, cmd)
		outputs = append(outputs, out)
		if e := cmd.Start(); e != nil {
			t.Fatal(e)
		}
	}
	for n, c := range children {
		if e := c.Wait(); e != nil {
			t.Fatalf("child %v: %s", e, outputs[n])
		}
		for _, line := range strings.Split(outputs[n].String(), "\n") {
			if strings.HasPrefix(line, "ID ") {
				id := strings.TrimPrefix(line, "ID ")
				if id != expectedA.GenerationID && id != expectedB.GenerationID {
					t.Fatal("mixed generation")
				}
			}
		}
	}
	i, e := s.Inspect()
	if e != nil {
		t.Fatal(e)
	}
	if i.GenerationID != expectedA.GenerationID && i.GenerationID != expectedB.GenerationID {
		t.Fatal("mixed current")
	}
	if !sameInode(before, lockInode(t, s.root)) {
		t.Fatal("unstable process lock")
	}
}
func TestProcessCrashInitializationAndCommitBoundaries(t *testing.T) {
	for _, boundary := range []string{"lock-created", "fsync:lock", "fsync:lock-entry", "after-fsync:lock-entry", "after-rename:store-pending->store.json", "after-fsync:identity", "after-mkdir:generations", "after-mkdir:incomplete", "after-fsync:namespace"} {
		t.Run(boundary, func(t *testing.T) {
			root := scratch(t)
			c := child(t, root, "", "", "crash-init", boundary)
			if b, e := c.CombinedOutput(); e == nil || c.ProcessState.ExitCode() != 42 {
				t.Fatalf("not crashed: %v %s", e, b)
			}
			before := lockInode(t, root)
			s, e := Open(root)
			if e == nil {
				if _, e = s.Recover(); e != nil {
					t.Fatal(e)
				}
			}
			if !sameInode(before, lockInode(t, root)) {
				t.Fatal("crash lock adoption")
			}
		})
	}
	repo, a := fixture(t, "one")
	other, b := fixture(t, "two")
	for _, boundary := range []string{"fsync:stage-entry", "after-fsync:stage-entry", "after-rename:incomplete/STAGE->generations/", "fsync:incomplete-finalization", "after-fsync:incomplete-finalization", "fsync:generations-finalization", "after-fsync:generations-finalization", "symlink:selector", "after-rename:selector-STAGE->candidate-current", "fsync:selection", "after-fsync:selection"} {
		t.Run(boundary, func(t *testing.T) {
			s := mustOpen(t)
			mustStage(t, s, repo, a)
			actual := boundary
			if strings.HasSuffix(actual, "->generations/") {
				i := mustStage(t, mustOpen(t), other, b)
				actual += i.GenerationID
			}
			c := child(t, s.root, other, b, "crash-stage", actual)
			if out, e := c.CombinedOutput(); e == nil || c.ProcessState.ExitCode() != 42 {
				t.Fatalf("not crashed: %v %s", e, out)
			}
			reopened, e := Open(s.root)
			if e == nil {
				if _, e = reopened.Recover(); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
