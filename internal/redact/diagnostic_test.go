package redact

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDiagnosticBalancedValuePreservesUsefulSuffix(t *testing.T) {
	for _, input := range []string{
		`{"credential":{"nested":{"value":"alpha"},"second":"FAKE_SECRET_SUFFIX"}} retry connection`,
		`ov --password "alpha FAKE_SECRET_SUFFIX omega" retry connection`,
		`{"credential":[["} ] \" alpha"],{"second":"FAKE_SECRET_SUFFIX"}]} retry connection`,
	} {
		got := DiagnosticText(input)
		if strings.Contains(got, "FAKE_SECRET_SUFFIX") || !strings.HasSuffix(got, " retry connection") {
			t.Fatalf("lost boundary: %q", got)
		}
	}
}

// Run the seed corpus in every ordinary test run. Additional fuzzing can vary
// strings, nesting depth and escape layers without needing a child process.
func FuzzDiagnosticSecretValues(f *testing.F) {
	for _, padding := range []string{"", `"}]\"`, "line\nbreak", "\\", "☃", "'"} {
		f.Add(padding, uint8(3), uint8(2))
	}
	f.Fuzz(func(t *testing.T, padding string, depth, escapes uint8) {
		if len(padding) > 1024 {
			t.Skip()
		}
		const marker = "FAKE_SECRET_SUFFIX"
		var value any = map[string]any{"first": padding, "second": marker}
		for i := 0; i < int(depth%8); i++ {
			value = []any{map[string]any{"nested": value}, marker}
		}
		bytes, err := json.Marshal(map[string]any{"credential": value})
		if err != nil {
			t.Fatal(err)
		}
		input := string(bytes)
		for i := 0; i < int(escapes%5); i++ {
			bytes, _ = json.Marshal(input)
			input = string(bytes)
		}
		got := DiagnosticText("upstream: " + input + " retry")
		if strings.Contains(got, marker) {
			t.Fatalf("secret survived %q: %q", input, got)
		}
		// The quote primitive is also used by flags and assignments.
		quoted, _ := json.Marshal(padding + " " + marker + " tail")
		for _, key := range []string{"--password ", "--api-key ", "OV_API_KEY=", "password="} {
			got = DiagnosticText(fmt.Sprintf("failed: %s%s retry", key, quoted))
			if strings.Contains(got, marker) {
				t.Fatalf("secret survived: %q", got)
			}
		}
	})
}
