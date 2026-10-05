package agent

import (
	"errors"
	"path/filepath"

	"github.com/iryzhkov/UpKeeper/internal/release"
)

func t3UpstreamServed(c *Context, spec map[string]any, prefix string) bool {
	if release.VerifyT3Upstream(c.Home, prefix, adapterSpecString(spec, "version")) != nil ||
		t3ControlledSelected(c, prefix) != nil || !t3UnitSelectsOwned(c, adapterSpecString(spec, "service"), prefix) {
		return false
	}
	nativePath := "node_modules/@t3code/t3-" + release.T3Target() + "/t3"
	body, e := release.T3ReadFile(filepath.Join(prefix, "lib/node_modules/t3", nativePath), release.T3FileLimit)
	if e != nil {
		return false
	}
	r := &release.T3Retained{Prefix: prefix, Receipt: &release.T3Receipt{Platform: release.T3Target(), Files: []release.T3File{{Path: nativePath, SHA256: release.T3Digest(body)}}}}
	return t3ServedArtifact(c, spec, r)
}

// Retained upstream files remain identity-managed on every subsequent apply;
// repairing a serving split never reacquires or overwrites the verified files.
func t3RetainedUpstreamApply(c *Context, spec map[string]any, actions *[]string) (string, error) {
	if c.DryRun {
		*actions = append(*actions, "would reconcile retained upstream serving identity after fresh idle proof")
		return "updated", nil
	}
	if _, _, e := stewardVerifyPairBinary(c, t3StewardBinary(c)); e != nil {
		return "", e
	}
	fresh, e := t3Observe(c, spec)
	if e != nil {
		return "", e
	}
	prefix := adapterSpecString(fresh, "npm_prefix")
	if fresh["artifact_installed"] != true {
		return "", errors.New("retained upstream installed identity changed")
	}
	if !t3Idle(fresh) {
		return "deferred", nil
	}
	service := adapterSpecString(spec, "service")
	binding, e := t3UnitBinding(c, service, prefix)
	if e != nil {
		return "", e
	}
	if e = t3VerifyInstalledCLI(c, adapterSpecString(spec, "version")); e != nil {
		return "", e
	}
	fresh, e = t3Observe(c, spec)
	if e != nil {
		return "", e
	}
	if fresh["artifact_installed"] != true || adapterSpecString(fresh, "npm_prefix") != prefix {
		return "", errors.New("retained upstream identity changed before restart")
	}
	if !t3Idle(fresh) || !t3BoundaryIdle(c) {
		return "deferred", nil
	}
	current, e := t3UnitBinding(c, service, prefix)
	if e != nil || current != binding {
		return "", errors.New("retained upstream service mapping changed before restart")
	}
	if _, e = c.Must([]string{"systemctl", "--user", "restart", service}, 0, "", "restart idle retained upstream T3"); e != nil {
		return "", e
	}
	after, e := t3Observe(c, spec)
	version := adapterSpecString(spec, "version")
	if e != nil || after["artifact_installed"] != true || after["artifact_served"] != true ||
		after["check_ok"] != true || after["service_active"] != true || after["installed"] != version || after["served"] != version {
		return "", errors.New("retained upstream files unchanged; post-restart serving identity or health unverified")
	}
	*actions = append(*actions, "serving verified retained upstream T3 identity")
	return "updated", nil
}
