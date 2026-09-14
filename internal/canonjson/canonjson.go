// Package canonjson produces the exact JSON bytes the Python implementation
// produced.
//
// Two encodings are durable identities in UpKeeper and neither may drift:
//
//   - the release manifest, json.dumps(manifest, indent=2, sort_keys=True) plus
//     a trailing newline, whose SHA-256 is published as release_sha256;
//   - the agent-environment revision, json.dumps(spec, separators=(",", ":"),
//     sort_keys=True), a deliberately different, compact encoding.
//
// Python's json module escapes every character outside printable ASCII but
// leaves "<", ">" and "&" alone, which is the opposite of Go's encoding/json
// default. The encoder below therefore writes the bytes itself rather than
// configuring the standard encoder.
package canonjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Marshal renders value as json.dumps(value, indent=2, sort_keys=True) does.
func Marshal(value any) ([]byte, error) {
	return marshal(value, 2)
}

// MarshalFile renders value for on-disk storage: indented, with the trailing
// newline every UpKeeper document carries.
func MarshalFile(value any) ([]byte, error) {
	encoded, err := marshal(value, 2)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// Compact renders value as json.dumps(value, separators=(",", ":"),
// sort_keys=True) does.
func Compact(value any) ([]byte, error) {
	return marshal(value, 0)
}

// MarshalSpaced renders value as json.dumps(value, sort_keys=True) does, with
// Python's default ", " and ": " separators and no indentation. Lock owner
// records are written this way.
func MarshalSpaced(value any) ([]byte, error) {
	return marshal(value, -1)
}

// The indent argument of marshal selects one of the three Python encodings:
// a positive value indents by that many spaces, zero is the compact form, and
// -1 is the unindented form with Python's default spaced separators.
func marshal(value any, indent int) ([]byte, error) {
	normalized, err := Normalize(value)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := encode(&out, normalized, indent, 0); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Normalize converts an arbitrary Go value into the generic tree the encoder
// understands: nil, bool, string, json.Number, []any and map[string]any.
//
// Numbers keep their original literal text so that a value read from a manifest
// is written back unchanged.
func Normalize(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	return generic, nil
}

// Decode parses JSON into the same generic tree, preserving number literals.
func Decode(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, fmt.Errorf("trailing data after the JSON document")
	}
	return generic, nil
}

// DecodeObject parses JSON and insists the document is an object.
func DecodeObject(data []byte) (map[string]any, error) {
	generic, err := Decode(data)
	if err != nil {
		return nil, err
	}
	object, ok := generic.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("document is not an object")
	}
	return object, nil
}

func encode(out *bytes.Buffer, value any, indent, level int) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		out.WriteString(EscapeASCII(typed))
	case json.Number:
		out.WriteString(typed.String())
	case []any:
		return encodeArray(out, typed, indent, level)
	case map[string]any:
		return encodeObject(out, typed, indent, level)
	default:
		return fmt.Errorf("canonjson: unsupported value of type %T", value)
	}
	return nil
}

func encodeArray(out *bytes.Buffer, items []any, indent, level int) error {
	if len(items) == 0 {
		out.WriteString("[]")
		return nil
	}
	out.WriteByte('[')
	for position, item := range items {
		if position > 0 {
			writeItemSeparator(out, indent)
		}
		writeNewlineIndent(out, indent, level+1)
		if err := encode(out, item, indent, level+1); err != nil {
			return err
		}
	}
	writeNewlineIndent(out, indent, level)
	out.WriteByte(']')
	return nil
}

func encodeObject(out *bytes.Buffer, object map[string]any, indent, level int) error {
	if len(object) == 0 {
		out.WriteString("{}")
		return nil
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	// Go compares strings by byte, which for UTF-8 is code-point order: the
	// same order Python's sort_keys produces.
	sort.Strings(keys)
	out.WriteByte('{')
	for position, key := range keys {
		if position > 0 {
			writeItemSeparator(out, indent)
		}
		writeNewlineIndent(out, indent, level+1)
		out.WriteString(EscapeASCII(key))
		if indent == 0 {
			out.WriteByte(':')
		} else {
			out.WriteString(": ")
		}
		if err := encode(out, object[key], indent, level+1); err != nil {
			return err
		}
	}
	writeNewlineIndent(out, indent, level)
	out.WriteByte('}')
	return nil
}

func writeItemSeparator(out *bytes.Buffer, indent int) {
	if indent < 0 {
		out.WriteString(", ")
		return
	}
	out.WriteByte(',')
}

func writeNewlineIndent(out *bytes.Buffer, indent, level int) {
	if indent <= 0 {
		return
	}
	out.WriteByte('\n')
	out.WriteString(strings.Repeat(" ", indent*level))
}

// EscapeASCII quotes and escapes a string the way Python's json encoder does
// with ensure_ascii=True: everything outside printable ASCII becomes a \uXXXX
// escape, and "<", ">" and "&" are left alone.
func EscapeASCII(value string) string {
	var out strings.Builder
	out.Grow(len(value) + 2)
	out.WriteByte('"')
	for _, r := range value {
		switch {
		case r == '"':
			out.WriteString(`\"`)
		case r == '\\':
			out.WriteString(`\\`)
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\r':
			out.WriteString(`\r`)
		case r == '\t':
			out.WriteString(`\t`)
		case r == '\b':
			out.WriteString(`\b`)
		case r == '\f':
			out.WriteString(`\f`)
		case r >= 0x20 && r <= 0x7e:
			out.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&out, `\u%04x`, r)
		default:
			astral := r - 0x10000
			fmt.Fprintf(&out, `\u%04x\u%04x`, 0xd800+(astral>>10), 0xdc00+(astral&0x3ff))
		}
	}
	out.WriteByte('"')
	return out.String()
}
