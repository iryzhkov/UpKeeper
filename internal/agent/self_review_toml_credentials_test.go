package agent

import (
	"github.com/iryzhkov/UpKeeper/internal/prereq"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"os"
	"path/filepath"
	"testing"
)

func TestRound1SelfReviewTOMLCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		want         bool
	}{
		{"multiline", "api_key = \"\"\"\nsecret\n\"\"\"\n", true},
		{"bare", "api_key = secret\n", false},
		{"non-string", "api_key = 123\n", false},
		{"trailing-junk", "api_key = \"secret\" garbage\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.toml")
			if e := os.WriteFile(p, []byte(tc.config), 0600); e != nil {
				t.Fatal(e)
			}
			if got := configHasKey(&Context{}, p); got != tc.want {
				t.Fatalf("configHasKey=%v want=%v config=%q", got, tc.want, tc.config)
			}
		})
	}
}

func TestRound1SelfReviewValidMultilineCredentialAllowsTooling(t *testing.T) {
	r := &adapterTestRunner{handle: func(command procx.Command) procx.Result { return procx.Result{Code: 1} }}
	c := adapterTestContext(t, r)
	c.Env = []string{"PATH=" + freshTestTools(t, c.Home, "secret-tool")}
	adapterTestWrite(t, c.Path(".config/ov-mcp/config.toml"), "api_key = \"\"\"\nsecret\n\"\"\"\n")
	old := componentPrerequisites
	componentPrerequisites = func(string) []prereq.Prerequisite { return nil }
	t.Cleanup(func() { componentPrerequisites = old })
	if block := requirementsBlock(c, "agent-tooling", nil, &[]string{}); block != "" {
		t.Fatalf("valid per-host TOML credential incorrectly blocked: %s", block)
	}
}
