package publish_test

import (
	"os"
	"path/filepath"
	"testing"
)

// The publication fixtures must remain independent of an operator's obsolete
// Steward config while preserving all original publication assertions.
func TestRound1ReviewAuthoredPublicationIgnoresOperatorConfig(t *testing.T) {
	for name, test := range map[string]func(*testing.T){
		"commits inventory":        TestAuthoredPublicationCommitsTheInventoryTheReleaseNeeds,
		"refuses unknown host":     TestAuthoredPublicationRefusesAReleaseTheInventoryDoesNotKnow,
		"preserves unrelated edit": TestAuthoredPublicationLeavesAnUnrelatedInventoryEditAlone,
	} {
		t.Run(name, func(t *testing.T) {
			config := t.TempDir()
			dir := filepath.Join(config, "t3-steward")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("legacy_file_intake_enabled: true\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_CONFIG_HOME", config)
			t.Setenv("HOME", config)
			test(t)
		})
	}
}
