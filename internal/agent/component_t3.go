package agent

import (
	"regexp"
	"strconv"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

func init() {
	Register("t3", Handler{Observe: t3Observe, Apply: t3Apply})
}

// The steward's check output is the only evidence the agent has about the
// running T3: which version it serves, and how many threads are running in it.
var (
	t3ServedPattern  = regexp.MustCompile(`T3 server (\d+\.\d+\.\d+) `)
	t3ThreadsPattern = regexp.MustCompile(`shell snapshot: \d+ threads, (\d+) running`)
)

// t3StewardBinary is the steward binary this release pins, which is also the
// program that answers for the running T3.
func t3StewardBinary(c *Context) string {
	return c.Path(adapterSpecString(c.Spec("t3-steward"), "path"))
}

func t3Observe(c *Context, spec map[string]any) (map[string]any, error) {
	observed := map[string]any{
		"installed":       nil,
		"served":          nil,
		"running_threads": nil,
	}
	packaged := c.Run([]string{"npm", "ls", "-g", "--depth=0", "--json", "t3"},
		30*time.Second, "")
	if packaged.OK {
		if document, err := canonjson.DecodeObject([]byte(packaged.Output)); err == nil {
			dependencies := adapterSpecMap(document, "dependencies")
			if version := adapterSpecString(adapterSpecMap(dependencies, "t3"), "version"); version != "" {
				observed["installed"] = version
			}
		}
	}
	check := c.Run([]string{t3StewardBinary(c), "check"}, 30*time.Second, "")
	if served := t3ServedPattern.FindStringSubmatch(check.Output); served != nil {
		observed["served"] = served[1]
	}
	if threads := t3ThreadsPattern.FindStringSubmatch(check.Output); threads != nil {
		if running, err := strconv.Atoi(threads[1]); err == nil {
			observed["running_threads"] = running
		}
	}
	observed["service_active"] = c.UnitActive(adapterSpecString(spec, "service"))
	observed["check_ok"] = check.OK
	return observed, nil
}

// t3Idle reports whether an observation proves that no thread is running.
//
// An observation the steward could not make is never idle: a missing answer is
// not the same as an answer of zero.
func t3Idle(observed map[string]any) bool {
	if checkOK, _ := observed["check_ok"].(bool); !checkOK {
		return false
	}
	running, counted := observed["running_threads"].(int)
	return counted && running == 0
}

func t3Apply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	version := adapterSpecString(spec, "version")
	service := adapterSpecString(spec, "service")
	// Seeding never overwrites an existing unit, its credentials or its bind
	// address, and it only starts after the installed package and the steward
	// have been verified.
	seeded := false
	installed, _ := before["installed"].(string)
	active, _ := before["service_active"].(bool)
	if installed == version && !active {
		if !c.DryRun {
			if _, _, err := stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
				return "", err
			}
		}
		var err error
		seeded, err = SeedT3Service(c, actions)
		if err != nil {
			return "", err
		}
		if seeded && !c.DryRun {
			if before, err = t3Observe(c, spec); err != nil {
				return "", err
			}
			installed, _ = before["installed"].(string)
			active, _ = before["service_active"].(bool)
		}
	}
	served, _ := before["served"].(string)
	if installed == version && served == version && active {
		if !c.DryRun {
			if _, _, err := stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
				return "", err
			}
		}
		if seeded {
			return "updated", nil
		}
		return "converged", nil
	}
	if c.DryRun {
		*actions = append(*actions, "would install t3@"+version+
			"; restart requires a fresh zero-running-thread observation")
		return "updated", nil
	}
	if _, _, err := stewardVerifyPairBinary(c, t3StewardBinary(c)); err != nil {
		return "", err
	}
	// Guard the installation too: npm replaces files a running T3 process may
	// still need.
	fresh, err := t3Observe(c, spec)
	if err != nil {
		return "", err
	}
	if !t3Idle(fresh) {
		*actions = append(*actions,
			"T3 change deferred: steward cannot prove zero running threads")
		return "deferred", nil
	}
	if freshVersion, _ := fresh["installed"].(string); freshVersion != version {
		if _, err := c.Must([]string{"npm", "install", "-g", "t3@" + version},
			400*time.Second, "", "install pinned T3"); err != nil {
			return "", err
		}
		if adapterLookPath(c, "mise") != "" {
			if _, err := c.Must([]string{"mise", "reshim"}, 0, "", "refresh T3 shim"); err != nil {
				return "", err
			}
		}
	}
	if fresh, err = t3Observe(c, spec); err != nil {
		return "", err
	}
	if !t3Idle(fresh) {
		*actions = append(*actions,
			"T3 installed; restart deferred because idle evidence changed")
		return "deferred", nil
	}
	if _, err := c.Must([]string{"systemctl", "--user", "restart", service},
		0, "", "restart idle T3"); err != nil {
		return "", err
	}
	after, err := t3Observe(c, spec)
	if err != nil {
		return "", err
	}
	afterInstalled, _ := after["installed"].(string)
	afterServed, _ := after["served"].(string)
	afterActive, _ := after["service_active"].(bool)
	if afterInstalled != version || afterServed != version || !afterActive {
		return "", Failf("T3 did not verify after restart; observe before retry")
	}
	*actions = append(*actions, "installed and serving t3@"+version)
	return "updated", nil
}
