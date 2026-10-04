//go:build linux

package toolcatalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func put(t *testing.T, p string, b []byte) {
	t.Helper()
	if e := os.WriteFile(p, b, 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(p, 0644); e != nil {
		t.Fatal(e)
	}
}
func candidate(t *testing.T) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "generation")
	if e := os.Mkdir(root, 0755); e != nil {
		t.Fatal(e)
	}
	raw := fixture(t)
	meta := filepath.Join(dir, "metadata.json")
	put(t, meta, raw)
	payload, e := os.ReadFile("testdata/payload.txt")
	if e != nil {
		t.Fatal(e)
	}
	put(t, filepath.Join(root, "payload.txt"), payload)
	return Options{Metadata: meta, Root: root, SHA256: digest(raw)}, filepath.Join(root, "payload.txt")
}
func repin(t *testing.T, o *Options, m map[string]any) {
	t.Helper()
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	put(t, o.Metadata, b)
	o.SHA256 = digest(b)
}
func denied(t *testing.T, o Options, h hooks, stage string) {
	t.Helper()
	r, e := inspect(o, h)
	if e == nil {
		t.Fatal("invalid candidate verified")
	}
	if stage != "" && e.(*Error).Stage != stage {
		t.Fatalf("stage %s expected %s", e.(*Error).Stage, stage)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "synthetic") || strings.Contains(string(b), o.Metadata) || strings.Contains(string(b), "purpose") || strings.Contains(string(b), "payload") {
		t.Fatalf("failure leaks declarations or paths: %s", b)
	}
	if r["result"] != "invalid-input" || r["exit_code"] != 30 {
		t.Fatal(r)
	}
}
func TestCandidateTruthStates(t *testing.T) {
	o, _ := candidate(t)
	o.Platform = "linux-amd64"
	r, e := Inspect(o)
	if e != nil {
		t.Fatal(e)
	}
	if r["generation_sha256"] != o.SHA256 || r["observation_scope"] != "per-file-no-atomic-snapshot" || r["result"] != "content-verified" {
		t.Fatal(r)
	}
	tools := r["tools"].([]map[string]any)
	if len(tools) != 3 {
		t.Fatal(tools)
	}
	for _, x := range tools {
		for k, want := range map[string]string{"installed": "unknown", "configured": "unknown", "authorized": "unknown", "enforcement": "unqualified", "readiness": "unassessed", "selected": "declared", "content_state": "verified"} {
			if x[k] != want {
				t.Fatalf("%s: %v", k, x)
			}
		}
	}
	if tools[0]["platform_compatibility"] != "compatible" || tools[2]["platform_compatibility"] != "incompatible" {
		t.Fatal(tools)
	}
	o.Platform = ""
	r, e = Inspect(o)
	if e != nil || r["tools"].([]map[string]any)[0]["platform_compatibility"] != "unassessed" {
		t.Fatal(r, e)
	}
}
func TestOptionalMissingAndCorrupt(t *testing.T) {
	o, p := candidate(t)
	m := fixtureMap(t)
	artifact(m)["required"] = false
	for _, v := range m["tools"].([]any) {
		v.(map[string]any)["required"] = false
	}
	repin(t, &o, m)
	if e := os.Remove(p); e != nil {
		t.Fatal(e)
	}
	r, e := Inspect(o)
	if e != nil {
		t.Fatal(e)
	}
	if r["artifacts"].([]map[string]any)[0]["content_state"] != "optional-missing" {
		t.Fatal(r)
	}
	for _, x := range r["tools"].([]map[string]any) {
		if x["content_state"] != "optional-missing" {
			t.Fatal(x)
		}
	}
	put(t, p, []byte("corrupt secret payload"))
	denied(t, o, hooks{}, "artifact-integrity")
}
func TestEmptyCatalog(t *testing.T) {
	o, _ := candidate(t)
	raw := []byte(`{"schema_version":1,"state":"candidate-inert","sources":[],"artifacts":[],"tools":[]}`)
	put(t, o.Metadata, raw)
	o.SHA256 = digest(raw)
	r, e := Inspect(o)
	if e != nil || len(r["tools"].([]map[string]any)) != 0 || len(r["artifacts"].([]map[string]any)) != 0 {
		t.Fatal(r, e)
	}
}
func TestIntegrityRefusals(t *testing.T) {
	cases := map[string]func(*testing.T, *Options, string){
		"required-missing": func(t *testing.T, o *Options, p string) {
			if e := os.Remove(p); e != nil {
				t.Fatal(e)
			}
		},
		"hash": func(t *testing.T, o *Options, p string) { put(t, p, []byte("not approved")) },
		"mode": func(t *testing.T, o *Options, p string) {
			if e := os.Chmod(p, 0600); e != nil {
				t.Fatal(e)
			}
		},
		"metadata-pin": func(t *testing.T, o *Options, p string) { o.SHA256 = strings.Repeat("0", 64) },
		"metadata-too-large": func(t *testing.T, o *Options, p string) {
			put(t, o.Metadata, []byte(strings.Repeat("x", metadataLimit+1)))
		},
		"artifact-too-large": func(t *testing.T, o *Options, p string) {
			if e := os.Truncate(p, fileLimit+1); e != nil {
				t.Fatal(e)
			}
		},
		"artifact-symlink": func(t *testing.T, o *Options, p string) {
			if e := os.Remove(p); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink(o.Metadata, p); e != nil {
				t.Fatal(e)
			}
		},
		"metadata-symlink": func(t *testing.T, o *Options, p string) {
			old := o.Metadata + ".old"
			if e := os.Rename(o.Metadata, old); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink(old, o.Metadata); e != nil {
				t.Fatal(e)
			}
		},
		"root-symlink": func(t *testing.T, o *Options, p string) {
			link := o.Root + "-link"
			if e := os.Symlink(o.Root, link); e != nil {
				t.Fatal(e)
			}
			o.Root = link
		},
		"metadata-symlink-before-dotdot": func(t *testing.T, o *Options, p string) {
			link := filepath.Join(filepath.Dir(o.Metadata), "linked")
			if e := os.Symlink(o.Root, link); e != nil {
				t.Fatal(e)
			}
			o.Metadata = link + "/../metadata.json"
		},
		"root-symlink-before-dotdot": func(t *testing.T, o *Options, p string) {
			link := filepath.Join(filepath.Dir(o.Metadata), "linked")
			if e := os.Symlink(o.Root, link); e != nil {
				t.Fatal(e)
			}
			o.Root = link + "/../generation"
		},
		"metadata-parent-symlink": func(t *testing.T, o *Options, p string) {
			dir := filepath.Dir(o.Metadata)
			link := dir + "-link"
			if e := os.Symlink(dir, link); e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { os.Remove(link) })
			o.Metadata = filepath.Join(link, "metadata.json")
		},
		"artifact-parent-symlink": func(t *testing.T, o *Options, p string) {
			if e := os.Symlink(o.Root, filepath.Join(o.Root, "linked")); e != nil {
				t.Fatal(e)
			}
			m := fixtureMap(t)
			artifact(m)["path"] = "linked/payload.txt"
			repin(t, o, m)
		},
		"metadata-overlap": func(t *testing.T, o *Options, p string) {
			put(t, p, fixture(t))
			o.Metadata = p
			o.SHA256 = digest(fixture(t))
		},
		"hardlink-metadata": func(t *testing.T, o *Options, p string) {
			if e := os.Remove(p); e != nil {
				t.Fatal(e)
			}
			if e := os.Link(o.Metadata, p); e != nil {
				t.Fatal(e)
			}
			m := fixtureMap(t)
			artifact(m)["sha256"] = digest(fixture(t))
			repin(t, o, m)
		},
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) { o, p := candidate(t); modify(t, &o, p); denied(t, o, hooks{}, "") })
	}
}
func TestNonregularNeverDataOpened(t *testing.T) {
	for _, target := range []string{"metadata", "artifact"} {
		for _, kind := range []string{"fifo", "directory", "socket", "device"} {
			t.Run(target+"-"+kind, func(t *testing.T) {
				o, p := candidate(t)
				if target == "metadata" {
					p = o.Metadata
				}
				if e := os.Remove(p); e != nil {
					t.Fatal(e)
				}
				switch kind {
				case "device":
					if e := syscall.Mknod(p, syscall.S_IFCHR|0600, 0x103); e != nil {
						if e == syscall.EPERM {
							t.Skip("synthetic device requires CAP_MKNOD")
						}
						t.Fatal(e)
					}
				case "fifo":
					if e := syscall.Mkfifo(p, 0600); e != nil {
						t.Fatal(e)
					}
				case "directory":
					if e := os.Mkdir(p, 0700); e != nil {
						t.Fatal(e)
					}
				case "socket":
					if e := syscall.Mknod(p, syscall.S_IFSOCK|0600, 0); e != nil {
						t.Fatal(e)
					}
				}
				done := make(chan error, 1)
				opened := []string{}
				go func() {
					_, e := inspect(o, hooks{dataOpen: func(label string) { opened = append(opened, label) }})
					done <- e
				}()
				select {
				case e := <-done:
					if e == nil {
						t.Fatal("accepted nonregular")
					}
				case <-time.After(time.Second):
					t.Fatal("nonregular acquisition blocked without writer")
				}
				for _, label := range opened {
					if label == p || target == "artifact" && label == "payload.txt" {
						t.Fatal("data-open on nonregular target")
					}
				}
			})
		}
	}
}
func TestSubstitutionAndMutationHooks(t *testing.T) {
	t.Run("regular-to-fifo-after-capture", func(t *testing.T) {
		o, p := candidate(t)
		artifactOpens := 0
		h := hooks{afterCapture: func(label string) {
			if label == "payload.txt" {
				if e := os.Rename(p, p+".held"); e != nil {
					t.Fatal(e)
				}
				if e := syscall.Mkfifo(p, 0600); e != nil {
					t.Fatal(e)
				}
			}
		}, dataOpen: func(label string) {
			if label == "payload.txt" {
				artifactOpens++
			}
		}}
		denied(t, o, h, "")
		if artifactOpens != 1 {
			t.Fatal("held regular inode was not the sole artifact data-open")
		}
	})
	t.Run("metadata-to-fifo-after-capture", func(t *testing.T) {
		o, _ := candidate(t)
		opens := 0
		h := hooks{afterCapture: func(label string) {
			if label == o.Metadata {
				if e := os.Rename(o.Metadata, o.Metadata+".held"); e != nil {
					t.Fatal(e)
				}
				if e := syscall.Mkfifo(o.Metadata, 0600); e != nil {
					t.Fatal(e)
				}
			}
		}, dataOpen: func(label string) { opens++ }}
		denied(t, o, h, "")
		if opens != 1 {
			t.Fatal(opens)
		}
	})
	t.Run("final-artifact-path-binding", func(t *testing.T) {
		o, p := candidate(t)
		denied(t, o, hooks{afterRead: func(label string) {
			if e := os.Rename(p, p+".held"); e != nil {
				t.Fatal(e)
			}
			put(t, p, []byte("replacement"))
		}}, "artifact-binding")
	})
	t.Run("metadata-end-binding", func(t *testing.T) {
		o, _ := candidate(t)
		denied(t, o, hooks{beforeFinal: func() {
			if e := os.Rename(o.Metadata, o.Metadata+".held"); e != nil {
				t.Fatal(e)
			}
			put(t, o.Metadata, fixture(t))
		}}, "metadata-binding")
	})
	t.Run("mode-during-read", func(t *testing.T) {
		o, p := candidate(t)
		denied(t, o, hooks{afterDataRead: func(label string) {
			if label == "payload.txt" {
				if e := os.Chmod(p, 0600); e != nil {
					t.Fatal(e)
				}
			}
		}}, "artifact")
	})
	t.Run("growth-before-data-open", func(t *testing.T) {
		o, p := candidate(t)
		denied(t, o, hooks{afterCapture: func(label string) {
			if label == "payload.txt" {
				put(t, p, []byte("grew from held observation to other content"))
			}
		}}, "artifact")
	})
	t.Run("growth-after-read", func(t *testing.T) {
		o, p := candidate(t)
		denied(t, o, hooks{afterDataRead: func(label string) {
			if label == "payload.txt" {
				if e := os.Truncate(p, 100); e != nil {
					t.Fatal(e)
				}
			}
		}}, "artifact")
	})
	t.Run("metadata-mutation-end-read", func(t *testing.T) {
		o, _ := candidate(t)
		n := 0
		denied(t, o, hooks{afterDataRead: func(label string) {
			if label == o.Metadata {
				n++
				if n == 2 {
					put(t, o.Metadata, []byte("changed"))
				}
			}
		}}, "metadata-binding")
	})
}
func TestProcfsUnavailableNoFallback(t *testing.T) {
	for _, p := range []string{filepath.Join(t.TempDir(), "absent"), t.TempDir()} {
		o, _ := candidate(t)
		opens := 0
		denied(t, o, hooks{procPath: p, dataOpen: func(string) { opens++ }}, "procfs-unavailable")
		if opens != 0 {
			t.Fatal("procfs refusal used data open")
		}
	}
}
func TestAggregateActualReadBound(t *testing.T) {
	o, p := candidate(t)
	held, e := absoluteHeld(p, false)
	if e != nil {
		t.Fatal(e)
	}
	defer syscall.Close(held)
	proc, e := syscall.Open("/proc/self/fd", oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer syscall.Close(proc)
	x := &inspector{proc: proc, total: aggregateLimit - 1}
	_, _, e = x.readHeld(held, o.Root, fileLimit, true)
	if e == nil || x.total != aggregateLimit {
		t.Fatal("aggregate actual reads exceeded cap or truncation was accepted", x.total, e)
	}
}
func TestPinnedClosureAllBeforeReport(t *testing.T) {
	o, p := candidate(t)
	m := fixtureMap(t)
	// A last optional corrupt member must refuse the entire otherwise valid catalog.
	a := map[string]any{}
	for k, v := range artifact(m) {
		a[k] = v
	}
	a["id"] = "last"
	a["path"] = "last"
	a["required"] = false
	m["artifacts"] = append(m["artifacts"].([]any), a)
	m["tools"].([]any)[2].(map[string]any)["artifact_ids"] = []string{"payload", "last"}
	repin(t, &o, m)
	put(t, filepath.Join(filepath.Dir(p), "last"), []byte("corrupt"))
	denied(t, o, hooks{}, "artifact-integrity")
}
