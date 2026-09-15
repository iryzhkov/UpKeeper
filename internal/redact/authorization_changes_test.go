package redact_test

import (
	"reflect"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/redact"
)

func TestAuthorizationChangesRemainReviewableWithoutBypassingSecretRedaction(t *testing.T) {
	changes := []string{
		"worker homelab adds model claudeAgent/claude-opus-4-5",
		"worker homelab executor slots 1 -> 2",
		"unexpected token=fixture-secret",
		"Authorization: Bearer fixture-credential",
		"ghp_abcdefghijklmnopqrstuvwxyz0123",
	}
	got := redact.Value(map[string]any{
		"authorization_changes": changes,
		"authorization":         "Bearer do-not-print",
		"authorization_token":   "do-not-print",
	})
	want := map[string]any{
		"authorization_changes": []any{
			changes[0], changes[1], "unexpected token=<redacted>",
			"Authorization: Bearer <redacted>", "<redacted>",
		},
		"authorization":       "<redacted>",
		"authorization_token": "<redacted>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewable diff and secret filtering differ: got %#v, want %#v", got, want)
	}
}

func TestAuthorizationChangesUnexpectedShapesFailClosed(t *testing.T) {
	for _, value := range []any{
		"secret-scalar", map[string]any{"value": "secret"},
		[]any{"reviewable", map[string]any{"Authorization": "secret"}},
		[]any{"reviewable", 3},
	} {
		if got := redact.WithKey(value, "authorization_changes"); got != redact.Placeholder {
			t.Fatalf("unexpected diff shape was not redacted: %#v", got)
		}
	}
	got := redact.WithKey([]any{"worker homelab removes model codex/example"}, "authorization_changes")
	want := []any{"worker homelab removes model codex/example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded JSON string list is not reviewable: %#v", got)
	}
}
