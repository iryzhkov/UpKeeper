package agent

import "github.com/iryzhkov/UpKeeper/internal/fleet"

// A component that does not complete (it is blocked, deferred, rolled back or
// failed) makes only its dependents skip; every independent component after it
// still runs. Only the abort class stops the host: a failed rollback, an
// unknown adapter, or a legacy updater retirement that failed or deferred.
//
// componentDependencies names, for each component, the earlier components it
// cannot be converged without. An edge exists only where the later adapter
// needs state the earlier component installs in the same run; reading another
// component's manifest entry (c.Spec) is not a dependency. The audit of every
// component, with the reason for each edge and non-edge, is in
// docs/upkeeper-push-pull.md under "Component dependencies".
var componentDependencies = map[string][]string{
	// The T3 adapter proves its pair through the installed steward binary:
	// t3Observe runs `t3-steward check` (component_t3.go:137-141), and every
	// artifact path verifies the pair with it (component_t3.go:464).
	"t3": {"t3-steward"},
	// The worker projection configures the steward t3-steward installs
	// (component_worker.go:24, worker-bootstrap.json). It runs no steward
	// binary, but a projection for a steward that did not install is the
	// half-applied pair this table exists to prevent.
	"worker-configuration": {"t3-steward"},
	// The fleet policy gate runs the installed `t3-steward policy`
	// (fleet/route_policy.go:83, via component_fleet_policy.go:110-156).
	fleet.Component: {"t3-steward"},
}

// unavailableDependency names the first dependency of name that did not
// complete in this run, or "".
func unavailableDependency(name string, unavailable map[string]string) string {
	for _, dependency := range componentDependencies[name] {
		if _, missing := unavailable[dependency]; missing {
			return dependency
		}
	}
	return ""
}

// dependencyStates are the outcomes after which a component is unavailable to
// its dependents but the host carries on with every independent component.
var dependencyStates = map[string]bool{
	"blocked": true, "deferred": true, "rolled-back": true, "failed": true,
}
