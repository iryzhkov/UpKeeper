// Package rpcprobe performs bounded, non-inference JSON-RPC health checks.
// It never sends a thread, turn, write tool, login or authorization request.
package rpcprobe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/redact"
)

type Call struct {
	Method string
	Params any
}

func Exchange(argv, env []string, dir string, codex bool, calls []Call, timeout time.Duration) ([]map[string]any, error) {
	if len(argv) == 0 {
		return nil, errors.New("missing probe command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.WaitDelay = time.Second
	cmd.Dir = dir
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer output.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = input.Close(); _ = output.Close() })
	defer stopClose()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	write := func(v any) error { return json.NewEncoder(input).Encode(v) }
	read := func(id int) (map[string]any, error) {
		for scanner.Scan() {
			var message map[string]any
			if json.Unmarshal(scanner.Bytes(), &message) != nil {
				continue
			}
			if message["id"] != float64(id) {
				continue
			}
			if message["error"] != nil {
				return nil, fmt.Errorf("probe request %d failed", id)
			}
			result, ok := message["result"].(map[string]any)
			if !ok {
				return nil, errors.New("invalid probe response")
			}
			return result, nil
		}
		if ctx.Err() != nil {
			return nil, errors.New("probe timed out")
		}
		return nil, errors.New("probe closed without answering")
	}
	initParams := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "upkeeper", "version": "1"}}
	notification := "notifications/initialized"
	if codex {
		initParams = map[string]any{"clientInfo": map[string]any{"name": "upkeeper", "version": "1"}, "capabilities": map[string]any{}}
		notification = "initialized"
	}
	if err := write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": initParams}); err != nil {
		return nil, err
	}
	if _, err := read(1); err != nil {
		return nil, err
	}
	if err := write(map[string]any{"jsonrpc": "2.0", "method": notification}); err != nil {
		return nil, err
	}
	var results []map[string]any
	for i, c := range calls {
		id := i + 2
		if err := write(map[string]any{"jsonrpc": "2.0", "id": id, "method": c.Method, "params": c.Params}); err != nil {
			return nil, err
		}
		result, err := read(id)
		if err != nil {
			return nil, err
		}
		if failed, _ := result["isError"].(bool); failed {
			return nil, fmt.Errorf("%s failed: %s", callName(c), toolErrorText(result))
		}
		results = append(results, result)
	}
	return results, nil
}

// ToolErrorLimit bounds, in bytes, the tool's own error text a failed call
// reports.
const ToolErrorLimit = 300

// toolRedactLimit bounds, in bytes, the text redaction reads, so that an
// enormous error text costs no more than this to redact.
const toolRedactLimit = 64 << 10

// callName names a call the way an operator would: a tool call by its tool.
func callName(c Call) string {
	if params, ok := c.Params.(map[string]any); ok && c.Method == "tools/call" {
		if name, ok := params["name"].(string); ok && name != "" {
			return c.Method + " " + name
		}
	}
	return c.Method
}

// privateKeyStart finds what is left of a private key block after redaction:
// one the tool cut short has no END marker for the redaction to match.
var privateKeyStart = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY`)

// Secret shapes a tool's error text plausibly carries that the shared
// redaction does not: an environment assignment such as OV_API_KEY=... (the
// shared pattern needs a word boundary before the key word), a quoted value
// with spaces (the shared pattern takes only its first word), a JSON- or
// dict-quoted secret field, and a bearer token without its Authorization
// header. The key words are the shared redaction's secret key words, an
// access key, and a key word written with a space, as in "api key".
const toolSecretWord = `(?:(?:api|access)[\s_-]?key|private[\s_-]?key|token|secret|password|passwd|credential|authorization|cookie)s?`

// toolSecretValue is a secret's value, tried in order: an authorization
// scheme and its credential; a value in escaped double quotes, as inside a
// JSON string; one in double, single (either optionally a Python bytes or raw
// literal), back or curly quotes; a JSON array or object; or a bare word. A
// quoted value, array or object the text ends inside of runs to the end, so a
// tool's own truncation never leaves the rest of a value behind.
const toolSecretValue = `(?:(?:bearer|basic|token)\s+[^\s,;"'\\]+` +
	`|\\"(?:[^"\\]|\\[^"])*(?:\\"|\\?$)` +
	`|[bru]?"(?:[^"\\]|\\.)*(?:"|\\?$)` +
	`|[bru]?'(?:[^'\\]|\\.)*(?:'|\\?$)` +
	`|\x60[^\x60]*(?:\x60|$)` +
	`|“[^”]*(?:”|$)` +
	`|\[[^\]]*(?:\]|$)` +
	`|\{[^}]*(?:\}|$)` +
	`|[^\s,;}\]]+)`

var (
	// An assignment's key may be a longer name around a key word, such as
	// OV_API_KEY or AWS_SECRET_ACCESS_KEY, but not a word such as secret-tool.
	toolSecretAssignment = regexp.MustCompile(
		`(?i)([A-Za-z0-9_-]*` + toolSecretWord + `(?:_[A-Za-z0-9]+)*\s*[=:]\s*)` + toolSecretValue)
	// A JSON or dict key is quoted, or escaped-quoted inside a JSON string.
	toolSecretJSON = regexp.MustCompile(
		`(?i)(\\?["'][A-Za-z0-9_.\s-]*` + toolSecretWord + `[A-Za-z0-9_.-]*\\?["']\s*:\s*)` + toolSecretValue)
	// A command-line flag gives its value after a space.
	toolSecretFlag = regexp.MustCompile(
		`(?i)((?:^|\s)--?[A-Za-z0-9_-]*` + toolSecretWord + `[A-Za-z0-9_-]*\s+)[^\s-]\S*`)
	// A Cookie header carries several pairs.
	toolCookie = regexp.MustCompile(`(?i)(\bcookie\s*[=:]\s*)[^\s,;]+(?:;\s*[^\s,;=]+=[^\s,;]*)*`)
	// Credentials in a URL of any scheme, such as a database's.
	toolURLUserInfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`)
	toolBearer      = regexp.MustCompile(`(?i)(\b(?:bearer|token|basic)\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// privateKeyBlock is the shared redaction's whole private key block.
	privateKeyBlock = regexp.MustCompile(
		`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// redactToolText removes private keys first, so that an assignment matcher
// never takes only a key's BEGIN marker; then the shapes above, before the
// shared helper can consume an opening quote or an authorization scheme and
// hide the rest of a value; then the shared helper and the bare bearer token.
func redactToolText(text string) string {
	text = privateKeyBlock.ReplaceAllString(text, redact.Placeholder)
	if start := privateKeyStart.FindStringIndex(text); start != nil {
		text = text[:start[0]] + redact.Placeholder
	}
	text = toolURLUserInfo.ReplaceAllString(text, "${1}"+redact.Placeholder+"@")
	text = toolCookie.ReplaceAllString(text, "${1}"+redact.Placeholder)
	text = toolSecretJSON.ReplaceAllString(text, `${1}"`+redact.Placeholder+`"`)
	text = toolSecretAssignment.ReplaceAllString(text, "${1}"+redact.Placeholder)
	text = toolSecretFlag.ReplaceAllString(text, "${1}"+redact.Placeholder)
	text = redact.Text(text)
	return toolBearer.ReplaceAllString(text, "${1}"+redact.Placeholder)
}

// hiddenCharacter reports a character that must not reach a report: a control
// character, a format character such as a bidi override or a zero-width
// space, a line or paragraph separator, or the replacement character.
func hiddenCharacter(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) || r == unicode.ReplacementChar
}

// toolErrorText is the text a tool gave with an error result: its text content
// items joined, with format characters removed so that one inside a key word
// cannot split it, other control characters made spaces so that nothing they
// hide or join escapes redaction, secrets redacted, and only then bounded, so
// that the bound never hides a secret's end from the redaction.
func toolErrorText(result map[string]any) string {
	items, _ := result["content"].([]any)
	parts := []string{}
	for _, item := range items {
		object, _ := item.(map[string]any)
		if kind, _ := object["type"].(string); kind != "text" {
			continue
		}
		text, _ := object["text"].(string)
		text = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.In(r, unicode.Cf) {
				return -1
			}
			if hiddenCharacter(r) {
				return ' '
			}
			return r
		}, strings.ToValidUTF8(text, " ")))
		if text == "" {
			continue
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return "no error text"
	}
	// Items are joined by a space, which the redaction patterns read across,
	// so a key and its value sent as two items are still seen together.
	text := strings.Join(parts, " ")
	// Text past the redaction limit is dropped, back to a space, so that a
	// word the cut would split is dropped whole rather than reported in part.
	capped := len(text) > toolRedactLimit
	if capped {
		text = text[:max(strings.LastIndexFunc(text[:toolRedactLimit], unicode.IsSpace), 0)]
	}
	text = redactToolText(text)
	if start := privateKeyStart.FindStringIndex(text); start != nil {
		text = text[:start[0]] + redact.Placeholder
	}
	if capped {
		text += " ..."
	}
	if len(text) <= ToolErrorLimit {
		return text
	}
	cut := ToolErrorLimit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

func Models(argv, env []string, home string) ([]string, error) {
	results, err := Exchange(argv, env, home, true, []Call{{"model/list", map[string]any{"limit": 100, "includeHidden": false}}}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	result := results[0]
	if cursor, _ := result["nextCursor"].(string); cursor != "" {
		return nil, errors.New("model catalog exceeds probe page")
	}
	data, ok := result["data"].([]any)
	if !ok {
		return nil, errors.New("invalid model catalog")
	}
	var names []string
	for _, v := range data {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["model"].(string)
		if name == "" {
			name, _ = m["id"].(string)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("empty model catalog")
	}
	return names, nil
}
