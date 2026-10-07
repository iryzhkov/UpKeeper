package redact

import (
	"strconv"
	"strings"
	"testing"
)

func TestRound3QuotedSubstitutionBoundaries(t *testing.T) {
	for name, word := range map[string]string{
		"command":                 `"prefix $(printf "%s" "alpha FAKE_SECRET_SUFFIX omega")"`,
		"nested command":          `"prefix $(printf "%s" "$(printf "%s" "alpha FAKE_SECRET_SUFFIX omega")")"`,
		"parameter":               `"prefix ${unset:-"alpha FAKE_SECRET_SUFFIX omega"}"`,
		"backtick":                "\"prefix `printf \"%s\" \"alpha FAKE_SECRET_SUFFIX omega\"`\"",
		"command inside backtick": "\"prefix `printf \"%s\" \"$(printf \"%s\" \"alpha FAKE_SECRET_SUFFIX omega\")\"`\"",
		"escaped substitution":    `"prefix \$(literal \" alpha FAKE_SECRET_SUFFIX omega)"`,
		"escaped inner quote":     `"prefix $(printf "%s" "alpha \" FAKE_SECRET_SUFFIX omega")"`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, prefix := range []string{"OV_API_KEY=", "ov --password ", "Authorization: Bearer "} {
				got := DiagnosticText(prefix + word + " retry connection")
				if strings.Contains(got, "FAKE_SECRET_SUFFIX") || strings.Contains(got, "omega") || !strings.HasSuffix(got, " retry connection") {
					t.Fatalf("%q -> %q", word, got)
				}
			}
		})
	}
	for _, word := range []string{
		`"prefix $(printf "%s" "alpha FAKE_SECRET_SUFFIX omega"`,
		`"prefix $(printf "%s" "alpha") FAKE_SECRET_SUFFIX omega`,
		"\"prefix `printf \"%s\" \"alpha FAKE_SECRET_SUFFIX omega\"",
		`"prefix ${unset:-"alpha"} ) FAKE_SECRET_SUFFIX omega`,
		`"prefix $(alpha } FAKE_SECRET_SUFFIX omega)"`,
	} {
		got := DiagnosticText("OV_API_KEY=" + word + " retry connection")
		if got != "OV_API_KEY="+Placeholder {
			t.Fatalf("uncertain boundary must fail closed: %q -> %q", word, got)
		}
	}
}

// Ordinary tests run these seeds; generated shell quoting keeps every padding
// string inside the credential while varying inner quotes and nesting.
func FuzzDiagnosticQuotedSubstitutions(f *testing.F) {
	for _, padding := range []string{"alpha", "\"", "'", "\\", "$(printf x)", "`printf x`", "\n", "☃", ",;}]"} {
		f.Add(padding, uint8(2))
	}
	f.Fuzz(func(t *testing.T, padding string, depth uint8) {
		if len(padding) > 1024 {
			t.Skip()
		}
		quoted := "'" + strings.ReplaceAll(padding+" FAKE_SECRET_SUFFIX omega", "'", `'\''`) + "'"
		word := "$(printf \"%s\" " + quoted + ")"
		for i := 0; i < int(depth%8); i++ {
			word = "$(printf \"%s\" \"" + word + "\")"
		}
		for _, prefix := range []string{"OV_API_KEY=", "--password ", "Authorization: Bearer "} {
			input := prefix + "\"prefix " + word + "\"" + " retry connection"
			got := DiagnosticText(input)
			if strings.Contains(got, "FAKE_SECRET_SUFFIX") || strings.Contains(got, "omega") || !strings.HasSuffix(got, " retry connection") {
				t.Fatalf("%s -> %q", strconv.Quote(input), got)
			}
		}
	})
}
