package agent

import (
	"encoding/json"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Read 5 is the original post-observation fence; reads 6–8 are the
// independent final upstream helper; read 9 is the final transaction fence.
// Each interleaving must actually fire, so reordered checks cannot silently pass.
func TestT3TransitionFinalOriginalBindingFence(t *testing.T) {
	for _, driftAt := range []int{0, 5, 6, 7, 8, 9} {
		t.Run(string(rune('0'+driftAt)), func(t *testing.T) {
			c, s, r, p := t3ArtifactHost(t, false, false, 0, 0)
			a := []string{}
			b, _ := t3Observe(c, s)
			if got, e := t3Apply(c, s, b, &a); e != nil || got != "updated" {
				t.Fatalf("setup %s %v", got, e)
			}
			root := filepath.Join(p, "lib/node_modules/t3")
			old, err := release.T3TreeDigest(root)
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(c.Home, release.T3StatePath, "current")
			oldState, err := release.T3TreeDigest(state)
			if err != nil {
				t.Fatal(err)
			}
			oldLink, err := os.Readlink(filepath.Join(p, "bin/t3"))
			if err != nil {
				t.Fatal(err)
			}
			a = nil
			priorJournals, _ := filepath.Glob(filepath.Join(p, "lib/node_modules/.upkeeper-t3-*/transaction.json"))
			priorPaths := map[string]bool{}
			for _, path := range priorJournals {
				priorPaths[path] = true
			}
			artifact, archive, receipt := t3AgentFixture(t, release.T3Target())
			delete(s, "artifact")
			orig := r.handle
			restarts, reads, acquisitions := 0, 0, 0
			changed := false
			native := filepath.Join(root, "node_modules/@t3code/t3-"+release.T3Target()+"/t3")
			args := "serve --host 127.0.0.1 --port 7392 --no-browser"
			r.handle = func(cmd procx.Command) procx.Result {
				if len(cmd.Argv) > 5 && cmd.Argv[0] == "npm" && cmd.Argv[1] == "install" {
					acquisitions++
					dest := filepath.Join(cmd.Argv[4], "lib/node_modules/t3")
					if e := os.MkdirAll(dest, 0755); e != nil {
						t.Fatal(e)
					}
					if _, e := release.VerifyT3Bundle(archive, receipt, artifact, release.T3Target(), "0.0.45", dest); e != nil {
						t.Fatal(e)
					}
					path := filepath.Join(dest, "package.json")
					body, _ := os.ReadFile(path)
					var pkg map[string]any
					_ = json.Unmarshal(body, &pkg)
					delete(pkg, "upkeeperControlledBuild")
					body, _ = json.Marshal(pkg)
					if e := os.WriteFile(path, body, 0644); e != nil {
						t.Fatal(e)
					}
					return procx.Result{}
				}
				if adapterTestMatches(cmd.Argv, "ExecStart") && restarts > 0 {
					reads++
					t.Logf("postrestart binding read=%d", reads)
					if driftAt > 0 && reads >= driftAt {
						changed = true
						adapterTestWrite(t, filepath.Join(t3ProcRoot, "124/cmdline"), native+"\x00"+strings.ReplaceAll(args, " ", "\x00")+"\x00")
						return procx.Result{Stdout: t3BindingRecord(filepath.Join(p, "bin/t3"), args)}
					}
				}
				result := orig(cmd)
				if adapterTestMatches(cmd.Argv, "restart") {
					restarts++
				}
				return result
			}
			b, e := t3Observe(c, s)
			if e != nil {
				t.Fatal(e)
			}
			got, e := t3Apply(c, s, b, &a)
			now, digestErr := release.T3TreeDigest(root)
			if digestErr != nil {
				t.Fatal(digestErr)
			}
			journals, _ := filepath.Glob(filepath.Join(p, "lib/node_modules/.upkeeper-t3-*/transaction.json"))
			phase := ""
			newJournals := 0
			for _, path := range journals {
				if priorPaths[path] {
					continue
				}
				newJournals++
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var v map[string]any
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatal(err)
				}
				phase, _ = v["phase"].(string)
			}
			verificationReads := reads
			obs, obsErr := t3Observe(c, s)
			if obsErr != nil {
				t.Fatal(obsErr)
			}
			t.Logf("after obs=%v error=%v journal=%s upstreamVerified=%v", obs, obsErr, phase, release.VerifyT3Upstream(c.Home, p, "0.0.45") == nil)
			t.Logf("transition status=%q error=%v restarts=%d reads=%d changed=%v restored=%v", got, e, restarts, verificationReads, changed, now == old)
			if acquisitions != 1 || restarts != 1 || newJournals != 1 {
				t.Fatalf("duplicate/missing effects: acquisitions=%d restarts=%d new journals=%d", acquisitions, restarts, newJournals)
			}
			if driftAt == 0 {
				if got != "updated" || e != nil || phase != "committed" || changed {
					t.Fatalf("matching transition fails %s %v", got, e)
				}
			} else {
				nowState, stateErr := release.T3TreeDigest(state)
				nowLink, linkErr := os.Readlink(filepath.Join(p, "bin/t3"))
				if !changed || e == nil || got == "updated" || got == "converged" || now != old ||
					stateErr != nil || nowState != oldState || linkErr != nil || nowLink != oldLink || phase != "files-restored" {
					t.Fatalf("drift not refused/restored: status=%q error=%v changed=%v tree=%v state=%v link=%v phase=%q", got, e, changed, now == old, nowState == oldState, nowLink == oldLink, phase)
				}
				for _, action := range a {
					if strings.Contains(action, "installed and serving verified") {
						t.Fatalf("success action after refusal: %q", action)
					}
				}
			}
			if driftAt == 0 {
				count := len(journals)
				if got, err := t3Apply(c, s, obs, &a); err != nil || got != "converged" {
					t.Fatalf("unchanged followup %q %v", got, err)
				}
				again, err := filepath.Glob(filepath.Join(p, "lib/node_modules/.upkeeper-t3-*/transaction.json"))
				if err != nil || len(again) != count || restarts != 1 || acquisitions != 1 {
					t.Fatal("convergence duplicated transaction effects")
				}
			}
		})
	}
}
