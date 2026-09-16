package agent

import (
	"encoding/json"
	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func jocastaTest(t *testing.T) (*Context, map[string]any, *adapterTestRunner) {
	t.Helper()
	r := &adapterTestRunner{}
	c := adapterTestContext(t, r)
	body := []byte("candidate binary")
	spec := map[string]any{"kind": "github-release", "repository": "iryzhkov/jocasta", "version": "0.1.0", "commit": strings.Repeat("a", 40), "url": "https://jocasta.example.test", "assets": map[string]any{"linux_amd64": agentenv.Digest(body), "linux_arm64": agentenv.Digest(body)}, "hosts": map[string]any{"testhost": "secretref:jocasta/testhost"}}
	secret := c.Path(workerSecretRoot, "jocasta/testhost")
	adapterTestWrite(t, secret, "private-never-log")
	os.Chmod(secret, 0600)
	r.handle = func(cmd procx.Command) procx.Result {
		if cmd.Argv[0] == "gh" {
			dir := cmd.Argv[len(cmd.Argv)-1]
			name := release.JocastaAsset("0.1.0", runtime.GOOS+"_"+runtime.GOARCH)
			if e := os.WriteFile(filepath.Join(dir, name), body, 0600); e != nil {
				t.Fatal(e)
			}
			return procx.Result{}
		}
		return procx.Result{Stdout: `{"version":"0.1.0","commit":"` + strings.Repeat("a", 40) + `"}`}
	}
	return c, spec, r
}
func TestJocastaInstallAndIdempotentObservation(t *testing.T) {
	c, spec, _ := jocastaTest(t)
	before, err := jocastaObserve(c, spec)
	if err != nil {
		t.Fatal(err)
	}
	actions := []string{}
	state, err := jocastaApply(c, spec, before, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("%s %v", state, err)
	}
	after, err := jocastaObserve(c, spec)
	if err != nil || after["converged"] != true {
		t.Fatalf("%v %v", after, err)
	}
	config, _ := os.ReadFile(c.Path(jocastaConfig))
	if strings.Contains(string(config), "private-never-log") {
		t.Fatal("secret leaked")
	}
	var decoded map[string]string
	if json.Unmarshal(config, &decoded) != nil || decoded["token_file"] != c.Path(workerSecretRoot, "jocasta/testhost") {
		t.Fatal("wrong reference")
	}
	state, err = jocastaApply(c, spec, after, &actions)
	if err != nil || state != "converged" {
		t.Fatalf("%s %v", state, err)
	}
}
func TestJocastaRejectsBadArtifactWithoutChangingPrior(t *testing.T) {
	c, spec, r := jocastaTest(t)
	adapterTestWrite(t, c.Path(jocastaBinary), "old")
	original := r.handle
	r.handle = func(cmd procx.Command) procx.Result {
		result := original(cmd)
		if cmd.Argv[0] == "gh" {
			name := release.JocastaAsset("0.1.0", runtime.GOOS+"_"+runtime.GOARCH)
			os.WriteFile(filepath.Join(cmd.Argv[len(cmd.Argv)-1], name), []byte("tampered"), 0600)
		}
		return result
	}
	_, err := jocastaApply(c, spec, map[string]any{}, &[]string{})
	if err == nil {
		t.Fatal("accepted tampering")
	}
	body, _ := os.ReadFile(c.Path(jocastaBinary))
	if string(body) != "old" {
		t.Fatal("changed prior binary")
	}
}
func TestJocastaDryRunAndCredentialSafety(t *testing.T) {
	c, spec, r := jocastaTest(t)
	c.DryRun = true
	before, err := jocastaObserve(c, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = jocastaApply(c, spec, before, &[]string{}); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Fatal("dry run executed program")
	}
	if _, err = os.Stat(c.Path(jocastaConfig)); !os.IsNotExist(err) {
		t.Fatal("dry run wrote config")
	}
	os.Chmod(c.Path(workerSecretRoot, "jocasta/testhost"), 0644)
	if _, err = jocastaObserve(c, spec); err == nil {
		t.Fatal("accepted public credential")
	}
}
func TestJocastaStagingRejectsEscapedParent(t *testing.T) {
	for _, relative := range []string{jocastaBinary, jocastaConfig} {
		t.Run(relative, func(t *testing.T) {
			c, spec, r := jocastaTest(t)
			outside := t.TempDir()
			destination := filepath.Join(outside, filepath.Base(relative))
			adapterTestWrite(t, destination, "outside must remain unchanged")
			parent := filepath.Dir(c.Path(relative))
			if err := os.MkdirAll(filepath.Dir(parent), 0755); err != nil {
				t.Fatal(err)
			}
			original := r.handle
			r.handle = func(cmd procx.Command) procx.Result {
				result := original(cmd)
				if cmd.Argv[0] == "gh" {
					if err := os.Symlink(outside, parent); err != nil {
						t.Fatal(err)
					}
				}
				return result
			}
			_, err := jocastaApply(c, spec, map[string]any{}, &[]string{})
			if err == nil {
				t.Fatal("accepted parent escape introduced during download")
			}
			body, readErr := os.ReadFile(destination)
			if readErr != nil || string(body) != "outside must remain unchanged" {
				t.Fatalf("modified outside destination: %q %v", body, readErr)
			}
		})
	}
}

func TestJocastaRollbackRejectsEscapedParent(t *testing.T) {
	c, spec, r := jocastaTest(t)
	outside := t.TempDir()
	destination := filepath.Join(outside, "config.json")
	config, projectionErr := jocastaProjection(c, mustJocastaSpec(t, spec))
	if projectionErr != nil {
		t.Fatal(projectionErr)
	}
	adapterTestWrite(t, destination, string(config))
	original := r.handle
	r.handle = func(cmd procx.Command) procx.Result {
		result := original(cmd)
		if cmd.Argv[0] == c.Path(jocastaBinary) {
			parent := filepath.Dir(c.Path(jocastaConfig))
			if err := os.Rename(parent, parent+".preserved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, parent); err != nil {
				t.Fatal(err)
			}
			return procx.Result{Code: 1}
		}
		return result
	}
	_, err := jocastaApply(c, spec, map[string]any{}, &[]string{})
	if err == nil || !strings.Contains(err.Error(), "rollback target changed") {
		t.Fatalf("expected guarded rollback refusal: %v", err)
	}
	body, readErr := os.ReadFile(destination)
	if readErr != nil || string(body) != string(config) {
		t.Fatalf("modified outside destination: %q %v", body, readErr)
	}
}

func mustJocastaSpec(t *testing.T, raw map[string]any) *release.Jocasta {
	t.Helper()
	spec, err := release.DecodeJocasta(raw)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestJocastaActivationFailureRestoresBinary(t *testing.T) {
	c, spec, r := jocastaTest(t)
	adapterTestWrite(t, c.Path(jocastaBinary), "old")
	os.Chmod(c.Path(jocastaBinary), 0755)
	original := r.handle
	r.handle = func(cmd procx.Command) procx.Result {
		if cmd.Argv[0] == c.Path(jocastaBinary) {
			return procx.Result{Code: 1}
		}
		return original(cmd)
	}
	_, err := jocastaApply(c, spec, map[string]any{}, &[]string{})
	if err == nil {
		t.Fatal("accepted failed verification")
	}
	if _, ok := err.(*RolledBack); !ok {
		t.Fatalf("expected rollback: %v", err)
	}
	body, _ := os.ReadFile(c.Path(jocastaBinary))
	if string(body) != "old" {
		t.Fatal("lost prior binary")
	}
	if _, err = os.Stat(c.Path(jocastaConfig)); !os.IsNotExist(err) {
		t.Fatal("left new configuration")
	}
}
