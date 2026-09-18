package release

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// ProjectSpec is one Steward project as `upkeeper project add` authors it.
// It carries exactly the fields the fleet contract's `projects` entry has;
// there is nothing else to author, because the coordinator supplies every
// host-local binding itself.
type ProjectSpec struct {
	Name            string
	Repository      string
	DefaultRef      string
	SetupProfile    string
	EligibleWorkers []string
}

// AddProject adds one project to the fleet intent of the checkout's release
// and writes the manifest back. It is the one place a project entry is
// authored by a program rather than by hand, and it goes through the same
// model as everything else that reads the manifest: the release is loaded and
// validated, the edited component is parsed by fleet.Parse before a byte is
// written, and the bytes are the canonical encoding every other writer
// produces, so the file's identity is exactly the digest a capture would
// have published for the same content.
//
// Nothing is committed, pushed or pulled. The caller reviews the diff.
func AddProject(root string, spec ProjectSpec) (Release, error) {
	value, document, err := loadFleetForEdit(root)
	if err != nil {
		return Release{}, err
	}
	if _, exists := document.Projects[spec.Name]; exists {
		return Release{}, failure.Validation(
			"project %s is already in %s; remove it first or edit it by hand", spec.Name, File)
	}
	if len(spec.EligibleWorkers) == 0 {
		return Release{}, failure.Validation("project %s needs at least one eligible worker", spec.Name)
	}
	// Unknown workers are refused by name before the parser sees the document,
	// because the parser's refusal is written for a document and this one is
	// written for an operator who typed a host name.
	for _, worker := range spec.EligibleWorkers {
		if _, known := document.Hosts[worker]; !known {
			return Release{}, failure.Validation(
				"worker %s is not a fleet host; the fleet intent declares %s",
				worker, strings.Join(document.HostNames(), ", "))
		}
	}
	workers := make([]any, 0, len(spec.EligibleWorkers))
	for _, worker := range sortedUniqueStrings(spec.EligibleWorkers) {
		workers = append(workers, worker)
	}
	component := value.Component(fleet.Component)
	projects, _ := component["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		component["projects"] = projects
	}
	projects[spec.Name] = map[string]any{
		"repository":       spec.Repository,
		"default_ref":      spec.DefaultRef,
		"setup_profile":    spec.SetupProfile,
		"eligible_workers": workers,
	}
	return writeEditedFleet(root, value)
}

// RemoveProject removes one project from the fleet intent of the checkout's
// release and writes the manifest back. Removing a project revokes it for
// every worker it was eligible for, which the plan diff will say.
func RemoveProject(root, name string) (Release, error) {
	value, document, err := loadFleetForEdit(root)
	if err != nil {
		return Release{}, err
	}
	if _, exists := document.Projects[name]; !exists {
		return Release{}, failure.Validation(
			"project %s is not in %s; the fleet intent declares %s",
			name, File, strings.Join(document.ProjectNames(), ", "))
	}
	projects, _ := value.Component(fleet.Component)["projects"].(map[string]any)
	delete(projects, name)
	return writeEditedFleet(root, value)
}

// loadFleetForEdit loads the release and insists it carries fleet intent: a
// release without the component has nowhere to put a project.
func loadFleetForEdit(root string) (Release, *fleet.Document, error) {
	value, err := Load(root)
	if err != nil {
		return Release{}, nil, err
	}
	document, present, err := value.FleetIntent()
	if err != nil {
		return Release{}, nil, err
	}
	if !present {
		return Release{}, nil, failure.Validation(
			"this release carries no %s component, so there is no fleet intent to add a project to",
			fleet.Component)
	}
	return value, document, nil
}

// writeEditedFleet validates the edited manifest exactly the way Load will
// read it back, then replaces the file atomically: the canonical bytes go to
// a temporary file beside the manifest and are renamed over it, so a crash
// leaves either the old release or the new one and never a torn file.
func writeEditedFleet(root string, value Release) (Release, error) {
	validated, err := Validate(value.Manifest)
	if err != nil {
		return Release{}, err
	}
	payload, err := validated.CanonicalBytes()
	if err != nil {
		return Release{}, err
	}
	// Re-read the canonical bytes so that the returned release is the same
	// generic tree Load produces from the file, number literals included.
	reread, err := canonjson.DecodeObject(payload)
	if err != nil {
		return Release{}, err
	}
	if _, err := Validate(reread); err != nil {
		return Release{}, err
	}
	path := filepath.Join(root, File)
	temporary := filepath.Join(filepath.Dir(path), ".release.json.tmp")
	if err := os.WriteFile(temporary, payload, 0o644); err != nil {
		return Release{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return Release{}, err
	}
	return Release{Manifest: reread}, nil
}

func sortedUniqueStrings(values []string) []string {
	seen := map[string]bool{}
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			unique = append(unique, value)
		}
	}
	sort.Strings(unique)
	return unique
}
