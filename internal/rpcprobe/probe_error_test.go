package rpcprobe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeServer writes an MCP server that answers initialize and tools/list and
// then answers the one tools/call with result.
func fakeServer(t *testing.T, result map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	answer, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "answer"), append(answer, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"read -r line\necho '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}'\n" +
		"read -r line\nread -r line\necho '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[{\"name\":\"memory_status\"}]}}'\n" +
		"read -r line\ncat \"$(dirname \"$0\")/answer\"\n"
	path := filepath.Join(dir, "server")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func exchangeToolError(t *testing.T, content []any) error {
	t.Helper()
	result := map[string]any{"isError": true}
	if content != nil {
		result["content"] = content
	}
	calls := []Call{
		{Method: "tools/list", Params: map[string]any{}},
		{Method: "tools/call", Params: map[string]any{"name": "memory_status", "arguments": map[string]any{}}},
	}
	_, err := Exchange([]string{fakeServer(t, result)}, os.Environ(), t.TempDir(), false, calls, 20*time.Second)
	if err == nil {
		t.Fatal("a tool error was accepted")
	}
	return err
}

func text(value string) map[string]any { return map[string]any{"type": "text", "text": value} }

func TestToolErrorCarriesTheToolsOwnText(t *testing.T) {
	err := exchangeToolError(t, []any{
		text("OpenViking API key not found"),
		map[string]any{"type": "image", "data": "aW1hZ2U="},
		text("set OV_API_KEY or create ~/.config/ov-mcp/key"),
	})
	want := "tools/call memory_status failed: OpenViking API key not found; set OV_API_KEY or create ~/.config/ov-mcp/key"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestToolErrorWithoutTextSaysSo(t *testing.T) {
	for name, content := range map[string][]any{
		"absent": nil, "empty": {}, "blank": {text("  \n")}, "not text": {map[string]any{"type": "image"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := exchangeToolError(t, content)
			if want := "tools/call memory_status failed: no error text"; err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
		})
	}
}

func TestToolErrorTextIsBounded(t *testing.T) {
	err := exchangeToolError(t, []any{text(strings.Repeat("é", 2000))})
	detail, found := strings.CutPrefix(err.Error(), "tools/call memory_status failed: ")
	if !found {
		t.Fatalf("error = %q", err)
	}
	if len(detail) > ToolErrorLimit+len("...") || !strings.HasSuffix(detail, "...") {
		t.Fatalf("%d bytes of tool error text: %q", len(detail), detail)
	}
	if !strings.HasPrefix(detail, "éé") || strings.ContainsRune(detail, '�') {
		t.Fatalf("the bound split a character: %q", detail)
	}
}

func TestToolErrorTextIsRedactedAndPrintable(t *testing.T) {
	token := "sk-" + strings.Repeat("A1b2", 8)
	err := exchangeToolError(t, []any{text("upstream refused \x1b[31mtoken " + token + "\x1b[0m\r\nretry\x00 later")})
	message := err.Error()
	if strings.Contains(message, token) || strings.Contains(message, "A1b2A1b2") {
		t.Fatalf("the token leaked: %q", message)
	}
	if !strings.Contains(message, "<redacted>") {
		t.Fatalf("no redaction marker: %q", message)
	}
	for _, r := range message {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("control character %q in %q", r, message)
		}
	}
	if !strings.Contains(message, "retry later") && !strings.Contains(message, "retry  later") {
		t.Fatalf("the text around the control characters was lost: %q", message)
	}
}

// A control character between two halves of a token must not join them into
// a token the redaction no longer sees, nor hide one from it.
func TestToolErrorControlCharactersCannotSmuggleAToken(t *testing.T) {
	err := exchangeToolError(t, []any{text("key sk-\x00" + strings.Repeat("Zq9", 10))})
	if strings.Contains(err.Error(), "sk-"+strings.Repeat("Zq9", 10)) {
		t.Fatalf("a smuggled token leaked: %q", err)
	}
}
