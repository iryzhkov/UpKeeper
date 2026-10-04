package release

import (
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// ValidateFleetConfiguration checks the steward-fleet-configuration component
// of a release.
//
// The whole check lives in the fleet package, because the same document is read
// by the controller, by the target-side agent and by the enrollment planner,
// and three copies of one schema is three chances to disagree about what is
// authorized.
func ValidateFleetConfiguration(component map[string]any) error {
	document, err := fleet.Parse(component)
	if err != nil {
		return err
	}
	return document.ValidateRoutePolicy()
}

// FleetIntent returns the parsed fleet intent of a release, or nothing when the
// release does not carry the component. A release without it is not an error:
// it is a release captured before the component existed.
func (r Release) FleetIntent() (*fleet.Document, bool, error) {
	component := r.Component(fleet.Component)
	if component == nil {
		return nil, false, nil
	}
	document, err := fleet.Parse(component)
	if err != nil {
		return nil, true, err
	}
	return document, true, nil
}

// preserveAuthoredFleetIntent puts the authored fleet document back exactly as
// the published release carried it.
//
// Capture reads the controller's own machine. For every other component that is
// the point; for this one it would be a catastrophe, because one host's local
// state would silently become fleet-wide authorization. Capture has already
// moved a reviewed pin to the controller's newer checkout once, so the rule is
// enforced here rather than left to the absence of code that would break it:
// the authored document is restored byte-for-byte, and a document that was
// absent stays absent.
func preserveAuthoredFleetIntent(components map[string]any, authored any, present bool) error {
	if !present {
		delete(components, fleet.Component)
		return nil
	}
	encoded, err := canonjson.Compact(authored)
	if err != nil {
		return failure.Capture("cannot preserve the authored fleet intent: %v", err)
	}
	restored, err := canonjson.Decode(encoded)
	if err != nil {
		return failure.Capture("cannot preserve the authored fleet intent: %v", err)
	}
	components[fleet.Component] = restored
	return nil
}

// RefuseAuthoredCapture rejects a capture that names a component nothing
// observes.
//
// `push --components steward-fleet-configuration` looks like it captures the
// fleet intent and cannot: the document is authored by hand, reviewed as a
// diff, and deliberately never inferred from a host. Accepting the selection
// and doing nothing would exit 0 and read as success, which is worse than not
// offering it, so the selection is refused and says where the edit belongs.
func RefuseAuthoredCapture(only []string) error {
	for _, name := range only {
		if name == fleet.Component {
			return failure.Capture(
				"%s is authored, not captured; edit it in %s and review the diff",
				fleet.Component, File)
		}
	}
	return nil
}
