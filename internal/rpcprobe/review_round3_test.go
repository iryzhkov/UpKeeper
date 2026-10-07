package rpcprobe

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRound3QuotedSubstitutionExchange(t *testing.T) {
	const word = `"prefix $(printf "%s" "alpha FAKE_SECRET_SUFFIX omega")"`
	c := exec.Command("bash", "-c", "value="+word+"; printf '%s' \"$value\"")
	c.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := c.CombinedOutput()
	if err != nil || string(out) != "prefix alpha FAKE_SECRET_SUFFIX omega" {
		t.Fatalf("shell proof: %q %v", out, err)
	}
	for _, prefix := range []string{"OV_API_KEY=", "ov --password ", "Authorization: Bearer "} {
		t.Run(prefix, func(t *testing.T) {
			input := prefix + word + " failed"
			got := exchangeToolError(t, []any{text(input)}).Error()
			if strings.Contains(got, "FAKE_SECRET_SUFFIX") || strings.Contains(got, "omega") {
				t.Fatalf("%q -> %q", input, got)
			}
			if !strings.HasSuffix(got, " failed") {
				t.Fatalf("lost useful suffix: %q", got)
			}
		})
	}
}
