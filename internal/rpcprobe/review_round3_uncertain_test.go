package rpcprobe

import (
	"os/exec"
	"strings"
	"testing"
)

// Case patterns and comments can contain ')' without closing a substitution.
// The diagnostic scanner must suppress uncertain grammar rather than expose
// later words from the same credential.
func TestRound3UncertainSubstitutionExchange(t *testing.T) {
	for name, word := range map[string]string{
		"quoted case":    `"prefix $(case x in x) printf "%s" "alpha FAKE_SECRET_SUFFIX omega";; esac)"`,
		"unquoted case":  `$(case x in x) printf "%s" "alpha FAKE_SECRET_SUFFIX omega";; esac)`,
		"continued case": "\"prefix $(ca\\\nse x in x) printf \"%s\" \"alpha FAKE_SECRET_SUFFIX omega\";; esac)\"",
		"comment":        "\"prefix $(printf x # )\n printf \"%s\" \"alpha FAKE_SECRET_SUFFIX omega\")\"",
		"heredoc":        "\"prefix $(cat <<EOF\n)\nalpha FAKE_SECRET_SUFFIX omega\nEOF\n)\"",
	} {
		t.Run(name, func(t *testing.T) {
			c := exec.Command("bash", "-c", "value="+word+"; printf '%s' \"$value\"")
			c.Env = []string{"PATH=/usr/bin:/bin"}
			out, err := c.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "FAKE_SECRET_SUFFIX") {
				t.Fatalf("shell proof: %q %v", out, err)
			}
			for _, prefix := range []string{"OV_API_KEY=", "ov --password ", "Authorization: Bearer "} {
				got := exchangeToolError(t, []any{text(prefix + word + " failed")}).Error()
				if strings.Contains(got, "FAKE_SECRET_SUFFIX") || strings.Contains(got, "omega") {
					t.Fatalf("%q -> %q", prefix+word, got)
				}
			}
		})
	}
}
