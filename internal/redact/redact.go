// Package redact removes secrets from every value before it is printed or
// persisted.
//
// Redaction is a gate, not a convenience: reports, run records and lock owner
// files all pass through it, and the test suite treats a leak as a release
// blocker. The patterns are ported from the Python implementation unchanged.
package redact

import (
	"regexp"
	"strings"
)

// Placeholder replaces every value a pattern matches.
const Placeholder = "<redacted>"

var (
	secretKeyPattern = regexp.MustCompile(
		`(?i)(?:^|[_-])(?:authorization|cookie|credential|password|passwd|private[_-]?key|` +
			`secret|token|api[_-]?key)(?:$|[_-])`)
	privateKeyPattern = regexp.MustCompile(
		`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	authorizationPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*(?:bearer|token|basic)\s+)[^\s,;]+`)
	assignmentPattern    = regexp.MustCompile(
		`(?i)\b(password|passwd|secret|token|api[_-]?key|private[_-]?key)\s*([=:])\s*([^\s,;]+)`)
	queryPattern = regexp.MustCompile(`(?i)([?&](?:access_token|api_key|key|password|secret|token)=)[^&\s]+`)
	tokenPattern = regexp.MustCompile(
		`(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|` +
			`gitea_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{20,}|AGE-SECRET-KEY-[A-Z0-9-]{20,})`)
	jwtPattern         = regexp.MustCompile(`[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}`)
	urlUserInfoPattern = regexp.MustCompile(`(https?|ssh)://[^\s/@:]+:[^\s/@]+@`)
)

// Text redacts secrets inside a free-form string.
func Text(value string) string {
	value = privateKeyPattern.ReplaceAllString(value, Placeholder)
	value = urlUserInfoPattern.ReplaceAllString(value, "${1}://"+Placeholder+"@")
	value = authorizationPattern.ReplaceAllString(value, "${1}"+Placeholder)
	value = assignmentPattern.ReplaceAllString(value, "${1}${2}"+Placeholder)
	value = queryPattern.ReplaceAllString(value, "${1}"+Placeholder)
	value = replaceUnlessPrecededBy(tokenPattern, value, Placeholder, isASCIIAlphanumeric)
	return replaceUnlessPrecededBy(jwtPattern, value, Placeholder, isJWTBoundaryByte)
}

// Value redacts a whole decoded document. Keys that name a secret are replaced
// wholesale; every remaining string is scanned for secret-shaped content.
func Value(value any) any {
	return withKey(value, "")
}

// WithKey redacts a value that was found under the given key.
func WithKey(value any, key string) any {
	if key == "" {
		return Value(value)
	}
	return withKey(value, key)
}

func withKey(value any, key string) any {
	// secret_refs holds the *names* of secrets, never their content. Redacting
	// the list wholesale would destroy the audit trail it exists for.
	if key == "secret_refs" {
		if items, ok := value.([]any); ok {
			redacted := make([]any, 0, len(items))
			for _, item := range items {
				redacted = append(redacted, withKey(item, ""))
			}
			return redacted
		}
	}
	if key != "" && secretKeyPattern.MatchString(key) {
		return Placeholder
	}
	switch typed := value.(type) {
	case string:
		return Text(typed)
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for itemKey, itemValue := range typed {
			redacted[itemKey] = withKey(itemValue, itemKey)
		}
		return redacted
	case []any:
		redacted := make([]any, 0, len(typed))
		for _, item := range typed {
			redacted = append(redacted, withKey(item, ""))
		}
		return redacted
	default:
		return value
	}
}

// replaceUnlessPrecededBy is Go's stand-in for a negative lookbehind.
//
// Go's regexp engine has no lookbehind, so the scan is driven by hand: when a
// candidate is rejected because of the byte in front of it, the search resumes
// one byte later, which is what Python's engine does.
func replaceUnlessPrecededBy(
	pattern *regexp.Regexp, value, replacement string, rejects func(byte) bool,
) string {
	var out strings.Builder
	position := 0
	for position <= len(value) {
		found := pattern.FindStringIndex(value[position:])
		if found == nil {
			break
		}
		start, end := position+found[0], position+found[1]
		if start > 0 && rejects(value[start-1]) {
			out.WriteString(value[position : start+1])
			position = start + 1
			continue
		}
		out.WriteString(value[position:start])
		out.WriteString(replacement)
		position = end
	}
	out.WriteString(value[position:])
	return out.String()
}

func isASCIIAlphanumeric(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func isJWTBoundaryByte(b byte) bool {
	return isASCIIAlphanumeric(b) || b == '_' || b == '-'
}
