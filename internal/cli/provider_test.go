package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/cli"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func TestProviderAddEditsTheManifestAndReportsThePlan(t *testing.T) {
	real := realReleaseDigest(t)
	root := projectCheckout(t)
	t.Setenv(cli.ConfigRootEnvironment, root)
	var out, errs bytes.Buffer
	code := cli.Main("upkeeper", []string{"provider", "add", "ollama",
		"--quota-pool", "local", "--hosts", "omarchy-pc", "--models", "qwen3,llama4", "--json"},
		cli.Streams{Out: &out, Err: &errs})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, errs.String())
	}
	report, err := canonjson.DecodeObject(out.Bytes())
	if err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out.String())
	}
	if report["command"] != "provider" || report["action"] != "add" || report["instance"] != "ollama" {
		t.Fatalf("report header: %v", report)
	}
	edited, _ := report["edited"].(map[string]any)
	if profiles, _ := edited["profiles"].([]any); len(profiles) != 1 || profiles[0] != "build" {
		t.Errorf("the report does not say the build profile was edited: %v", report["edited"])
	}
	plan, _ := report["fleet_plan"].(map[string]any)
	enrollment, _ := plan["enrollment_plan"].(map[string]any)
	changes, _ := enrollment["authorization_changes"].([]any)
	text := ""
	for _, change := range changes {
		text += change.(string) + "\n"
	}
	for _, expected := range []string{
		"worker omarchy-pc is authorized for ollama model llama4",
		"worker omarchy-pc is authorized for ollama model qwen3",
		"worker omarchy-pc joins quota pool local",
		"worker omarchy-pc binds ollama to quota pool local",
		"t3-steward worker enroll omarchy-pc --current-catalog",
		fleet.ReloadRefusalAdvice,
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("the plan does not say %q:\n%s", expected, text)
		}
	}
	if enrollment["applied"] != false {
		t.Error("the plan claims to have applied something")
	}
	value, err := release.Load(root)
	if err != nil {
		t.Fatalf("the edited manifest does not load: %v", err)
	}
	document, err := fleet.Parse(value.Component(fleet.Component))
	if err != nil {
		t.Fatalf("fleet.Parse refuses the edited component: %v", err)
	}
	profile, err := document.Resolved("omarchy-pc")
	if err != nil {
		t.Fatal(err)
	}
	if profile.QuotaBindings["ollama"] != "local" || strings.Join(profile.Models["ollama"], ",") != "llama4,qwen3" {
		t.Fatalf("omarchy-pc resolves to %+v", profile)
	}
	if report["release_sha256"] != value.Digest() {
		t.Errorf("reported digest %v is not the written release's %s", report["release_sha256"], value.Digest())
	}
	if realReleaseDigest(t) != real {
		t.Fatal("the repository's own desired/release.json changed")
	}

	// The text form says where the edit landed and names the file.
	out.Reset()
	if code := cli.Main("upkeeper", []string{"provider", "remove", "ollama", "--hosts", "omarchy-pc"},
		cli.Streams{Out: &out, Err: &errs}); code != 0 {
		t.Fatalf("remove: exit %d, stderr=%q", code, errs.String())
	}
	for _, expected := range []string{
		"removed provider ollama from " + release.File, "profile build", "not enrolled",
	} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("remove output does not say %q:\n%s", expected, out.String())
		}
	}
	value, err = release.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	document, err = fleet.Parse(value.Component(fleet.Component))
	if err != nil {
		t.Fatal(err)
	}
	if profile, _ := document.Resolved("omarchy-pc"); profile.Models["ollama"] != nil {
		t.Fatal("the provider survived removal")
	}
}

func TestProviderAddRefusesAnUnknownHostWithoutWriting(t *testing.T) {
	root := projectCheckout(t)
	t.Setenv(cli.ConfigRootEnvironment, root)
	before := snapshot(t, root)
	var out, errs bytes.Buffer
	code := cli.Main("upkeeper", []string{"provider", "add", "ollama",
		"--quota-pool", "local", "--hosts", "laptop"},
		cli.Streams{Out: &out, Err: &errs})
	if code != 30 {
		t.Fatalf("exit %d, want 30; stderr=%q", code, errs.String())
	}
	if !strings.Contains(errs.String(), "laptop") {
		t.Errorf("the refusal does not name the host: %q", errs.String())
	}
	after := snapshot(t, root)
	for path, content := range before {
		if after[path] != content {
			t.Errorf("a refused add rewrote %s", path)
		}
	}
}

func TestProviderUsageErrors(t *testing.T) {
	for _, argv := range [][]string{
		{"provider"},
		{"provider", "add"},
		{"provider", "add", "ollama"},
		{"provider", "add", "ollama", "--quota-pool", "local"},
		{"provider", "add", "ollama", "--hosts", "homelab"},
		{"provider", "remove"},
		{"provider", "rename", "ollama"},
		{"provider", "remove", "ollama", "--quota-pool", "local"},
	} {
		var out, errs bytes.Buffer
		if code := cli.Main("upkeeper", argv, cli.Streams{Out: &out, Err: &errs}); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr=%q", argv, code, errs.String())
		}
	}
	var out, errs bytes.Buffer
	if code := cli.Main("upkeeper", []string{"provider", "--help"}, cli.Streams{Out: &out, Err: &errs}); code != 0 {
		t.Fatalf("help: exit %d", code)
	}
	for _, expected := range []string{
		"usage: upkeeper provider", "--quota-pool", "--hosts", "--models", "pushes nothing", "quota_bindings",
	} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("help lacks %q:\n%s", expected, out.String())
		}
	}
}
