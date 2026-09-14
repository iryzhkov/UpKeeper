package redact_test

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/redact"
)

// The reports internal/publish builds carry []string values, not []any, and
// the printed-report gate passes the document to redact.Value as it is. A
// walker that recognised only map[string]any and []any printed those fields
// verbatim, which made redaction depend on how a field was declared rather
// than on what it held.
func TestTypedContainersAreRedacted(t *testing.T) {
	document := map[string]any{
		"summary": []string{"password=hunter2", "clean"},
		"pushed":  []string{"https://user:tok@github.com/x.git"},
		"nested":  map[string]string{"token": "s3cr3t", "note": "password=hunter2"},
		"deep":    map[string][]string{"remotes": {"ssh://user:pw@host/x.git"}},
	}
	rendered := render(t, redact.Value(document))
	for _, leaked := range []string{"hunter2", "s3cr3t", "user:tok@", "user:pw@"} {
		if strings.Contains(rendered, leaked) {
			t.Errorf("a typed container leaked %q: %s", leaked, rendered)
		}
	}
	if !strings.Contains(rendered, "clean") {
		t.Errorf("redaction removed a value that held no secret: %s", rendered)
	}
}

// A key that names a secret is replaced wholesale whatever its container's
// static type is.
func TestSecretKeyInATypedMapIsReplacedWholesale(t *testing.T) {
	produced, ok := redact.Value(map[string]string{"api_key": "anything at all"}).(map[string]any)
	if !ok {
		t.Fatalf("a map[string]string did not come back as a document: %#v", produced)
	}
	if produced["api_key"] != redact.Placeholder {
		t.Errorf("api_key = %#v, want %q", produced["api_key"], redact.Placeholder)
	}
}

// secret_refs holds the names of secrets and must survive whatever its
// container's type is, or the audit trail it exists for is destroyed.
func TestSecretRefsSurviveATypedSlice(t *testing.T) {
	produced, ok := redact.Value(map[string]any{
		"secret_refs": []string{"github-token", "age-key"},
	}).(map[string]any)
	if !ok {
		t.Fatalf("unexpected shape: %#v", produced)
	}
	rendered := render(t, produced)
	for _, kept := range []string{"github-token", "age-key"} {
		if !strings.Contains(rendered, kept) {
			t.Errorf("secret_refs lost %q: %s", kept, rendered)
		}
	}
}

// A []byte is left as it is rather than walked into a list of integers: that
// would change an encoding, not remove a secret.
func TestByteSlicesAreLeftAlone(t *testing.T) {
	produced, ok := redact.Value(map[string]any{"blob": []byte("abc")}).(map[string]any)
	if !ok {
		t.Fatalf("unexpected shape: %#v", produced)
	}
	if _, stillBytes := produced["blob"].([]byte); !stillBytes {
		t.Errorf("blob = %#v, want the original []byte", produced["blob"])
	}
}

func render(t *testing.T, value any) string {
	t.Helper()
	return strings.TrimSpace(sprint(value))
}

func sprint(value any) string {
	var builder strings.Builder
	write(&builder, value)
	return builder.String()
}

func write(builder *strings.Builder, value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			builder.WriteString(key)
			builder.WriteString("=")
			write(builder, item)
			builder.WriteString(" ")
		}
	case []any:
		for _, item := range typed {
			write(builder, item)
			builder.WriteString(" ")
		}
	case string:
		builder.WriteString(typed)
	default:
		builder.WriteString("?")
	}
}
