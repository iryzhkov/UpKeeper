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

func Exchange(argv, env []string, dir string, codex bool, calls []Call, timeout time.Duration) (results []map[string]any, err error) {
	// All returned errors, including OS errors and caller-supplied call names,
	// pass through the shared diagnostic gate. Child stderr stays discarded.
	defer func() {
		if err != nil {
			err = redact.Error(err)
		}
	}()
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
	text = redact.DiagnosticText(text)
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
