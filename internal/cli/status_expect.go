package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// statusExpectation is one known, accepted non-converged state: "homelab's t3
// is expected to drift, because ...". A file of them turns `status` into a
// gate, because only what nobody expected fails it.
type statusExpectation struct {
	Host string `json:"host"`
	// Component is empty for a host-level state such as unavailable.
	Component string `json:"component,omitempty"`
	State     string `json:"state"`
	// Reason is required: an accepted state nobody can explain is a failure
	// that has been hidden, not one that has been understood.
	Reason string `json:"reason"`
	used   bool
}

type statusExpectations struct {
	SchemaVersion int                  `json:"schema_version"`
	Accepted      []*statusExpectation `json:"accepted"`
}

var statePattern = regexp.MustCompile(`^[a-z][a-z-]*$`)

// loadStatusExpectations reads and checks an expectations file. Every host
// must be in the inventory and every component in the release's vocabulary,
// so that a typo cannot accept something by accepting nothing.
func loadStatusExpectations(path string, known map[string]bool) ([]*statusExpectation, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, failure.Validation("cannot read the expectations file %s: %v", path, err)
	}
	var document statusExpectations
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, failure.Validation("the expectations file %s is not valid: %v", path, err)
	}
	if document.SchemaVersion != 1 {
		return nil, failure.Validation("the expectations file %s has schema_version %d; expected 1",
			path, document.SchemaVersion)
	}
	components := map[string]bool{}
	for _, name := range release.ComponentOrder {
		components[name] = true
	}
	seen := map[string]bool{}
	for index, entry := range document.Accepted {
		if entry == nil {
			return nil, failure.Validation("expectation %d in %s is empty", index, path)
		}
		if _, err := safety.ValidateHostName(entry.Host, known); err != nil {
			return nil, failure.Validation("expectation %d in %s: %v", index, path, err)
		}
		if entry.Component != "" && !components[entry.Component] {
			return nil, failure.Validation("expectation %d in %s: unknown component %q", index, path, entry.Component)
		}
		if !statePattern.MatchString(entry.State) || entry.State == "converged" {
			return nil, failure.Validation("expectation %d in %s: %q is not a non-converged state", index, path, entry.State)
		}
		if strings.TrimSpace(entry.Reason) == "" {
			return nil, failure.Validation("expectation %d in %s: a reason is required", index, path)
		}
		key := entry.Host + "\x00" + entry.Component + "\x00" + entry.State
		if seen[key] {
			return nil, failure.Validation("expectation %d in %s repeats an earlier one", index, path)
		}
		seen[key] = true
	}
	return document.Accepted, nil
}

// classifyStatus decides status's result and exit code from an observation.
// A host that did not report an observation is unavailable (20), a failed host
// is partial (20) and a drifting host is drift (10). With expectations, a host
// whose every non-converged state is accepted counts as converged, and the
// document says what was accepted and which expectations matched nothing.
// Without expectations the result is exactly what it always was.
func classifyStatus(document map[string]any, expectations []*statusExpectation) (int, string) {
	code := 0
	result := "converged"
	accepted := []any{}
	for _, raw := range list(document["hosts"]) {
		host, _ := raw.(map[string]any)
		if host == nil {
			continue
		}
		host["name"] = host["host"]
		if host["observe_only"] != true {
			host["result"] = "unavailable"
			host["error"] = "release observation unavailable; target needs a compatible UpKeeper binary"
		}
		state := text(host["result"])
		if state != "unavailable" && state != "failed" && state != "drift" {
			continue
		}
		if matched := acceptHost(host, state, expectations); len(matched) > 0 {
			accepted = append(accepted, matched...)
			continue
		}
		switch state {
		case "unavailable", "failed":
			code = 20
			result = "partial"
		case "drift":
			if code == 0 {
				code = 10
				result = "drift"
			}
		}
	}
	if expectations != nil {
		document["accepted"] = accepted
		unused := []any{}
		for _, entry := range expectations {
			if !entry.used {
				unused = append(unused, expectationDocument(entry))
			}
		}
		document["expectations_unused"] = unused
	}
	return code, result
}

// acceptHost returns what accepted a non-converged host, or nothing when the
// host has a state nobody expected. A host-level expectation accepts the host
// whole; otherwise every component that is not converged or ahead must be
// accepted by name and state, and a host with no such component is not.
func acceptHost(host map[string]any, state string, expectations []*statusExpectation) []any {
	name := text(host["host"])
	for _, entry := range expectations {
		if entry.Host == name && entry.Component == "" && entry.State == state {
			entry.used = true
			host["accepted"] = entry.Reason
			return []any{expectationDocument(entry)}
		}
	}
	if state == "unavailable" {
		return nil
	}
	type match struct {
		component map[string]any
		entry     *statusExpectation
	}
	matches := []match{}
	for _, raw := range list(host["components"]) {
		component, _ := raw.(map[string]any)
		componentState := text(component["state"])
		if componentState == "converged" || componentState == "ahead" {
			continue
		}
		var found *statusExpectation
		for _, entry := range expectations {
			if entry.Host == name && entry.Component == text(component["name"]) && entry.State == componentState {
				found = entry
				break
			}
		}
		if found == nil {
			return nil
		}
		matches = append(matches, match{component: component, entry: found})
	}
	if len(matches) == 0 {
		return nil
	}
	documents := make([]any, 0, len(matches))
	for _, matched := range matches {
		matched.entry.used = true
		matched.component["accepted"] = matched.entry.Reason
		documents = append(documents, expectationDocument(matched.entry))
	}
	return documents
}

func expectationDocument(entry *statusExpectation) map[string]any {
	return map[string]any{
		"host": entry.Host, "component": nullable(entry.Component),
		"state": entry.State, "reason": entry.Reason,
	}
}
