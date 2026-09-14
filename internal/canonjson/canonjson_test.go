package canonjson_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// The expected strings below were produced by the Python implementation this
// package replaces, with json.dumps at the three encodings UpKeeper uses. They
// are the contract: the release identity is the digest of these exact bytes.
func TestEncodingsMatchPython(t *testing.T) {
	cases := []struct {
		name     string
		document string
		indented string
		compact  string
		spaced   string
	}{
		{
			name:     "html characters are never escaped",
			document: `{"a": "<&>\"\\"}`,
			indented: "{\n  \"a\": \"<&>\\\"\\\\\"\n}",
			compact:  `{"a":"<&>\"\\"}`,
			spaced:   `{"a": "<&>\"\\"}`,
		},
		{
			name:     "every non-ascii rune becomes a escape, astral runes become a pair",
			document: "{\"k\": \"café 中文 \U0001F600\"}",
			indented: "{\n  \"k\": \"caf\\u00e9 \\u4e2d\\u6587 \\ud83d\\ude00\"\n}",
			compact:  "{\"k\":\"caf\\u00e9 \\u4e2d\\u6587 \\ud83d\\ude00\"}",
			spaced:   "{\"k\": \"caf\\u00e9 \\u4e2d\\u6587 \\ud83d\\ude00\"}",
		},
		{
			name:     "scalars, empty containers and key order",
			document: `{"z":1,"a":[],"m":{},"n":null,"b":true,"f":1.5,"i":-3}`,
			indented: "{\n  \"a\": [],\n  \"b\": true,\n  \"f\": 1.5,\n  \"i\": -3,\n  \"m\": {},\n  \"n\": null,\n  \"z\": 1\n}",
			compact:  `{"a":[],"b":true,"f":1.5,"i":-3,"m":{},"n":null,"z":1}`,
			spaced:   `{"a": [], "b": true, "f": 1.5, "i": -3, "m": {}, "n": null, "z": 1}`,
		},
		{
			name:     "nested containers indent per level",
			document: `{"nested":{"b":[1,{"c":"d"}],"a":"x"}}`,
			indented: "{\n  \"nested\": {\n    \"a\": \"x\",\n    \"b\": [\n      1,\n      {\n        \"c\": \"d\"\n      }\n    ]\n  }\n}",
			compact:  `{"nested":{"a":"x","b":[1,{"c":"d"}]}}`,
			spaced:   `{"nested": {"a": "x", "b": [1, {"c": "d"}]}}`,
		},
		{
			name:     "control characters use the named escapes python uses",
			document: "{\"ctl\": \"tab\\tnl\\nbs\\\\ del\\u007f bell\\u0007\"}",
			indented: "{\n  \"ctl\": \"tab\\tnl\\nbs\\\\ del\\u007f bell\\u0007\"\n}",
			compact:  "{\"ctl\":\"tab\\tnl\\nbs\\\\ del\\u007f bell\\u0007\"}",
			spaced:   "{\"ctl\": \"tab\\tnl\\nbs\\\\ del\\u007f bell\\u0007\"}",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decoded, err := canonjson.Decode([]byte(testCase.document))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, encoding := range []struct {
				name     string
				encode   func(any) ([]byte, error)
				expected string
			}{
				{"Marshal", canonjson.Marshal, testCase.indented},
				{"Compact", canonjson.Compact, testCase.compact},
				{"MarshalSpaced", canonjson.MarshalSpaced, testCase.spaced},
			} {
				produced, err := encoding.encode(decoded)
				if err != nil {
					t.Fatalf("%s: %v", encoding.name, err)
				}
				if string(produced) != encoding.expected {
					t.Errorf("%s\n produced %q\n expected %q",
						encoding.name, string(produced), encoding.expected)
				}
			}
		})
	}
}

// The published release manifest is the durable identity this package exists
// for. These digests come from the Python implementation reading the very file
// in this repository, so a drift in the encoder shows up here first.
func TestPublishedManifestDigestsMatchPython(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "desired", "release.json"))
	if err != nil {
		t.Skipf("no published manifest to compare against: %v", err)
	}
	document, err := canonjson.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	indented, err := canonjson.MarshalFile(document)
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	if string(indented) != string(raw) {
		t.Error("re-encoding the published manifest did not reproduce the file byte for byte")
	}
	for _, expectation := range []struct {
		name     string
		produced []byte
		digest   string
	}{
		{"release identity", indented,
			"4af4e0281126f3523f256106ce85d5adf6d2e29ef76ae7141d584780fc81fc5a"},
		{"compact", mustEncode(t, canonjson.Compact, document),
			"b61345938d6895afecb9e231d71b136f3d144a39de03c94146429e837ef25026"},
		{"spaced", mustEncode(t, canonjson.MarshalSpaced, document),
			"d45b49fb10eced8a86f559b2a1dab5c280096ea54a6e7335949efac2c5f94779"},
	} {
		sum := sha256.Sum256(expectation.produced)
		if hex.EncodeToString(sum[:]) != expectation.digest {
			t.Errorf("%s digest is %s, expected %s",
				expectation.name, hex.EncodeToString(sum[:]), expectation.digest)
		}
	}
}

func TestNumbersKeepTheirLiteralText(t *testing.T) {
	// A manifest read and written back must not have its numbers reformatted,
	// because that would change the release identity without changing intent.
	document, err := canonjson.Decode([]byte(`{"a":1.0,"b":1e3,"c":19}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	produced, err := canonjson.Compact(document)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if string(produced) != `{"a":1.0,"b":1e3,"c":19}` {
		t.Errorf("produced %q", string(produced))
	}
}

func TestDecodeObjectRefusesNonObjectsAndTrailingData(t *testing.T) {
	for _, document := range []string{`[]`, `"text"`, `{} {}`, ``} {
		if _, err := canonjson.DecodeObject([]byte(document)); err == nil {
			t.Errorf("expected %q to be refused", document)
		}
	}
}

func mustEncode(t *testing.T, encode func(any) ([]byte, error), value any) []byte {
	t.Helper()
	produced, err := encode(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return produced
}
