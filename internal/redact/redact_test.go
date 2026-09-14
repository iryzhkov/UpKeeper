package redact_test

import (
	"reflect"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/redact"
)

// Every expectation here was produced by running the Python implementation this
// package replaces. Redaction is a release gate, so a difference is a leak, not
// a formatting detail.
func TestTextMatchesPython(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			"bearer header",
			"Authorization: Bearer abcdefghijklmnop",
			"Authorization: Bearer <redacted>",
		},
		{
			"assignment keeps its key and separator",
			"password=hunter2, other=ok",
			"password=<redacted>, other=ok",
		},
		{
			"url user information",
			"https://user:pw@github.com/x.git",
			"https://<redacted>@github.com/x.git",
		},
		{
			"github token",
			"token ghp_abcdefghijklmnopqrstuvwxyz0123",
			"token <redacted>",
		},
		{
			// The pattern has a word boundary in front of it, so a token-shaped
			// substring inside a longer word is not a token.
			"token shape inside a word is left alone",
			"prefixghp_abcdefghijklmnopqrstuvwxyz0123",
			"prefixghp_abcdefghijklmnopqrstuvwxyz0123",
		},
		{
			"json web token",
			"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0." +
				"dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			"<redacted>",
		},
		{
			"query parameter",
			"?access_token=abc&x=1",
			"?access_token=<redacted>&x=1",
		},
		{
			"private key block including its newlines",
			"-----BEGIN OPENSSH PRIVATE KEY-----\nzzz\n-----END OPENSSH PRIVATE KEY-----",
			"<redacted>",
		},
		{
			"age secret key",
			"AGE-SECRET-KEY-1ABCDEFGHIJKLMNOPQRSTUVWXYZ234567",
			"<redacted>",
		},
		{
			"ordinary text is untouched",
			"nothing to see",
			"nothing to see",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if produced := redact.Text(testCase.input); produced != testCase.expected {
				t.Errorf("produced %q, expected %q", produced, testCase.expected)
			}
		})
	}
}

func TestValueMatchesPython(t *testing.T) {
	document := map[string]any{
		"api_key": "x",
		// secret_refs names secrets rather than holding them, so the list is
		// kept and only its contents are scanned.
		"secret_refs": []any{"a", "ghp_abcdefghijklmnopqrstuvwxyz0123"},
		"nested":      map[string]any{"Cookie": "v", "ok": "fine"},
		"list":        []any{"password=1"},
	}
	expected := map[string]any{
		"api_key":     "<redacted>",
		"secret_refs": []any{"a", "<redacted>"},
		"nested":      map[string]any{"Cookie": "<redacted>", "ok": "fine"},
		"list":        []any{"password=<redacted>"},
	}
	if produced := redact.Value(document); !reflect.DeepEqual(produced, expected) {
		t.Errorf("produced %#v\nexpected %#v", produced, expected)
	}
}

func TestValueLeavesNonStringScalarsAlone(t *testing.T) {
	document := map[string]any{"count": 3, "ok": true, "missing": nil}
	produced, _ := redact.Value(document).(map[string]any)
	if produced["count"] != 3 || produced["ok"] != true || produced["missing"] != nil {
		t.Errorf("scalars were altered: %#v", produced)
	}
}
