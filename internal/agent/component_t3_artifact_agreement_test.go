package agent

import (
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentConfiguredKernelAgreement(t *testing.T) {
	for _, kernel := range []string{"serve --host 127.0.0.1 --port 7392 --no-browser", "serve --host 0.0.0.0 --port 7391 --no-browser", "start"} {
		t.Run(kernel, func(t *testing.T) {
			c, s, r, p, n := t3ServeHost(t, "native", t3ServeArgs, false)
			a := []string{}
			b, _ := t3Observe(c, s)
			if got, e := t3Apply(c, s, b, &a); e != nil || got != "updated" {
				t.Fatalf("setup %s %v", got, e)
			}
			native := filepath.Join(p, "lib/node_modules/t3/node_modules/@t3code/t3-"+release.T3Target()+"/t3")
			adapterTestWrite(t, filepath.Join(t3ProcRoot, "124/cmdline"), native+"\x00"+strings.ReplaceAll(kernel, " ", "\x00")+"\x00")
			if _, e := t3UnitBinding(c, "t3code.service", p); e != nil {
				t.Fatal(e)
			}
			obs, e := t3Observe(c, s)
			if e != nil {
				t.Fatal(e)
			}
			got, e := t3Apply(c, s, obs, &a)
			t.Logf("configured=%q kernel=%q installed=%v served=%v active=%v health=%v apply=%q error=%v totalRestarts=%d", t3ServeArgs, kernel, obs["artifact_installed"], obs["artifact_served"], obs["service_active"], obs["check_ok"], got, e, *n)
			if obs["artifact_served"] == true || got == "converged" {
				t.Fatal("different direct-native invocation accepted as configured serving identity")
			}
			_ = r
		})
	}
}
func TestIndependentPostRestartSupportedConfigDrift(t *testing.T) {
	c, s, r, p, n := t3ServeHost(t, "native", t3ServeArgs, false)
	original := r.handle
	r.handle = func(cmd procx.Command) procx.Result {
		if *n > 0 && adapterTestMatches(cmd.Argv, "ExecStart") {
			native := filepath.Join(p, "lib/node_modules/t3/node_modules/@t3code/t3-"+release.T3Target()+"/t3")
			return procx.Result{Stdout: t3BindingRecord(native, "serve --host 127.0.0.1 --port 7392 --no-browser")}
		}
		return original(cmd)
	}
	b, _ := t3Observe(c, s)
	a := []string{}
	got, e := t3Apply(c, s, b, &a)
	t.Logf("post-restart changed configured port, running original port: status=%q error=%v restarts=%d", got, e, *n)
	if got == "updated" && e == nil {
		t.Fatal("committed despite supported configuration/kernel mismatch")
	}
}

func TestT3InvocationAgreementAcrossOwnedPaths(t *testing.T) {
	for _, mode := range []string{"native", "launcher", "shim", "mise"} {
		for _, args := range []string{"start", t3ServeArgs} {
			t.Run(mode+"/"+args, func(t *testing.T) {
				c, s, r, p, n := t3ServeHost(t, mode, args, false)
				if mode == "mise" {
					mise := c.Path("owned/mise")
					adapterTestWrite(t, mise, "mise")
					original := r.handle
					r.handle = func(cmd procx.Command) procx.Result {
						if adapterTestMatches(cmd.Argv, "ExecStart") {
							return procx.Result{Stdout: t3BindingRecord(mise, "exec -- "+filepath.Join(p, "bin/t3")+" "+args)}
						}
						return original(cmd)
					}
				}
				a := []string{}
				b, e := t3Observe(c, s)
				if e != nil {
					t.Fatal(e)
				}
				if got, e := t3Apply(c, s, b, &a); e != nil || got != "updated" {
					t.Fatalf("install %s %v", got, e)
				}
				b, e = t3Observe(c, s)
				if e != nil || b["artifact_served"] != true {
					t.Fatalf("matching %v %v", b, e)
				}
				if got, e := t3Apply(c, s, b, &a); e != nil || got != "converged" || *n != 1 {
					t.Fatalf("converged %s %v %d", got, e, *n)
				}
				native := filepath.Join(p, "lib/node_modules/t3/node_modules/@t3code/t3-"+release.T3Target()+"/t3")
				for _, kernel := range []string{"serve --host 127.0.0.1 --port 7392 --no-browser", "serve --host 0.0.0.0 --port 7391 --no-browser", "serve --host 127.0.0.1 --port 7391"} {
					adapterTestWrite(t, filepath.Join(t3ProcRoot, "124/cmdline"), native+"\x00"+strings.ReplaceAll(kernel, " ", "\x00")+"\x00")
					b, e = t3Observe(c, s)
					if e != nil || b["artifact_served"] == true {
						t.Fatalf("different argv %q: %v %v", kernel, b, e)
					}
				}
			})
		}
	}
}

// Even a new matching supported kernel/config pair cannot replace the original
// stable configuration after the restart. Files restore; no second restart is owned.
func TestT3PostRestartMatchingConfigurationDrift(t *testing.T) {
	for _, mode := range []string{"native", "launcher", "shim"} {
		t.Run(mode, func(t *testing.T) {
			c, s, r, p, n := t3ServeHost(t, mode, t3ServeArgs, false)
			root := filepath.Join(p, "lib/node_modules/t3")
			old, e := release.T3TreeDigest(root)
			if e != nil {
				t.Fatal(e)
			}
			original := r.handle
			changed := "serve --host 127.0.0.1 --port 7392 --no-browser"
			r.handle = func(cmd procx.Command) procx.Result {
				result := original(cmd)
				native := filepath.Join(root, "node_modules/@t3code/t3-"+release.T3Target()+"/t3")
				if *n > 0 && adapterTestMatches(cmd.Argv, "ExecStart") {
					path := filepath.Join(p, "bin/t3")
					if mode == "native" {
						path = native
					}
					if mode == "shim" {
						path = c.Path(".local/share/mise/shims/t3")
					}
					return procx.Result{Stdout: t3BindingRecord(path, changed)}
				}
				if adapterTestMatches(cmd.Argv, "restart") && result.Code == 0 {
					adapterTestWrite(t, filepath.Join(t3ProcRoot, "124/cmdline"), native+"\x00"+strings.ReplaceAll(changed, " ", "\x00")+"\x00")
				}
				return result
			}
			b, e := t3Observe(c, s)
			if e != nil {
				t.Fatal(e)
			}
			a := []string{}
			got, e := t3Apply(c, s, b, &a)
			now, digestErr := release.T3TreeDigest(root)
			if e == nil || got == "updated" || *n != 1 || digestErr != nil || now != old {
				t.Fatalf("drift %s %v restarts=%d restored=%v", got, e, *n, now == old)
			}
		})
	}
}

func TestT3RetainedUpstreamPostRestartMatchingConfigurationDrift(t *testing.T) {
	c, s, r, p := t3RepairUpstreamHost(t)
	root := filepath.Join(p, "lib/node_modules/t3")
	old, e := release.T3TreeDigest(root)
	if e != nil {
		t.Fatal(e)
	}
	native := filepath.Join(root, "node_modules/@t3code/t3-"+release.T3Target()+"/t3")
	adapterTestWrite(t, filepath.Join(t3ProcRoot, "124/cmdline"), native+"\x00start\x00")
	original := r.handle
	restarts := 0
	changed := "serve --host 127.0.0.1 --port 7392 --no-browser"
	r.handle = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "ExecStart") {
			args := t3ServeArgs
			if restarts > 0 {
				args = changed
			}
			return procx.Result{Stdout: t3BindingRecord(native, args)}
		}
		result := original(cmd)
		if adapterTestMatches(cmd.Argv, "restart") {
			restarts++
			adapterTestWrite(t, filepath.Join(t3ProcRoot, "124/cmdline"), native+"\x00"+strings.ReplaceAll(changed, " ", "\x00")+"\x00")
		}
		return result
	}
	b, e := t3Observe(c, s)
	if e != nil || b["artifact_installed"] != true || b["artifact_served"] == true {
		t.Fatalf("before %v %v", b, e)
	}
	a := []string{}
	got, e := t3Apply(c, s, b, &a)
	now, digestErr := release.T3TreeDigest(root)
	if e == nil || got == "updated" || restarts != 1 || digestErr != nil || now != old {
		t.Fatalf("upstream drift %s %v restarts=%d unchanged=%v", got, e, restarts, now == old)
	}
}
