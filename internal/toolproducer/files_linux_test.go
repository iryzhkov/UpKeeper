//go:build linux

package toolproducer

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPhysicalReadRefusals(t *testing.T) {
	for _, tc := range []string{"symlink", "ancestor-symlink", "hardlink", "fifo", "socket", "directory", "device", "zero", "oversize", "mode", "special-mode", "relative", "dot", "long-segment"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "file")
			write(t, file, []byte("physical\n"), 420)
			target := file
			limit := int64(fileLimit)
			switch tc {
			case "symlink":
				target = filepath.Join(root, "link")
				must(t, os.Symlink(file, target))
			case "ancestor-symlink":
				must(t, os.Symlink(root, filepath.Join(root, "link")))
				target = filepath.Join(root, "link", "file")
			case "hardlink":
				must(t, os.Link(file, filepath.Join(root, "hard")))
			case "fifo":
				must(t, os.Remove(file))
				must(t, syscall.Mkfifo(file, 0644))
			case "socket":
				must(t, os.Remove(file))
				l, e := net.Listen("unix", file)
				must(t, e)
				defer l.Close()
			case "directory":
				target = root
			case "device":
				target = "/dev/null"
				var st syscall.Stat_t
				must(t, syscall.Lstat(target, &st))
				if st.Mode&syscall.S_IFMT != syscall.S_IFCHR {
					t.Fatal("actual /dev/null is not device")
				}
				t.Logf("actual device dev=%d inode=%d mode=%o", st.Dev, st.Ino, st.Mode)
			case "zero":
				write(t, file, []byte{}, 420)
			case "oversize":
				limit = 4
			case "mode":
				must(t, os.Chmod(file, 0600))
			case "special-mode":
				must(t, syscall.Chmod(file, 04755))
			case "relative":
				target = "file"
			case "dot":
				target = root + "/./file"
			case "long-segment":
				target = root + "/" + string(make([]byte, 256))
			}
			p, e := newPhysical()
			must(t, e)
			defer p.close()
			opens := 0
			p.hooks.dataOpen = func(string) { opens++ }
			if _, _, e = p.file(target, limit); e == nil {
				t.Fatal("accepted physical refusal")
			}
			if opens != 0 {
				t.Fatalf("data-open before refusal: %d", opens)
			}
		})
	}
}
func TestPhysicalReadMutation(t *testing.T) {
	for _, tc := range []string{"capture-growth", "read-growth", "same-size", "file-substitution", "root-substitution", "held-hardlink", "partial-shrink"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "file")
			write(t, file, []byte("physical\n"), 420)
			p, e := newPhysical()
			must(t, e)
			defer p.close()
			must(t, p.root(root))
			once := false
			mutate := func(string) {
				if once {
					return
				}
				once = true
				switch tc {
				case "capture-growth", "read-growth":
					write(t, file, []byte("physical growing\n"), 420)
				case "partial-shrink":
					write(t, file, []byte("x"), 420)
				case "same-size":
					write(t, file, []byte("PHYSICAL\n"), 420)
				case "file-substitution":
					must(t, os.Rename(file, file+".old"))
					write(t, file, []byte("physical\n"), 420)
				case "root-substitution":
					must(t, os.Rename(root, root+".old"))
					must(t, os.Mkdir(root, 0755))
					write(t, file, []byte("physical\n"), 420)
					t.Cleanup(func() { os.RemoveAll(root + ".old") })
				case "held-hardlink":
					must(t, os.Link(file, file+".hard"))
				}
			}
			if tc == "capture-growth" || tc == "partial-shrink" {
				p.hooks.afterCapture = mutate
			} else if tc == "read-growth" {
				p.hooks.afterRead = mutate
			}
			_, _, readErr := p.file(file, fileLimit)
			if readErr == nil {
				mutate(file)
				if e = p.check(); e == nil {
					t.Fatal("mutation accepted")
				}
			}
		})
	}
	t.Run("compile-no-output", func(t *testing.T) {
		f := newFixture(t, nil)
		r, e := compile(f.in, func(p *physical) { p.hooks.beforeFinal = func() { write(t, f.in.InputPath, []byte("{}\n"), 420) } })
		if e == nil || len(r.CatalogBytes) != 0 || len(r.ReceiptBytes) != 0 {
			t.Fatal("compile mutation returned output")
		}
	})
}

func TestOptionalMissingMutation(t *testing.T) {
	f := newFixture(t, nil)
	path := filepath.Join(f.artifacts, f.m.Artifacts[1].Path)
	must(t, os.Remove(path))
	r, e := compile(f.in, func(p *physical) { p.hooks.beforeFinal = func() { write(t, path, []byte("appeared\n"), 420) } })
	if e == nil || len(r.CatalogBytes) != 0 || len(r.ReceiptBytes) != 0 {
		t.Fatal("optional missing observation changed without refusal")
	}
}
