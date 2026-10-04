//go:build linux

package author

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

func TestSharedManifestEnvelopeBoundsAndCanonical(t *testing.T) {
	base := map[string]any{"schema_version": json.Number("1"), "captured_at": "now", "captured_from": "host", "components": map[string]any{}}
	good, err := canonjson.MarshalFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSharedManifest(good); err != nil {
		t.Fatal(err)
	}
	cases := [][]byte{
		[]byte("{}"), []byte("null"), append(append([]byte(nil), good...), []byte("{}")...),
		bytes.Replace(good, []byte(`"schema_version": 1`), []byte(`"schema_version": 1.0`), 1),
		bytes.Replace(good, []byte(`"schema_version": 1`), []byte(`"schema_version": "1"`), 1),
		bytes.Replace(good, []byte(`"components": {}`), []byte(`"components": null`), 1),
		bytes.Replace(good, []byte(`"components": {}`), []byte(`"components": {"x":1,"x":2}`), 1),
		bytes.TrimSpace(good), []byte(strings.Repeat(" ", sharedManifestLimit+1)), []byte{0xff},
	}
	for i, raw := range cases {
		if _, err := decodeSharedManifest(raw); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	if err := boundedSharedJSON([]byte(strings.Repeat("[", 65)+"0"+strings.Repeat("]", 65)), 64, 8192); err == nil {
		t.Fatal("depth accepted")
	}
	if err := boundedSharedJSON([]byte("["+strings.Repeat("0,", 8192)+"0]"), 64, 8192); err == nil {
		t.Fatal("container accepted")
	}
}

func TestSharedManifestSecureAcquisitionAndReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "release.json")
	write := func() {
		t.Helper()
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	raw, verify, closeInput, err := acquireSharedManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeInput()
	if string(raw) != "{}\n" || verify() != nil {
		t.Fatal("initial binding")
	}
	if err := os.Rename(path, filepath.Join(root, "prior")); err != nil {
		t.Fatal(err)
	}
	write()
	if verify() == nil {
		t.Fatal("same byte replacement accepted")
	}
	for _, kind := range []string{"symlink", "hardlink", "fifo", "directory", "oversized", "writable", "linked-parent", "unsafe-parent", "growth"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "input")
			if err := os.WriteFile(p, []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				os.Remove(p)
				if err := os.Symlink(path, p); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(p, filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				os.Remove(p)
				if err := syscall.Mkfifo(p, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				os.Remove(p)
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.Truncate(p, sharedManifestLimit+1); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(p, 0666); err != nil {
					t.Fatal(err)
				}
			case "linked-parent":
				link := filepath.Join(dir, "parent")
				if err := os.Symlink(root, link); err != nil {
					t.Fatal(err)
				}
				p = filepath.Join(link, "release.json")
			case "unsafe-parent":
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(dir, 0700)
			}
			_, verify, closeInput, err := acquireSharedManifest(p)
			if kind == "growth" {
				if err != nil {
					t.Fatal(err)
				}
				defer closeInput()
				if err := os.WriteFile(p, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
				if verify() == nil {
					t.Fatal("growth accepted")
				}
				return
			}
			if err == nil {
				closeInput()
				t.Fatal("unsafe input accepted")
			}
		})
	}
}

func TestSharedPolicyArgumentAndRepositoryBinding(t *testing.T) {
	for _, o := range []SharedPolicyOptions{
		{}, {Repository: "relative", Commit: strings.Repeat("a", 40), ReleaseManifestPath: "/tmp/input", ExpectedReleaseManifestSHA256: strings.Repeat("b", 64)},
		{Repository: "/tmp", Commit: strings.Repeat("A", 40), ReleaseManifestPath: "/tmp/input", ExpectedReleaseManifestSHA256: strings.Repeat("b", 64)},
		{Repository: "/tmp", Commit: strings.Repeat("a", 40), ReleaseManifestPath: "/tmp/../input", ExpectedReleaseManifestSHA256: strings.Repeat("b", 64)},
	} {
		if p, err := PrepareSharedWithPolicy(o); err == nil {
			p.Close()
			t.Fatal("invalid arguments accepted")
		}
	}
	root := t.TempDir()
	p := filepath.Join(root, "repo")
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	verify, closeInput, err := acquireSharedRepository(p)
	if err != nil {
		t.Fatal(err)
	}
	defer closeInput()
	if err := os.Rename(p, filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	if verify() == nil {
		t.Fatal("repository replacement accepted")
	}
}

func TestSharedOwnerVersionedClosedEncoding(t *testing.T) {
	raw, err := os.ReadFile("../../shared/agent-instructions/owner.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSharedOwnerEncoding(raw); err != nil {
		t.Fatal(err)
	}
	value, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"owner", "source_files", "policy_input_contract", "legacy_source_tree"} {
		copy := map[string]any{}
		for k, v := range value {
			copy[k] = v
		}
		delete(copy, key)
		b, _ := canonjson.MarshalFile(copy)
		if ValidateSharedOwnerEncoding(b) == nil {
			t.Fatalf("missing %s accepted", key)
		}
		copy[key] = nil
		b, _ = canonjson.MarshalFile(copy)
		if ValidateSharedOwnerEncoding(b) == nil {
			t.Fatalf("null %s accepted", key)
		}
	}
	value["schema_version"] = json.Number("1")
	raw, _ = canonjson.MarshalFile(value)
	if ValidateSharedOwnerEncoding(raw) == nil {
		t.Fatal("v1 accepted v2 fields")
	}
}
