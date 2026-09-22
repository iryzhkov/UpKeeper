package release

import (
	"reflect"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// ProjectUpdate changes only fields explicitly supplied by the caller.
type ProjectUpdate struct {
	Repository      *string
	DefaultRef      *string
	SetupProfile    *string
	EligibleWorkers *[]string
}

// UpdateProject applies a digest-fenced, field-preserving project edit.
// Replaying an already-applied update returns the unchanged release bytes.
func UpdateProject(root, name, expectedDigest string, update ProjectUpdate) (Release, error) {
	value, document, err := loadFleetForEdit(root)
	if err != nil {
		return Release{}, err
	}
	if expectedDigest == "" || value.Digest() != expectedDigest {
		return Release{}, failure.Validation("release digest changed: got %s, want %s", value.Digest(), expectedDigest)
	}
	if _, exists := document.Projects[name]; !exists {
		return Release{}, failure.Validation(
			"project %s is not in %s; the fleet intent declares %s",
			name, File, strings.Join(document.ProjectNames(), ", "))
	}
	projects, _ := value.Component(fleet.Component)["projects"].(map[string]any)
	current, _ := projects[name].(map[string]any)
	if current == nil {
		return Release{}, failure.Validation("project %s has invalid release representation", name)
	}
	before := make(map[string]any, len(current))
	for key, field := range current {
		before[key] = field
	}
	if update.Repository != nil {
		current["repository"] = *update.Repository
	}
	if update.DefaultRef != nil {
		current["default_ref"] = *update.DefaultRef
	}
	if update.SetupProfile != nil {
		current["setup_profile"] = *update.SetupProfile
	}
	if update.EligibleWorkers != nil {
		if len(*update.EligibleWorkers) == 0 {
			return Release{}, failure.Validation("project %s needs at least one eligible worker", name)
		}
		workers := make([]any, 0, len(*update.EligibleWorkers))
		for _, worker := range sortedUniqueStrings(*update.EligibleWorkers) {
			if _, known := document.Hosts[worker]; !known {
				return Release{}, failure.Validation("worker %s is not a fleet host", worker)
			}
			workers = append(workers, worker)
		}
		current["eligible_workers"] = workers
	}
	if reflect.DeepEqual(before, current) {
		return value, nil
	}
	return writeEditedFleet(root, value)
}
