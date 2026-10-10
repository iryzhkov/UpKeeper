package rollout

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

type AcceptedEffect struct {
	Host, Component, State, Reason string
	Actions                        []string
}

// Full preview acceptance is exact, including actions on converged components.
// This is publication authority for the parent release, never timer authority.
func fullPreviewDisposition(p preview, c Config) error {
	seen := map[string]bool{}
	used := make([]bool, len(c.AcceptedEffects))
	for _, row := range p.Hosts {
		host := row.Host
		if host == "" {
			host = row.Name
		}
		if host == "" || c.Targets[host] == "" || seen[host] {
			return errors.New("full preview host unknown or duplicated")
		}
		seen[host] = true
		if row.Result != "" && row.Result != "ok" && row.Result != "updated" && row.Result != "converged" {
			return fmt.Errorf("full preview host %s failed: %s", host, row.Result)
		}
		if len(row.Components) == 0 {
			return errors.New("full preview lacks component inventory")
		}
		componentSeen := map[string]bool{}
		for _, component := range row.Components {
			if component.Name == "" || componentSeen[component.Name] {
				return errors.New("full preview component missing or duplicated")
			}
			componentSeen[component.Name] = true
			if component.ErrorClass != "" || (component.State != "updated" && component.State != "converged" && component.State != "skipped") {
				return fmt.Errorf("unresolved full preview failure %s/%s: %s", host, component.Name, component.State)
			}
			if component.State == "converged" && len(component.Actions) == 0 {
				continue
			}
			matches := 0
			for i, effect := range c.AcceptedEffects {
				if effect.Host == host && effect.Component == component.Name && effect.State == component.State &&
					reflect.DeepEqual(effect.Actions, component.Actions) && strings.TrimSpace(effect.Reason) != "" {
					matches++
					used[i] = true
				}
			}
			if matches != 1 {
				return fmt.Errorf("full preview effect lacks exact unique disposition: %s/%s", host, component.Name)
			}
		}
	}
	for host := range c.Targets {
		if !seen[host] {
			return fmt.Errorf("full preview missing intended target %s", host)
		}
	}
	for _, matched := range used {
		if !matched {
			return errors.New("accepted full-preview effect did not match current pinned preview")
		}
	}
	return nil
}
