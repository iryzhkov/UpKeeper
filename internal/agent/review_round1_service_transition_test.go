package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// Service state is transient: a build selected after preflight must still
// enforce requirements, dependency blocking, dry-run and installation policy.
func TestRound1ReviewServiceStopsAfterPreflight(t *testing.T) {
	for _, dry := range []bool{false, true} {
		for _, install := range []bool{false, true} {
			t.Run(fmt.Sprintf("dry=%v/install=%v", dry, install), func(t *testing.T) {
				c, r := huyangRunningWorld(t, true, "git", "go")
				c.DryRun, c.Payload.DryRun = dry, dry
				c.Host.InstallOSPackages = install
				oldDependencies := componentDependencies
				componentDependencies = map[string][]string{"dependent-next": {"huyang"}}
				t.Cleanup(func() { componentDependencies = oldDependencies })
				c.Selected = []string{"huyang", "dependent-next", "independent-next"}
				c.Components["dependent-next"] = map[string]any{}
				replaceHandler(t, "dependent-next", handlers["independent-next"])
				inner := r.handle
				activeCalls, installations := 0, 0
				r.handle = func(cmd procx.Command) procx.Result {
					if adapterTestMatches(cmd.Argv, "is-active") {
						activeCalls++
						if activeCalls == 1 {
							return procx.Result{Stdout: "active"}
						}
						return procx.Result{Code: 3, Stdout: "inactive"}
					}
					if adapterTestMatches(cmd.Argv, "sudo", "-n", "true") {
						return procx.Result{}
					}
					if adapterTestMatches(cmd.Argv, "sudo", "apt-get", "install") {
						installations++
						// Installation must precede checkout preparation and any build.
						if r.ran("fetch") || r.ran("checkout") || r.ran("make", "build") {
							t.Error("installation came after checkout mutation")
						}
						freshTestTools(t, c.Home, "make")
						return procx.Result{}
					}
					// Once a newly built service has been restarted it can answer again.
					if adapterTestMatches(cmd.Argv, "restart") {
						r.handle = inner
					}
					return inner(cmd)
				}
				result := Execute(c)
				entry := resultEntry(t, result, "huyang")
				dependent := resultEntry(t, result, "dependent-next")
				next := resultEntry(t, result, "independent-next")
				if install && !dry {
					if entry["state"] != "updated" || installations != 1 || !r.ran("make", "build") || dependent["state"] != "updated" || next["state"] != "updated" {
						t.Fatalf("opted-in transition = %v; calls %v", result, r.calls)
					}
				} else {
					message, _ := entry["error"].(string)
					if entry["state"] != "blocked" || !strings.Contains(message, "sudo apt-get install -y make") ||
						dependent["state"] != "skipped" || next["state"] != "updated" {
						t.Fatalf("service changed after preflight: %v", result)
					}
					if installations != 0 || r.ran("fetch") || r.ran("checkout") || r.ran("make", "build") || r.ran("restart") {
						t.Fatalf("blocked transition mutated checkout: %v", r.calls)
					}
				}
			})
		}
	}
}
