package rpcprobe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/redact"
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
	want := "tools/call memory_status failed: OpenViking API key not found set OV_API_KEY or create ~/.config/ov-mcp/key"
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

// Self-review lens 4: the text was cut before it was redacted, so a private
// key whose END marker lay past the cut was no longer recognised and its body
// was reported. A key the tool itself cut short is not reported either.
func TestToolErrorTextNeverReportsALongOrUnterminatedPrivateKey(t *testing.T) {
	body := strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\n", 120)
	for name, value := range map[string]string{
		"long":         "failed: -----BEGIN PRIVATE KEY-----\n" + body + "-----END PRIVATE KEY-----\n",
		"unterminated": "failed: -----BEGIN RSA PRIVATE KEY-----\n" + body[:400],
	} {
		t.Run(name, func(t *testing.T) {
			got := toolErrorText(map[string]any{"content": []any{text(value)}})
			if strings.Contains(got, "MIIEvQ") || !strings.HasPrefix(got, "failed: ") {
				t.Fatalf("key material leaked: %q", got)
			}
		})
	}
}

// Self-review lens 5: shapes the shared redaction does not cover, which a
// tool's error text plausibly carries, are redacted here too: the ov-memory
// credential's own variable, JSON-quoted secrets, a bare bearer token, a key
// and its value sent as separate items, and a token pushed past a bound by
// padding. Format characters that could disguise a report are removed.
func TestToolErrorTextRedactsWhatToolsPrint(t *testing.T) {
	const fake = "FAKEfakeFAKE0123456789secret"
	for name, items := range map[string][]string{
		"OV_API_KEY env":     {"startup failed: OV_API_KEY=" + fake + " rejected"},
		"export":             {"export OPENVIKING_TOKEN='" + fake + "'"},
		"JSON api_key":       {`config {"api_key": "` + fake + `"}`},
		"JSON Authorization": {`request headers {"Authorization": "Bearer ` + fake + `"}`},
		"bare Bearer":        {"401 for Bearer " + fake},
		"split items":        {"token=", fake},
		"padded token":       {strings.Repeat(" ", 4780) + "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
	} {
		content := []any{}
		for _, item := range items {
			content = append(content, text(item))
		}
		got := toolErrorText(map[string]any{"content": content})
		if strings.Contains(got, fake) || strings.Contains(got, "ghp_abcdefghijklmnop") {
			t.Errorf("%s leaked: %q", name, got)
		}
	}
	// A bidi override, a line separator, a zero-width space and a BOM.
	bidi, separator, zeroWidth, bom := string(rune(0x202e)), string(rune(0x2028)), string(rune(0x200b)), string(rune(0xfeff))
	got := toolErrorText(map[string]any{"content": []any{text("ok " + bidi + "gnp.exe " + separator + "fake" + zeroWidth + " report" + bom + " line")}})
	if strings.ContainsAny(got, bidi+separator+zeroWidth+bom) || !strings.Contains(got, "gnp.exe") {
		t.Errorf("format characters pass through: %q", got)
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

// Review 1: the shared redaction replaced only the first word of a quoted
// password, consuming the opening quote, so the quote-aware matcher no longer
// saw the rest of the value; and JSON secret fields spelled api-key or
// private_key, which the shared redaction knows, passed through. Each case
// goes through the real Exchange path.
func TestToolErrorTextRedactsQuotedAndHyphenatedSecrets(t *testing.T) {
	for name, input := range map[string]string{
		"quoted password":         `upstream failed: password="alpha FAKE_SECRET_SUFFIX omega"`,
		"single-quoted password":  `upstream failed: password='alpha FAKE_SECRET_SUFFIX omega'`,
		"escaped quoted password": `{"error": "password=\"alpha FAKE_SECRET_SUFFIX omega\""}`,
		"hyphenated JSON key":     `{"api-key": "FAKE_OPAQUE_OV_KEY_1234567890"}`,
		"JSON private key":        `{"private_key": "FAKE_OPAQUE_PRIVATE_VALUE_1234567890"}`,
		"JSON private-key spaced": `{"Private-Key" : "alpha FAKE_OPAQUE_PRIVATE_VALUE omega"}`,
		"single-quoted dict key":  `{'api_key': 'alpha FAKE_OPAQUE_OV_KEY omega'}`,
		"JSON cookie":             `{"cookie": "session=FAKE_COOKIE_VALUE_1234567890"}`,
		"JSON suffixed key":       `{"api_key_value": "alpha FAKE_OPAQUE_OV_KEY omega"}`,
		"assignment api-key":      `api-key="alpha FAKE_OPAQUE_OV_KEY omega"`,
		"assignment private key":  `PRIVATE_KEY: "alpha FAKE_OPAQUE_PRIVATE_VALUE omega"`,
	} {
		t.Run(name, func(t *testing.T) {
			got := exchangeToolError(t, []any{text(input)}).Error()
			if strings.Contains(got, "FAKE_") || strings.Contains(got, "omega") {
				t.Fatalf("secret material reached error: %q", got)
			}
			if !strings.Contains(got, redact.Placeholder) {
				t.Fatalf("no redaction marker: %q", got)
			}
		})
	}
}

// Running the quote-aware matchers first must not stop a private key given as
// an unquoted assignment value from being removed whole.
func TestToolErrorTextRedactsAnAssignedPrivateKey(t *testing.T) {
	body := strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\n", 3)
	for name, value := range map[string]string{
		"terminated":   "failed: private_key: -----BEGIN PRIVATE KEY-----\n" + body + "-----END PRIVATE KEY-----\n retry",
		"unterminated": "failed: private_key= -----BEGIN RSA PRIVATE KEY-----\n" + body,
	} {
		t.Run(name, func(t *testing.T) {
			got := toolErrorText(map[string]any{"content": []any{text(value)}})
			if strings.Contains(got, "MIIEvQ") || !strings.HasPrefix(got, "failed: ") {
				t.Fatalf("key material leaked: %q", got)
			}
		})
	}
}

// Self-review of the fix round: secret shapes the quote-aware matchers still
// missed, and a regression the reordering caused, where the assignment matcher
// took only the scheme of an Authorization header and left its credential.
func TestToolErrorTextRedactsTheShapesTheFixRoundMissed(t *testing.T) {
	for name, input := range map[string]string{
		"authorization bearer header": "upstream 401: Authorization: Bearer FAKEopaqueTOKEN1234567890",
		"authorization basic header":  "request headers Authorization: Basic RkFLRV9VU0VSOkZBS0VfUEFTUw==",
		"escaped JSON key":            `{"error": "{\"api_key\": \"alpha FAKE_SECRET omega\"}"}`,
		"escaped JSON password":       `upstream body: {\"password\": \"alpha FAKE_SECRET omega\"}`,
		"unterminated double quote":   `config error: password="alpha FAKE_SECRET omega`,
		"unterminated single quote":   `config error: password='alpha FAKE_SECRET omega`,
		"unterminated JSON value":     `{"password": "alpha FAKE_SECRET omega`,
		"JSON value ending in escape": `{"password": "alpha FAKE_SECRET \`,
		"escaped single quote":        `password='it\'s FAKE_SECRET omega'`,
		"backtick quote":              "password=`alpha FAKE_SECRET omega`",
		"curly quote":                 "password=“alpha FAKE_SECRET omega”",
		"JSON array":                  `{"api_key": ["FAKE_SECRET_ONE", "FAKE_SECRET_TWO omega"]}`,
		"JSON number":                 `{"token": 98765432109876543210}`,
		"python bytes":                `{'password': b'FAKE_SECRET_BYTES omega'}`,
		"compound key name":           "AWS_SECRET_ACCESS_KEY=FAKEAWSSECRET1234567890",
		"access key":                  `{"access_key": "FAKE_OPAQUE_ACCESS omega"}`,
		"cookie pairs":                "Cookie: a=1; session=FAKESESSIONVALUE123; theme=omega",
		"command line flag":           "ov --api-key FAKE_OPAQUE_OV_KEY_1234567890 failed",
		"spaced JSON key":             `{"api key": "alpha FAKE_SECRET omega"}`,
		"database URL":                "dial postgres://ov:FAKE_DB_PASSWORD@db:5432 failed",
		"zero-width in key":           "pass​word=FAKE_SECRET_ZW",
	} {
		t.Run(name, func(t *testing.T) {
			got := exchangeToolError(t, []any{text(input)}).Error()
			if strings.Contains(got, "FAKE") || strings.Contains(got, "omega") ||
				strings.Contains(got, "98765") || strings.Contains(got, "RkFLRV") {
				t.Fatalf("secret material reached error: %q", got)
			}
		})
	}
}

// Text a tool can print that only looks like a secret's neighbour still
// reaches the operator.
func TestToolErrorTextKeepsWhatIsNotASecret(t *testing.T) {
	for _, input := range []string{
		"token count exceeded 4096",
		"secret-tool not found",
		"secret-tool: cannot find /usr/bin/secret-tool",
		"OV_API_KEY is not set",
		"authorization failed: 401",
		"api key missing in ~/.config/ov-mcp/key",
	} {
		if got := toolErrorText(map[string]any{"content": []any{text(input)}}); got != input {
			t.Errorf("toolErrorText(%q) = %q", input, got)
		}
	}
}

// Redaction reads the whole text before the bound, so an enormous text is cut
// to a size redaction handles quickly first, and never in a way that leaves
// the start of a secret the cut made unrecognisable.
func TestToolErrorTextCapsWhatItRedacts(t *testing.T) {
	huge := strings.Repeat("password=\"", 400000) + " ok"
	start := time.Now()
	toolErrorText(map[string]any{"content": []any{text(huge)}})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("redacting %d bytes took %s", len(huge), elapsed)
	}
	// A private key's body redacts to a placeholder, which would pull a token
	// cut at the cap into the reported bound.
	begin, end := "k -----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY----- "
	token := "ghp_" + strings.Repeat("Q", 36)
	for _, shift := range []int{1, 5, 17, 39} {
		body := strings.Repeat("A", toolRedactLimit-shift-len(begin)-len(end))
		got := toolErrorText(map[string]any{"content": []any{text(begin + body + end + token)}})
		if strings.Contains(got, "ghp_QQ") || strings.Contains(got, "QQQQQQ") {
			t.Fatalf("shift %d: a token cut at the cap leaked: %q", shift, got)
		}
	}
}
