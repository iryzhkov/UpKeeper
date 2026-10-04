package publish_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/policytest"
	"github.com/iryzhkov/UpKeeper/internal/publish"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func TestAuthoredPublicationRefusesInvalidPolicyBeforeEffects(t *testing.T) {
	policytest.RequireCLI(t)
	f, _, _ := prepareAuthoredFixture(t)
	authorFleetIntent(t, f.root)
	path := filepath.Join(f.root, release.File)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := canonjson.DecodeObject(body)
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err := json.Unmarshal([]byte(policytest.Policy), &policy); err != nil {
		t.Fatal(err)
	}
	manifest["components"].(map[string]any)[fleet.Component].(map[string]any)["route_policy"] = policy
	body, err = canonjson.MarshalFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatal(err)
	}
	indexBefore, _ := os.ReadFile(filepath.Join(f.root, ".git/index"))
	options := f.options()
	options.Authored = true
	options.ExpectedHead = rootCommit
	options.ExpectedReleaseSHA256 = strings.Repeat("0", 64) // catalog must refuse before digest fence
	for _, models := range [][]string{nil, {"gpt-6-sol"}, {"*"}} {
		t.Run(strings.Join(models, ","), func(t *testing.T) {
			policytest.Catalog(t, models)
			f.calls = nil
			_, err := publish.Release(f.root, options)
			if err == nil || !(strings.Contains(err.Error(), "catalog-authorized") || strings.Contains(err.Error(), "catalog unavailable")) {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(path)
			indexAfter, _ := os.ReadFile(filepath.Join(f.root, ".git/index"))
			if !bytes.Equal(body, after) || !bytes.Equal(indexBefore, indexAfter) || f.did(" add ") || f.did(" commit ") || f.did(" push ") {
				t.Fatalf("refusal had publication effects: %v", f.calls)
			}
		})
	}
}
