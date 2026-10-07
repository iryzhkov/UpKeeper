package redact

import (
	"strings"
	"testing"
)

// Ordinary runs execute the seeds; fuzzing varies delimiters inside a complete
// quoted word and each supported way of joining shell word segments.
func FuzzDiagnosticShellWords(f *testing.F) {
	for _, padding := range []string{"alpha", "'", `\\`, `"`, "\n", "☃", ",;}]"} {
		f.Add(padding)
	}
	f.Add(`\'00`) // Minimized fuzz reproduction: literal slash beside a POSIX apostrophe.
	f.Fuzz(func(t *testing.T, padding string) {
		if len(padding) > 1024 {
			t.Skip()
		}
		const marker = "FAKE_SECRET_SUFFIX"
		// POSIX single quotes represent a literal apostrophe by ending the quote,
		// escaping it outside quotes and beginning a new quoted segment.
		quoted := "'" + strings.ReplaceAll(padding+" "+marker+" omega", "'", `'\''`) + "'"
		for _, key := range []string{"--password ", "OV_API_KEY=", "Authorization: Bearer "} {
			input := "failed: " + key + quoted + " retry connection"
			got := DiagnosticText(input)
			if strings.Contains(got, marker) || strings.Contains(got, "omega") || !strings.HasSuffix(got, " retry connection") {
				t.Fatalf("incomplete shell boundary: %q -> %q", input, got)
			}
		}
	})
}
