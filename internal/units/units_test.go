package units_test

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/units"
)

// The embedded unit text is what a host with only the released binary installs,
// so it may never reach back into a checkout for an interpreter.
func TestEveryEmbeddedUnitRunsTheBinaryAndNoInterpreter(t *testing.T) {
	for _, group := range [][]units.Definition{units.SelfTimer(), units.FleetTimer()} {
		for _, definition := range group {
			if !strings.HasSuffix(definition.Name, ".service") {
				continue
			}
			if !strings.Contains(definition.Content, "ExecStart=%h/"+units.BinaryPath+" ") {
				t.Errorf("%s does not start the upkeeper binary:\n%s",
					definition.Name, definition.Content)
			}
			for _, forbidden := range []string{"python", "/usr/bin/uv", "ansible"} {
				if strings.Contains(definition.Content, forbidden) {
					t.Errorf("%s still mentions %q", definition.Name, forbidden)
				}
			}
			if !strings.Contains(definition.Content, "UMask=0077") {
				t.Errorf("%s does not keep its output private", definition.Name)
			}
		}
	}
}

func TestTheTimersKeepTheirSchedule(t *testing.T) {
	schedules := map[string]string{
		"upkeeper-self.timer": "OnCalendar=*-*-* 09:00",
		"upkeeper-pull.timer": "OnCalendar=*-*-* 06:30",
	}
	for _, group := range [][]units.Definition{units.SelfTimer(), units.FleetTimer()} {
		for _, definition := range group {
			expected, watched := schedules[definition.Name]
			if !watched {
				continue
			}
			if !strings.Contains(definition.Content, expected) {
				t.Errorf("%s no longer runs at its documented time", definition.Name)
			}
			if !strings.Contains(definition.Content, "Persistent=true") {
				t.Errorf("%s would skip a run the machine slept through", definition.Name)
			}
		}
	}
}
