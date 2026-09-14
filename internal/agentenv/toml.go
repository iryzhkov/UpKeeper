// This file is a span-preserving TOML editor, not a general TOML parser.
//
// UpKeeper owns exactly one part of ~/.codex/config.toml: the
// [mcp_servers.<name>] tables it registers. Everything else in that file --
// project trust levels, provider settings, a comment the operator wrote -- is
// the host's and must survive a convergence byte for byte. The editor therefore
// works on the file's text: it finds the header lines of the tables it owns,
// drops exactly those spans, and appends freshly rendered tables. No other byte
// is ever rewritten, so no formatting choice of the host's is ever "normalized"
// away.
//
// A reader is still needed, because the projection has to know what is already
// registered before it can merge, and because the rendered result is parsed
// again and compared with the intended registrations before it is written. That
// reader understands a deliberately narrow subset of TOML:
//
//   - comments, blank lines and table headers, with bare or basic-quoted keys;
//   - key/value pairs, including dotted keys;
//   - basic strings, literal strings, integers, floats, booleans, arrays and
//     inline tables.
//
// It refuses, with an error rather than a guess: arrays of tables ([[name]]),
// multi-line strings (""" and ”'), date and time values, and any other syntax
// it does not recognise. Refusing is the safe outcome: the projection fails
// closed and the file is left exactly as it was, rather than being rewritten
// from a partial understanding of its contents.
//
// Go's standard library has no TOML support and this repository takes no
// third-party dependencies, which is why this subset exists at all.

package agentenv

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// Table is an ordered TOML table.
//
// The order is part of the contract: a rendered registration keeps the key
// order the file already had, so that a converged host's file does not churn
// on every run.
type Table struct {
	keys   []string
	values map[string]any
}

// NewTable returns an empty ordered table.
func NewTable() *Table {
	return &Table{values: map[string]any{}}
}

// Set stores a value, keeping the position a key already had.
func (t *Table) Set(key string, value any) {
	if _, present := t.values[key]; !present {
		t.keys = append(t.keys, key)
	}
	t.values[key] = value
}

// Get returns a value and whether the key is present.
func (t *Table) Get(key string) (any, bool) {
	if t == nil {
		return nil, false
	}
	value, present := t.values[key]
	return value, present
}

// Child returns a nested table, or nil when the key is absent or holds
// something other than a table.
func (t *Table) Child(key string) *Table {
	value, present := t.Get(key)
	if !present {
		return nil
	}
	child, ok := value.(*Table)
	if !ok {
		return nil
	}
	return child
}

// Keys returns the keys in file order.
func (t *Table) Keys() []string {
	if t == nil {
		return nil
	}
	return t.keys
}

// Len returns the number of keys.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.keys)
}

// Generic converts the table into the plain maps the JSON encoders understand.
func (t *Table) Generic() map[string]any {
	if t == nil {
		return map[string]any{}
	}
	converted := make(map[string]any, len(t.keys))
	for _, key := range t.keys {
		converted[key] = genericValue(t.values[key])
	}
	return converted
}

func genericValue(value any) any {
	switch typed := value.(type) {
	case *Table:
		return typed.Generic()
	case []any:
		converted := make([]any, len(typed))
		for index, item := range typed {
			converted[index] = genericValue(item)
		}
		return converted
	default:
		return value
	}
}

// ParseTOML reads the supported subset of TOML and refuses everything else.
func ParseTOML(text string) (*Table, error) {
	parser := &tomlParser{text: text}
	return parser.parse()
}

type tomlParser struct {
	text   string
	cursor int
	// defined records the table headers already seen, so that a file which
	// declares the same table twice is refused instead of silently merged.
	defined map[string]bool
}

func (p *tomlParser) parse() (*Table, error) {
	p.defined = map[string]bool{}
	root := NewTable()
	current := root
	for {
		p.skipIgnorable()
		if p.cursor >= len(p.text) {
			return root, nil
		}
		if p.text[p.cursor] == '[' {
			table, err := p.parseHeader(root)
			if err != nil {
				return nil, err
			}
			current = table
			continue
		}
		if err := p.parsePair(current); err != nil {
			return nil, err
		}
	}
}

// skipIgnorable advances past whitespace, newlines and comments.
func (p *tomlParser) skipIgnorable() {
	for p.cursor < len(p.text) {
		character := p.text[p.cursor]
		switch {
		case character == ' ' || character == '\t' || character == '\r' || character == '\n':
			p.cursor++
		case character == '#':
			for p.cursor < len(p.text) && p.text[p.cursor] != '\n' {
				p.cursor++
			}
		default:
			return
		}
	}
}

func (p *tomlParser) skipSpaces() {
	for p.cursor < len(p.text) &&
		(p.text[p.cursor] == ' ' || p.text[p.cursor] == '\t' || p.text[p.cursor] == '\r') {
		p.cursor++
	}
}

func (p *tomlParser) parseHeader(root *Table) (*Table, error) {
	p.cursor++
	if p.cursor < len(p.text) && p.text[p.cursor] == '[' {
		return nil, fmt.Errorf("TOML arrays of tables are not supported")
	}
	path, err := p.parseKeyPath()
	if err != nil {
		return nil, err
	}
	p.skipSpaces()
	if p.cursor >= len(p.text) || p.text[p.cursor] != ']' {
		return nil, fmt.Errorf("unterminated TOML table header")
	}
	p.cursor++
	if err := p.endOfLine(); err != nil {
		return nil, err
	}
	joined := strings.Join(path, "\x00")
	if p.defined[joined] {
		return nil, fmt.Errorf("TOML table %s is declared more than once", strings.Join(path, "."))
	}
	p.defined[joined] = true
	return descend(root, path)
}

func (p *tomlParser) parsePair(current *Table) error {
	path, err := p.parseKeyPath()
	if err != nil {
		return err
	}
	p.skipSpaces()
	if p.cursor >= len(p.text) || p.text[p.cursor] != '=' {
		return fmt.Errorf("expected = after a TOML key")
	}
	p.cursor++
	p.skipSpaces()
	value, err := p.parseValue()
	if err != nil {
		return err
	}
	if err := p.endOfLine(); err != nil {
		return err
	}
	target := current
	if len(path) > 1 {
		if target, err = descend(current, path[:len(path)-1]); err != nil {
			return err
		}
	}
	key := path[len(path)-1]
	if _, present := target.Get(key); present {
		return fmt.Errorf("TOML key %s is defined more than once", key)
	}
	target.Set(key, value)
	return nil
}

// endOfLine insists that nothing but a comment follows a complete statement.
func (p *tomlParser) endOfLine() error {
	p.skipSpaces()
	if p.cursor >= len(p.text) {
		return nil
	}
	if p.text[p.cursor] == '#' {
		for p.cursor < len(p.text) && p.text[p.cursor] != '\n' {
			p.cursor++
		}
		return nil
	}
	if p.text[p.cursor] == '\n' {
		p.cursor++
		return nil
	}
	return fmt.Errorf("unexpected trailing TOML content: %q", p.rest())
}

func (p *tomlParser) rest() string {
	end := strings.IndexByte(p.text[p.cursor:], '\n')
	if end < 0 {
		return p.text[p.cursor:]
	}
	return p.text[p.cursor : p.cursor+end]
}

func (p *tomlParser) parseKeyPath() ([]string, error) {
	var path []string
	for {
		p.skipSpaces()
		key, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		path = append(path, key)
		p.skipSpaces()
		if p.cursor < len(p.text) && p.text[p.cursor] == '.' {
			p.cursor++
			continue
		}
		return path, nil
	}
}

func (p *tomlParser) parseKey() (string, error) {
	if p.cursor >= len(p.text) {
		return "", fmt.Errorf("a TOML key is missing")
	}
	character := p.text[p.cursor]
	if character == '"' || character == '\'' {
		return p.parseString()
	}
	start := p.cursor
	for p.cursor < len(p.text) && isBareKeyByte(p.text[p.cursor]) {
		p.cursor++
	}
	if p.cursor == start {
		return "", fmt.Errorf("a TOML key is missing at %q", p.rest())
	}
	return p.text[start:p.cursor], nil
}

func isBareKeyByte(character byte) bool {
	switch {
	case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z':
		return true
	case character >= '0' && character <= '9':
		return true
	case character == '_' || character == '-':
		return true
	default:
		return false
	}
}

func (p *tomlParser) parseValue() (any, error) {
	if p.cursor >= len(p.text) {
		return nil, fmt.Errorf("a TOML value is missing")
	}
	switch p.text[p.cursor] {
	case '"', '\'':
		return p.parseString()
	case '[':
		return p.parseArray()
	case '{':
		return p.parseInlineTable()
	}
	start := p.cursor
	for p.cursor < len(p.text) && !strings.ContainsRune(",]}#\n", rune(p.text[p.cursor])) {
		p.cursor++
	}
	literal := strings.TrimRight(p.text[start:p.cursor], " \t\r")
	return scalarValue(literal)
}

// scalarValue accepts booleans and numbers only. A date, a time or anything
// else unrecognised is refused so that the editor never rewrites a value it
// does not understand.
func scalarValue(literal string) (any, error) {
	switch literal {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "":
		return nil, fmt.Errorf("a TOML value is missing")
	}
	plain := strings.ReplaceAll(literal, "_", "")
	if whole, err := strconv.ParseInt(plain, 0, 64); err == nil {
		return json.Number(strconv.FormatInt(whole, 10)), nil
	}
	if fractional, err := strconv.ParseFloat(plain, 64); err == nil &&
		!strings.ContainsAny(plain, "xXoObB") {
		return json.Number(strconv.FormatFloat(fractional, 'g', -1, 64)), nil
	}
	return nil, fmt.Errorf("unsupported TOML value: %q", literal)
}

func (p *tomlParser) parseString() (string, error) {
	quote := p.text[p.cursor]
	if strings.HasPrefix(p.text[p.cursor:], strings.Repeat(string(quote), 3)) {
		return "", fmt.Errorf("multi-line TOML strings are not supported")
	}
	p.cursor++
	if quote == '\'' {
		end := strings.IndexByte(p.text[p.cursor:], '\'')
		if end < 0 {
			return "", fmt.Errorf("unterminated TOML literal string")
		}
		value := p.text[p.cursor : p.cursor+end]
		if strings.ContainsRune(value, '\n') {
			return "", fmt.Errorf("unterminated TOML literal string")
		}
		p.cursor += end + 1
		return value, nil
	}
	var out strings.Builder
	for p.cursor < len(p.text) {
		character := p.text[p.cursor]
		switch character {
		case '"':
			p.cursor++
			return out.String(), nil
		case '\n':
			return "", fmt.Errorf("unterminated TOML string")
		case '\\':
			decoded, width, err := unescape(p.text[p.cursor:])
			if err != nil {
				return "", err
			}
			out.WriteString(decoded)
			p.cursor += width
		default:
			out.WriteByte(character)
			p.cursor++
		}
	}
	return "", fmt.Errorf("unterminated TOML string")
}

func unescape(text string) (string, int, error) {
	if len(text) < 2 {
		return "", 0, fmt.Errorf("truncated TOML escape")
	}
	simple := map[byte]string{
		'b': "\b", 't': "\t", 'n': "\n", 'f': "\f", 'r': "\r", '"': "\"", '\\': "\\",
	}
	if decoded, ok := simple[text[1]]; ok {
		return decoded, 2, nil
	}
	width := 0
	switch text[1] {
	case 'u':
		width = 4
	case 'U':
		width = 8
	default:
		return "", 0, fmt.Errorf("unsupported TOML escape: %q", text[:2])
	}
	if len(text) < 2+width {
		return "", 0, fmt.Errorf("truncated TOML escape")
	}
	point, err := strconv.ParseUint(text[2:2+width], 16, 32)
	if err != nil {
		return "", 0, fmt.Errorf("invalid TOML escape: %q", text[:2+width])
	}
	return string(rune(point)), 2 + width, nil
}

func (p *tomlParser) parseArray() ([]any, error) {
	p.cursor++
	items := []any{}
	for {
		p.skipIgnorable()
		if p.cursor >= len(p.text) {
			return nil, fmt.Errorf("unterminated TOML array")
		}
		if p.text[p.cursor] == ']' {
			p.cursor++
			return items, nil
		}
		item, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		p.skipIgnorable()
		if p.cursor < len(p.text) && p.text[p.cursor] == ',' {
			p.cursor++
			continue
		}
		p.skipIgnorable()
		if p.cursor >= len(p.text) || p.text[p.cursor] != ']' {
			return nil, fmt.Errorf("unterminated TOML array")
		}
		p.cursor++
		return items, nil
	}
}

func (p *tomlParser) parseInlineTable() (*Table, error) {
	p.cursor++
	table := NewTable()
	p.skipSpaces()
	if p.cursor < len(p.text) && p.text[p.cursor] == '}' {
		p.cursor++
		return table, nil
	}
	for {
		p.skipSpaces()
		path, err := p.parseKeyPath()
		if err != nil {
			return nil, err
		}
		p.skipSpaces()
		if p.cursor >= len(p.text) || p.text[p.cursor] != '=' {
			return nil, fmt.Errorf("expected = in a TOML inline table")
		}
		p.cursor++
		p.skipSpaces()
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		target := table
		if len(path) > 1 {
			if target, err = descend(table, path[:len(path)-1]); err != nil {
				return nil, err
			}
		}
		target.Set(path[len(path)-1], value)
		p.skipSpaces()
		if p.cursor >= len(p.text) {
			return nil, fmt.Errorf("unterminated TOML inline table")
		}
		if p.text[p.cursor] == ',' {
			p.cursor++
			continue
		}
		if p.text[p.cursor] == '}' {
			p.cursor++
			return table, nil
		}
		return nil, fmt.Errorf("unterminated TOML inline table")
	}
}

func descend(root *Table, path []string) (*Table, error) {
	current := root
	for _, key := range path {
		value, present := current.Get(key)
		if !present {
			child := NewTable()
			current.Set(key, child)
			current = child
			continue
		}
		child, ok := value.(*Table)
		if !ok {
			return nil, fmt.Errorf("TOML key %s is not a table", key)
		}
		current = child
	}
	return current, nil
}

// serverHeader matches the header line of a table this projection owns. It is
// the exact pattern the Python implementation used, so that the same lines are
// claimed and the same lines are left alone.
var serverHeader = regexp.MustCompile(
	`^\s*\[\s*mcp_servers\.(?:"([^"]+)"|([A-Za-z0-9_-]+))(?:\.|\s*\])`)

var anyHeader = regexp.MustCompile(`^\s*\[`)

// ServerEntry is one registration to render, with its keys already in the order
// they must appear in.
type ServerEntry struct {
	Name  string
	Table *Table
}

// ProjectServerTables removes the [mcp_servers.<name>] spans the entries claim
// and appends the entries as freshly rendered tables.
//
// Every other line of text, including comments and unrelated tables, is copied
// through unchanged.
func ProjectServerTables(text string, entries []ServerEntry) (string, error) {
	owned := make(map[string]bool, len(entries))
	for _, entry := range entries {
		owned[entry.Name] = true
	}
	var kept strings.Builder
	skipping := false
	for _, line := range splitKeepEnds(text) {
		if anyHeader.MatchString(line) {
			match := serverHeader.FindStringSubmatch(line)
			skipping = match != nil && owned[firstNonEmpty(match[1], match[2])]
		}
		if !skipping {
			kept.WriteString(line)
		}
	}
	rendered := strings.TrimRightFunc(kept.String(), isPythonSpace) + "\n"
	for _, entry := range entries {
		rendered += "\n[mcp_servers." + canonjson.EscapeASCII(entry.Name) + "]\n"
		for _, key := range entry.Table.Keys() {
			value, _ := entry.Table.Get(key)
			encoded, err := RenderTOMLValue(value)
			if err != nil {
				return "", err
			}
			rendered += canonjson.EscapeASCII(key) + " = " + encoded + "\n"
		}
	}
	return rendered, nil
}

// RenderTOMLValue renders one registration value. Strings are quoted the way
// JSON quotes them, which is a subset of a TOML basic string, and a table is
// rendered inline so that it belongs unambiguously to its registration.
func RenderTOMLValue(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return canonjson.EscapeASCII(typed), nil
	case bool:
		if typed {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		return typed.String(), nil
	case int:
		return strconv.Itoa(typed), nil
	case int64:
		return strconv.FormatInt(typed, 10), nil
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64), nil
	case []any:
		return renderArray(typed)
	case *Table:
		return renderInlineTable(typed.Keys(), typed.Get)
	case map[string]any:
		keys := sortedKeysOf(typed)
		return renderInlineTable(keys, func(key string) (any, bool) {
			item, present := typed[key]
			return item, present
		})
	default:
		return "", fmt.Errorf("unsupported MCP TOML value")
	}
}

func renderArray(items []any) (string, error) {
	rendered := make([]string, 0, len(items))
	for _, item := range items {
		encoded, err := RenderTOMLValue(item)
		if err != nil {
			return "", err
		}
		rendered = append(rendered, encoded)
	}
	return "[" + strings.Join(rendered, ", ") + "]", nil
}

func renderInlineTable(keys []string, get func(string) (any, bool)) (string, error) {
	rendered := make([]string, 0, len(keys))
	for _, key := range keys {
		value, _ := get(key)
		encoded, err := RenderTOMLValue(value)
		if err != nil {
			return "", err
		}
		rendered = append(rendered, canonjson.EscapeASCII(key)+" = "+encoded)
	}
	return "{ " + strings.Join(rendered, ", ") + " }", nil
}

// splitKeepEnds splits text into lines that still carry their terminator, the
// way Python's splitlines(keepends=True) does for ordinary text.
func splitKeepEnds(text string) []string {
	var lines []string
	start := 0
	for index := 0; index < len(text); index++ {
		if text[index] == '\n' {
			lines = append(lines, text[start:index+1])
			start = index + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func isPythonSpace(character rune) bool {
	return strings.ContainsRune(" \t\n\r\v\f\x1c\x1d\x1e\x1f", character) ||
		character == ' '
}

func sortedKeysOf(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
