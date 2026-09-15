package agentenv

import (
	"errors"
	"strings"
)

// Account, provider, project trust, credentials, hooks and sandbox policy remain
// host-owned. These keys affect the shared model and interaction experience.
var codexSettingKeys = []string{
	"model", "model_reasoning_effort", "model_reasoning_summary", "model_verbosity",
	"service_tier", "personality", "features", "tui",
}

func captureCodexSettings(root *Table) map[string]any {
	result := map[string]any{}
	for _, key := range codexSettingKeys {
		value, _ := root.Get(key)
		result[key] = genericValue(value)
	}
	return result
}

func validateCodexSettings(owned map[string]any) error {
	if !sameKeys(owned, []string{"mcp_servers", "settings"}) {
		return errors.New("invalid Codex settings envelope")
	}
	if _, ok := owned["mcp_servers"].(map[string]any); !ok {
		return errors.New("invalid Codex MCP registrations")
	}
	settings, ok := owned["settings"].(map[string]any)
	if !ok {
		return errors.New("invalid Codex shared settings")
	}
	for key := range settings {
		if !contains(codexSettingKeys, key) {
			return errors.New("unowned Codex setting: " + key)
		}
	}
	return CheckPublic(settings, "")
}

// ProjectRootKeys replaces only the requested root keys, including their dotted
// assignments and nested table sections. The parser supplies byte boundaries;
// comments and every unrelated table survive byte for byte. nil removes a key
// so a source using the client's default does not leave a target-local override.
func ProjectRootKeys(text string, owned map[string]any) (string, error) {
	root, err := ParseTOML(text)
	if err != nil {
		return "", err
	}
	same := true
	for key, value := range owned {
		current, present := root.Get(key)
		if value == nil {
			same = same && !present
			continue
		}
		equal, err := sameJSON(genericValue(current), value)
		if err != nil {
			return "", err
		}
		same = same && present && equal
	}
	if same {
		return text, nil
	}

	p := &tomlParser{text: text, defined: map[string]bool{}}
	scratch := NewTable()
	current := scratch
	section := ""
	var kept strings.Builder
	previous := 0
	for {
		p.skipIgnorable()
		if p.cursor >= len(text) {
			break
		}
		start := p.cursor
		key := section
		if text[p.cursor] == '[' {
			look := *p
			look.cursor++
			names, err := look.parseKeyPath()
			if err != nil || len(names) == 0 {
				return "", errors.New("invalid TOML header")
			}
			section = names[0]
			key = section
			current, err = p.parseHeader(scratch)
			if err != nil {
				return "", err
			}
		} else {
			if section == "" {
				look := *p
				names, err := look.parseKeyPath()
				if err != nil || len(names) == 0 {
					return "", errors.New("invalid TOML key")
				}
				key = names[0]
			}
			if err := p.parsePair(current); err != nil {
				return "", err
			}
		}
		if _, selected := owned[key]; selected {
			kept.WriteString(text[previous:start])
			previous = p.cursor
		}
	}
	kept.WriteString(text[previous:])
	var prefix strings.Builder
	for _, key := range sortedKeysOf(owned) {
		if owned[key] == nil {
			continue
		}
		rendered, err := RenderTOMLValue(owned[key])
		if err != nil {
			return "", err
		}
		prefix.WriteString(key + " = " + rendered + "\n")
	}
	result := prefix.String() + kept.String()
	parsed, err := ParseTOML(result)
	if err != nil {
		return "", err
	}
	for key, value := range owned {
		current, present := parsed.Get(key)
		if value == nil {
			if present {
				return "", errors.New("removed TOML key survived")
			}
		} else {
			equal, err := sameJSON(genericValue(current), value)
			if err != nil || !equal {
				return "", errors.New("projected TOML key did not verify")
			}
		}
	}
	return result, nil
}
