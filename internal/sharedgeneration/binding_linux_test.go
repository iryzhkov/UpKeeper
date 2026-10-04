//go:build linux

package sharedgeneration

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPostAcquireBindingInIndependentProcess(t *testing.T) {
	s := mustOpen(t)
	c := child(t, s.root, "", "", "revalidate", "")
	stdin, e := c.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	stdout, e := c.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if e = c.Start(); e != nil {
		t.Fatal(e)
	}
	reader := bufio.NewReader(stdout)
	for {
		line, e := reader.ReadString('\n')
		if e != nil {
			t.Fatal(e)
		}
		if strings.TrimSpace(line) == "CREATOR" {
			break
		}
	}
	// The child already holds the original lock inode; replacement must refuse
	// after flock, even though a new lock is superficially owned and regular.
	old := lockInode(t, s.root)
	if e = os.Rename(filepath.Join(s.root, "lock"), filepath.Join(s.root, "preserved-lock")); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(s.root, "lock"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	stdin.Write([]byte("x"))
	stdin.Close()
	io.Copy(io.Discard, reader)
	if e = c.Wait(); e != nil {
		t.Fatal(e, stderr.String())
	}
	var kept syscall.Stat_t
	if e = syscall.Lstat(filepath.Join(s.root, "preserved-lock"), &kept); e != nil || !sameInode(old, kept) {
		t.Fatal("evidence lost")
	}
}
func TestEndBindingAndLimits(t *testing.T) {
	t.Run("metadata substitution", func(t *testing.T) {
		s := mustOpen(t)
		changed := false
		s.h.afterRead = func(path string) {
			if path == "store.json" && !changed {
				changed = true
				p := filepath.Join(s.root, path)
				b, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				os.Rename(p, p+".original")
				os.WriteFile(p, b, 0600)
			}
		}
		if _, e := s.Inspect(); !errors.Is(e, ErrRecoveryRequired) {
			t.Fatal("metadata replacement accepted", e)
		}
	})
	t.Run("root substitution", func(t *testing.T) {
		s := mustOpen(t)
		changed := false
		original := s.root
		s.h.afterRead = func(path string) {
			if path == "store.json" && !changed {
				changed = true
				if e := os.Rename(original, original+".preserved"); e != nil {
					t.Fatal(e)
				}
				if e := os.Mkdir(original, 0700); e != nil {
					t.Fatal(e)
				}
			}
		}
		t.Cleanup(func() { os.RemoveAll(original + ".preserved") })
		if _, e := s.Inspect(); !errors.Is(e, ErrRecoveryRequired) {
			t.Fatal("root replacement accepted", e)
		}
	})
	t.Run("aggregate", func(t *testing.T) {
		root := scratch(t)
		expected := map[string]uint32{}
		block := bytes.Repeat([]byte("x"), 64<<10)
		for n := 0; n < 5; n++ {
			p := string(rune('a' + n))
			expected[p] = 0644
			f, e := os.OpenFile(filepath.Join(root, p), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
			if e != nil {
				t.Fatal(e)
			}
			for written := 0; written < fileLimit; written += len(block) {
				if _, e = f.Write(block); e != nil {
					t.Fatal(e)
				}
			}
			if e = f.Close(); e != nil {
				t.Fatal(e)
			}
		}
		x, e := newFilesystem(hooks{})
		if e != nil {
			t.Fatal(e)
		}
		defer x.close()
		fd, e := absolute(root, true)
		if e != nil {
			t.Fatal(e)
		}
		defer syscall.Close(fd)
		if _, e = x.tree(fd, expected); e == nil {
			t.Fatal("aggregate limit not enforced")
		}
	})
	t.Run("metadata size", func(t *testing.T) {
		s := mustOpen(t)
		p := filepath.Join(s.root, "store.json")
		if e := os.Truncate(p, metadataLimit+1); e != nil {
			t.Fatal(e)
		}
		opened := false
		s.h.dataOpen = func(p string) {
			if p == "store.json" {
				opened = true
			}
		}
		if _, e := s.Inspect(); e == nil || opened {
			t.Fatal("oversized metadata data-opened")
		}
	})
}
