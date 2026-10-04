//go:build linux

package sharedgeneration

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMutationParentsNeverFollowSymlinks(t *testing.T) {
	repo, commit := fixture(t, "approved")
	for _, boundary := range []string{"mkdir:skills/huyang", "write:skills/huyang/SKILL.md", "rename:incomplete/"} {
		t.Run(boundary, func(t *testing.T) {
			s := mustOpen(t)
			outside := scratch(t)
			hit := false
			s.h.event = func(p string) {
				if hit || !strings.HasPrefix(p, boundary) {
					return
				}
				hit = true
				var victim string
				if strings.HasPrefix(p, "rename:") {
					victim = filepath.Join(s.root, "generations")
				} else {
					stages, e := os.ReadDir(filepath.Join(s.root, "incomplete"))
					if e != nil || len(stages) != 1 {
						t.Fatal("stage missing")
					}
					victim = filepath.Join(s.root, "incomplete", stages[0].Name(), "skills")
				}
				if e := os.Rename(victim, victim+".preserved"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(outside, victim); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := s.Stage(repo, commit); e == nil || !hit {
				t.Fatal("mutation parent accepted")
			}
			entries, e := os.ReadDir(outside)
			if e != nil || len(entries) != 0 {
				t.Fatal("symlink caused external writes")
			}
		})
	}
}
func TestUnownedIncompletePreserved(t *testing.T) {
	s := mustOpen(t)
	p := filepath.Join(s.root, "incomplete", "unowned")
	if e := os.WriteFile(p, []byte("evidence"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Recover(); e == nil {
		t.Fatal("unowned incomplete adopted")
	}
	if b, e := os.ReadFile(p); e != nil || string(b) != "evidence" {
		t.Fatal("evidence removed")
	}
}
func TestOtherUIDRefusal(t *testing.T) {
	root := scratch(t)
	var st syscall.Stat_t
	if e := syscall.Lstat(root, &st); e != nil {
		t.Fatal(e)
	}
	st.Uid = uint32(os.Getuid() + 1)
	if e := owned(st, 0700, true); e == nil {
		t.Fatal("unowned root accepted by production ownership helper")
	}
}
func TestExactPathLimits(t *testing.T) {
	if !validPath(strings.Repeat("a", 255) + "/" + strings.Repeat("b", 255)) {
		t.Fatal("valid boundary refused")
	}
	if validPath("a\u0085") {
		t.Fatal("Unicode control accepted")
	}
	// Types are captured without a preliminary data open.
	root := scratch(t)
	os.Mkdir(filepath.Join(root, "parent"), 0700)
	syscall.Mkfifo(filepath.Join(root, "parent", "file"), 0644)
	fd, e := absolute(root, true)
	if e != nil {
		t.Fatal(e)
	}
	defer syscall.Close(fd)
	x, e := newFilesystem(hooks{dataOpen: func(string) { t.Fatal("FIFO data opened") }})
	if e != nil {
		t.Fatal(e)
	}
	defer x.close()
	if _, _, e = x.read(fd, "parent/file", fileLimit, 0644); e == nil {
		t.Fatal("FIFO accepted")
	}
}
