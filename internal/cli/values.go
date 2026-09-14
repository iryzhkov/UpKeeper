package cli

import (
	"encoding/json"
	"fmt"
	"sort"
)

// The report documents flow through the command layer as decoded JSON, because
// a report may have been produced here or parsed back from a target. These
// helpers read one of those documents without ever panicking on a shape the
// caller did not expect, which keeps a malformed target response from taking
// the controller down.

func text(value any) string {
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
}

func scalar(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case int:
		return fmt.Sprintf("%d", typed)
	case json.Number:
		return typed.String()
	case float64:
		return fmt.Sprintf("%g", typed)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func flag(value any) bool {
	typed, _ := value.(bool)
	return typed
}

func list(value any) []any {
	typed, _ := value.([]any)
	return typed
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if entry, ok := item.(string); ok {
				values = append(values, entry)
			}
		}
		return values
	default:
		return nil
	}
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
