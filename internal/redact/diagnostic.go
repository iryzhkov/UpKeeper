package redact

import (
	"regexp"
	"strconv"
	"strings"
)

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

// Authorization values include a scheme followed by one complete word.
var diagnosticScheme = regexp.MustCompile(`(?i)^(?:bearer|basic|token)\s+`)

var (
	// An assignment's key may be a longer name around a key word, such as
	// OV_API_KEY or AWS_SECRET_ACCESS_KEY, but not a word such as secret-tool.
	toolSecretAssignment = regexp.MustCompile(
		`(?i)([A-Za-z0-9_-]*` + toolSecretWord + `(?:_[A-Za-z0-9]+)*\s*[=:]\s*)`)
	// A JSON or dict key is quoted, or escaped-quoted inside a JSON string.
	toolSecretJSON      = regexp.MustCompile(`\\*["'][A-Za-z0-9_.\s\\-]*\\*["']\s*:\s*`)
	diagnosticSecretKey = regexp.MustCompile(`(?i)^[A-Za-z0-9_.\s-]*` + toolSecretWord + `[A-Za-z0-9_.-]*$`)
	// A command-line flag gives its value after a space.
	toolSecretFlag = regexp.MustCompile(
		`(?i)((?:^|\s)--?[A-Za-z0-9_-]*` + toolSecretWord + `[A-Za-z0-9_-]*\s+)`)
	// A Cookie header carries several pairs.
	toolCookie = regexp.MustCompile(`(?i)(\bcookie\s*[=:]\s*)[^\s,;]+(?:;\s*[^\s,;=]+=[^\s,;]*)*`)
	// Credentials in a URL of any scheme, such as a database's.
	toolURLUserInfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@]+@`)
	toolBearer      = regexp.MustCompile(`(?i)(\b(?:bearer|token|basic)\s+)[A-Za-z0-9._~+/=-]{8,}`)
)

// DiagnosticText redacts untrusted diagnostic strings, including nested or
// truncated structured values. It leaves ordinary operational guidance intact.
// It removes private keys first, so that an assignment matcher
// never takes only a key's BEGIN marker; then the shapes above, before the
// shared helper can consume an opening quote or an authorization scheme and
// hide the rest of a value; then the shared helper and the bare bearer token.
func DiagnosticText(text string) string {
	text = privateKeyPattern.ReplaceAllString(text, Placeholder)
	if start := privateKeyStart.FindStringIndex(text); start != nil {
		text = text[:start[0]] + Placeholder
	}
	text = toolURLUserInfo.ReplaceAllString(text, "${1}"+Placeholder+"@")
	text = replaceDiagnosticValues(text, toolSecretJSON, true)
	text = replaceDiagnosticValues(text, toolSecretAssignment, false)
	text = replaceDiagnosticValues(text, toolSecretFlag, false)
	text = toolCookie.ReplaceAllString(text, "${1}"+Placeholder)
	text = Text(text)
	return toolBearer.ReplaceAllString(text, "${1}"+Placeholder)
}

// Error sanitizes the printable diagnostic while preserving errors.Is/As.
// Callers must print the wrapper, rather than printing its underlying cause.
func Error(err error) error {
	if err == nil {
		return nil
	}
	message := DiagnosticText(err.Error())
	if message == err.Error() {
		return err
	}
	return diagnosticError{cause: err, message: message}
}

type diagnosticError struct {
	cause   error
	message string
}

func (e diagnosticError) Error() string { return e.message }
func (e diagnosticError) Unwrap() error { return e.cause }

// replaceDiagnosticValues locates a sensitive prefix before consuming its
// value. In particular, the opening quote is never lost to a bare-word match.
func replaceDiagnosticValues(value string, prefix *regexp.Regexp, quoted bool) string {
	var out strings.Builder
	for position := 0; position < len(value); {
		found := prefix.FindStringIndex(value[position:])
		if found == nil {
			out.WriteString(value[position:])
			break
		}
		start, end := position+found[0], position+found[1]
		out.WriteString(value[position:end])
		if quoted && !diagnosticJSONSecretKey(value[start:end]) {
			position = end
			continue
		}
		consumed := diagnosticValueEnd(value[end:], !quoted)
		if consumed == 0 {
			position = end
			continue
		}
		if quoted {
			out.WriteByte('"')
		}
		out.WriteString(Placeholder)
		if quoted {
			out.WriteByte('"')
		}
		position = end + consumed
	}
	return out.String()
}

// A JSON key can encode a known sensitive name with legal Unicode escapes.
// Decode its escape layers before classification, while preserving the original
// key spelling in the report. Uncertain encoded names are suppressed.
func diagnosticJSONSecretKey(prefix string) bool {
	key := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(prefix), ":"))
	key = strings.Trim(key, "\\\"'")
	for layer := 0; layer < 8 && strings.Contains(key, "\\"); layer++ {
		decoded, err := strconv.Unquote("\"" + key + "\"")
		if err != nil {
			return true
		}
		key = decoded
	}
	if strings.Contains(key, "\\") {
		return true
	}
	return diagnosticSecretKey.MatchString(key)
}

// diagnosticValueEnd returns the complete value boundary. Escaped diagnostic
// JSON is decoded only in a scratch view; offsets map back to the original.
// An uncertain structured boundary consumes the rest rather than leaking it.
func diagnosticValueEnd(raw string, shell bool) int {
	if raw == "" {
		return 0
	}
	view := raw
	var offsets []int
	originalEnd := func(end int) int {
		if offsets == nil {
			return end
		}
		return offsets[end]
	}
	for layer := 0; layer < 8; layer++ {
		first := strings.IndexByte(view, '"')
		if first <= 0 || view[first-1] != '\\' {
			break
		}
		// Only unwrap an escaped string or an escaped structured value.
		if view[0] != '\\' && view[0] != '{' && view[0] != '[' {
			break
		}
		var decoded strings.Builder
		next := []int{0}
		for i := 0; i < len(view); {
			c := view[i]
			if c == '\\' && i+1 < len(view) && (view[i+1] == '\\' || view[i+1] == '"') {
				i++
				c = view[i]
			}
			decoded.WriteByte(c)
			i++
			next = append(next, originalEnd(i))
		}
		view, offsets = decoded.String(), next
	}
	if view[0] == '{' || view[0] == '[' {
		stack := []byte{view[0]}
		var quote byte
		for i := 1; i < len(view); i++ {
			c := view[i]
			if quote != 0 {
				if c == '\\' {
					i++
					continue
				}
				if c == quote {
					quote = 0
				}
				continue
			}
			switch c {
			case '"', '\'':
				quote = c
			case '{', '[':
				stack = append(stack, c)
			case '}', ']':
				top := stack[len(stack)-1]
				if (top == '{' && c != '}') || (top == '[' && c != ']') {
					return len(raw)
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					end := i + 1
					if shell {
						end += diagnosticWordEnd(view[end:], true)
					}
					return originalEnd(end)
				}
			}
		}
		return len(raw)
	}
	start := 0
	if scheme := diagnosticScheme.FindStringIndex(view); scheme != nil {
		start = scheme[1]
	}
	return originalEnd(start + diagnosticWordEnd(view[start:], shell))
}

// diagnosticWordEnd consumes a whole shell-style word, including concatenated
// quotes, dollar-prefixed quotes, substitutions and escaped whitespace. Retain Python,
// backtick and curly quote support. JSON delimiters end JSON scalars but are
// literal inside shell words; unfinished quotes/escapes consume the rest.
func diagnosticWordEnd(value string, shell bool) int {
	var quote byte
	quoteEscapes := false
	setQuote := func(c byte, i int) {
		quote = c
		// POSIX single quotes treat backslashes literally; ANSI-C and Python
		// literal prefixes use escapes. Confusing these loses quote boundaries.
		quoteEscapes = c != '\'' || !shell || (i > 0 && value[i-1] == '$') ||
			(i == 1 && strings.ContainsRune("bruBRU", rune(value[0])))
	}
	var substitutions []byte
	curly := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		// The POSIX apostrophe splice closes a quote, escapes an apostrophe
		// outside it and reopens it. Otherwise accept diagnostic/Python \'
		// escapes too; the ambiguous boundary is consumed conservatively.
		apostropheSplice := strings.HasPrefix(value[i+1:], `'\''`)
		if c == '\\' && (quote != '\'' || quoteEscapes ||
			(i+1 < len(value) && value[i+1] == '\'' && !apostropheSplice)) {
			if i+1 < len(value) {
				i++
			}
			continue
		}
		if curly {
			if strings.HasPrefix(value[i:], "”") {
				curly = false
				i += len("”") - 1
			}
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if strings.HasPrefix(value[i:], "“") {
			curly = true
			i += len("“") - 1
			continue
		}
		if shell && i+1 < len(value) &&
			((value[i+1] == '(' && (c == '$' || c == '<' || c == '>')) || (c == '$' && value[i+1] == '{')) {
			substitutions = append(substitutions, value[i+1])
			i++
			continue
		}
		if len(substitutions) > 0 {
			switch c {
			case '(', '{':
				substitutions = append(substitutions, c)
			case ')', '}':
				top := substitutions[len(substitutions)-1]
				if (top == '(' && c != ')') || (top == '{' && c != '}') {
					return len(value)
				}
				substitutions = substitutions[:len(substitutions)-1]
			case '\'', '"', '`':
				setQuote(c, i)
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			setQuote(c, i)
		case ' ', '\t', '\n', '\r', '\v', '\f', ';':
			return i
		case ',', '}', ']':
			if !shell {
				return i
			}
		}
	}
	return len(value)
}
