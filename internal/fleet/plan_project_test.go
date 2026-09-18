package fleet_test

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// An added project is loaded by the coordinator with default local bindings
// and changes the catalog of every worker it is eligible for, so the plan says
// both, and names the command that re-enrolls each worker on the coordinator
// host.
func TestEnrollmentPlanExplainsAnAddedProject(t *testing.T) {
	previous := parsed(t)
	proposed, err := fleet.Parse(edit(t, func(document map[string]any) {
		object(t, document, "projects")["home-assistant-config"] = map[string]any{
			"repository":       "ssh://git@git.ryzhkov.dev/igor/home-assistant-config.git",
			"default_ref":      "main",
			"setup_profile":    "git-status",
			"eligible_workers": []any{"homelab", "omarchy-pc"},
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := proposed.EnrollmentPlan(previous)
	if err != nil {
		t.Fatal(err)
	}
	changes, _ := plan["authorization_changes"].([]any)
	joined := make([]string, 0, len(changes))
	for _, change := range changes {
		joined = append(joined, change.(string))
	}
	text := strings.Join(joined, "\n")
	for _, expected := range []string{
		"project home-assistant-config is added at ssh://git@git.ryzhkov.dev/igor/home-assistant-config.git",
		"project home-assistant-config will be loaded by the coordinator with default local bindings",
		"t3-steward worker enroll homelab --current-catalog",
		"t3-steward worker enroll omarchy-pc --current-catalog",
		"on the coordinator host",
		// F-3: a coordinator catalog change is refused while a touched worker
		// holds a retained assignment, and the plan says so before anyone pushes.
		fleet.ReloadRefusalAdvice,
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("the plan does not say %q:\n%s", expected, text)
		}
	}
	// An unchanged project says nothing.
	unchanged, err := proposed.EnrollmentPlan(proposed)
	if err != nil {
		t.Fatal(err)
	}
	if rest, _ := unchanged["authorization_changes"].([]any); len(rest) != 0 {
		t.Fatalf("an identical document reports changes: %v", rest)
	}
}

// A worker that becomes eligible for an existing project also has a changed
// catalog, so it is told to re-enroll too.
func TestEnrollmentPlanNamesReenrollmentForANewlyEligibleWorker(t *testing.T) {
	previous, err := fleet.Parse(edit(t, func(document map[string]any) {
		object(t, document, "projects", "upkeeper")["eligible_workers"] = []any{"homelab"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := parsed(t).EnrollmentPlan(previous)
	if err != nil {
		t.Fatal(err)
	}
	changes, _ := plan["authorization_changes"].([]any)
	found := false
	for _, change := range changes {
		found = found || strings.Contains(change.(string), "t3-steward worker enroll omarchy-pc --current-catalog")
	}
	if !found {
		t.Fatalf("the plan does not tell omarchy-pc to re-enroll: %v", changes)
	}
}
