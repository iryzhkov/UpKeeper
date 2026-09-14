// Package pyfmt reproduces the small pieces of Python formatting that leak into
// UpKeeper's user-visible strings.
//
// Validation errors are quoted with repr(), and operators (and tests) have been
// reading those exact strings since the first release, so the Go port keeps
// them byte for byte.
package pyfmt

import (
	"fmt"
	"strings"
	"unicode"
)

// Repr renders value the way Python's repr() renders a str.
//
// Python prefers single quotes and switches to double quotes only when the
// string contains a single quote but no double quote.
func Repr(value string) string {
	quote := byte('\'')
	if strings.ContainsRune(value, '\'') && !strings.ContainsRune(value, '"') {
		quote = '"'
	}
	var out strings.Builder
	out.WriteByte(quote)
	for _, r := range value {
		switch {
		case r == rune(quote) || r == '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\r':
			out.WriteString(`\r`)
		case r == '\t':
			out.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&out, `\x%02x`, r)
		case r < 0x7f:
			out.WriteRune(r)
		case unicode.IsPrint(r):
			out.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&out, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&out, `\u%04x`, r)
		default:
			fmt.Fprintf(&out, `\U%08x`, r)
		}
	}
	out.WriteByte(quote)
	return out.String()
}
