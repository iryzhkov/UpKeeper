package rpcprobe

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Generated nested and escaped JSON reaches the real exchange error channel.
func TestRound1GeneratedProbeSecrets(t *testing.T) {
	const marker = "FAKE_SECRET_SUFFIX"
	for depth := 0; depth < 5; depth++ {
		var value any = map[string]any{"first": `"}]\"`, "second": marker}
		for i := 0; i < depth; i++ {
			value = []any{value, marker}
		}
		bytes, _ := json.Marshal(map[string]any{"credential": value})
		input := string(bytes)
		for layer := 0; layer < 4; layer++ {
			got := exchangeToolError(t, []any{text(input)}).Error()
			if strings.Contains(got, marker) {
				t.Fatalf("depth %d layer %d leaked: %q", depth, layer, got)
			}
			bytes, _ = json.Marshal(input)
			input = string(bytes)
		}
	}
}

func TestRound1EveryExchangeErrorUsesSharedRedaction(t *testing.T) {
	const marker = "FAKE_SECRET_SUFFIX"
	// Starting a missing executable reports the caller's path.
	_, err := Exchange([]string{filepath.Join(t.TempDir(), "password="+marker)}, os.Environ(), t.TempDir(), false, nil, time.Second)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("start error: %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lost OS error identity: %v", err)
	}
	// Tool names are caller supplied and appear outside the tool text bound.
	server := fakeServer(t, map[string]any{"isError": true, "content": []any{text("retry")}})
	calls := []Call{
		{Method: "tools/list", Params: map[string]any{}},
		{Method: "tools/call", Params: map[string]any{"name": "memory_status password=" + marker}},
	}
	_, err = Exchange([]string{server}, os.Environ(), t.TempDir(), false, calls, 20*time.Second)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("call name error: %v", err)
	}
	// Stderr is deliberately discarded, including on a failed exchange.
	script := filepath.Join(t.TempDir(), "server")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'password="+marker+"' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = Exchange([]string{script}, os.Environ(), t.TempDir(), false, nil, time.Second)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("stderr error: %v", err)
	}
}

// Every review reproduction exercises the actual child-process Exchange path.
func TestRound1ReviewSecretBoundaries(t *testing.T) {
	for name, input := range map[string]string{
		"quoted flag":            `ov --api-key "alpha FAKE_SECRET_SUFFIX omega" failed`,
		"quoted password flag":   `tool --password 'alpha FAKE_SECRET_SUFFIX omega' failed`,
		"unterminated flag":      `ov --api-key "alpha FAKE_SECRET_SUFFIX omega`,
		"nested object":          `{"credential": {"nested": {"value":"alpha"}, "second":"FAKE_SECRET_SUFFIX omega"}}`,
		"nested array":           `{"credential": [["alpha"], {"second":"FAKE_SECRET_SUFFIX omega"}]}`,
		"quoted delimiters":      `{"credential": {"nested": ["} ] \" alpha"], "second":"FAKE_SECRET_SUFFIX omega"}} retry`,
		"truncated object":       `{"credential": {"nested": {}, "second":"FAKE_SECRET_SUFFIX omega`,
		"escaped nested JSON":    `{\"credential\": {\"nested\": {}, \"second\":\"FAKE_SECRET_SUFFIX omega\"}}`,
		"quoted cookie":          `cookie="alpha FAKE_SECRET_SUFFIX omega"`,
		"quoted cookie header":   `Cookie: "alpha FAKE_SECRET_SUFFIX omega"`,
		"dash flag":              `tool --password -FAKE_SECRET_SUFFIX failed`,
		"dash API key flag":      `ov --api-key -FAKE_SECRET_SUFFIX failed`,
		"negative JSON password": `{"password": -123456789987654321}`,
		"negative JSON":          `{"token": -98765432109876543210}`,
		"unicode JSON key":       `{"api_\u006bey":"FAKE_SECRET_SUFFIX"}`,
		"URL":                    "postgres://user:FAKE_SECRET_SUFFIX@db failed",
		"environment":            `OV_API_KEY='alpha FAKE_SECRET_SUFFIX omega' failed`,
	} {
		t.Run(name, func(t *testing.T) {
			got := exchangeToolError(t, []any{text(input)}).Error()
			if strings.Contains(got, "FAKE_SECRET_SUFFIX") || strings.Contains(got, "omega") || strings.Contains(got, "987654321") || strings.Contains(got, "123456789987654321") {
				t.Fatalf("secret reached error: %q", got)
			}
		})
	}
}
