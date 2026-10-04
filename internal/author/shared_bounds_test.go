//go:build linux

package author

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func boundedPreparationFixture(t *testing.T) (*SharedPreparation, string, string, *sharedPolicyInput) {
	t.Helper()
	root := t.TempDir()
	source, home := filepath.Join(root, "source"), filepath.Join(root, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../shared/agent-instructions/owner.json")
	if err != nil {
		t.Fatal(err)
	}
	var owner SharedOwner
	if err := json.Unmarshal(raw, &owner); err != nil {
		t.Fatal(err)
	}
	p := &SharedPreparation{Owner: owner, scratch: root, SourceSHA256: map[string]string{}, OutputSHA256: map[string]string{}}
	for _, path := range append(SharedSourcePaths(), "owner.json") {
		content, mode := []byte("x"), os.FileMode(0644)
		if path == "owner.json" {
			content = raw
		}
		if path == Generator || path == "config/bin/agents-role-project" {
			mode = 0755
		}
		file := filepath.Join(source, path)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, content, mode); err != nil {
			t.Fatal(err)
		}
		p.SourceSHA256[path] = digestOf(content)
	}
	input := &sharedPolicyInput{policy: []byte("{}\n"), digest: strings.Repeat("a", 64), verify: func() error { return nil }}
	return p, source, home, input
}

// Exercises preparation itself, before any store boundary. The generator is a
// synthetic runner; real pinned consumer/generator integration remains separate.
func TestSharedPreparationOutputBounds(t *testing.T) {
	for _, kind := range []string{"over256K-0600", "exact16M", "over16M", "exact64M", "over64M", "check-growth", "extra-oversized", "extra-directory", "source-growth"} {
		t.Run(kind, func(t *testing.T) {
			p, source, home, input := boundedPreparationFixture(t)
			reserve, err := sharedPayloadReserve(source, input.policy)
			if err != nil {
				t.Fatal(err)
			}
			sizes := make([]int, len(policyInstructions))
			for i := range sizes {
				sizes[i] = 1
			}
			switch kind {
			case "over256K-0600":
				sizes[0] = (256 << 10) + 1
			case "exact16M", "check-growth":
				sizes[0] = 16 << 20
			case "over16M":
				sizes[0] = (16 << 20) + 1
			case "exact64M", "over64M":
				for i := 0; i < 4; i++ {
					sizes[i] = 16 << 20
				}
				sizes[3] -= int(reserve) + len(sizes) - 4
				if kind == "over64M" {
					sizes[3]++
				}
			}
			calls := 0
			runner := func(command procx.Command) procx.Result {
				calls++
				if calls == 1 {
					for i, path := range policyInstructions {
						file := filepath.Join(home, path)
						if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
							t.Fatal(err)
						}
						modes := []os.FileMode{0600, 0644, 0700, 0755}
						if err := os.WriteFile(file, bytes.Repeat([]byte("x"), sizes[i]), modes[i%4]); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "extra-oversized" {
						file := filepath.Join(home, "extra")
						if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Truncate(file, 1<<30); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "extra-directory" {
						if err := os.Mkdir(filepath.Join(home, "extra"), 0700); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "source-growth" {
						if err := os.Truncate(filepath.Join(source, Generator), sharedFileLimit+1); err != nil {
							t.Fatal(err)
						}
					}
				} else if kind == "check-growth" {
					file := filepath.Join(home, policyInstructions[0])
					if err := os.Truncate(file, sharedFileLimit+1); err != nil {
						t.Fatal(err)
					}
				}
				return procx.Result{}
			}
			got, err := prepareSharedPolicy(p, runner, source, home, input)
			accepted := kind == "over256K-0600" || kind == "exact16M" || kind == "exact64M"
			if accepted {
				if err != nil {
					t.Fatal(err)
				}
				if calls != 2 || len(got.Sources.Instructions) != 18 {
					t.Fatal("incomplete derivation")
				}
				var total int64 = reserve
				for _, file := range got.Sources.Instructions {
					total += int64(len(file.Content))
				}
				if kind == "exact64M" && total != sharedAggregateLimit {
					t.Fatal("not exact closed aggregate", total)
				}
				if got.Sources.Instructions[policyInstructions[0]].Permissions != 0600 {
					t.Fatal("mode changed")
				}
			} else {
				if err == nil || got != nil {
					t.Fatal("unsafe preparation returned")
				}
				if kind != "check-growth" && calls > 1 {
					t.Fatal("refusal deferred until check")
				}
			}
		})
	}
}

func TestSharedBoundedReadGrowthBeforeDataOpen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, closeInput, err := acquireSharedFile(file, sharedFileLimit, func() {
		if err := os.Truncate(file, sharedFileLimit+1); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		closeInput()
		t.Fatal("read-time growth accepted")
	}
}
