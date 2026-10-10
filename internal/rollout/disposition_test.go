package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRC122FullDispositionIncludesLaptopAndConvergedActions(t *testing.T) {
	c := rc122Fixture(t)
	f := &backendFake{c: c}
	raw := f.preview(true)
	var p preview
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	p.Hosts = append(p.Hosts, p.Hosts[0])
	p.Hosts[len(p.Hosts)-1].Host = c.LaptopHost
	for _, row := range p.Hosts {
		for _, component := range row.Components {
			c.AcceptedEffects = append(c.AcceptedEffects, AcceptedEffect{Host: row.Host, Component: component.Name, State: component.State, Actions: component.Actions, Reason: "parent release reviewed exact effect"})
		}
	}
	if err := fullPreviewDisposition(p, c); err != nil {
		t.Fatal(err)
	}
	c.AcceptedEffects = c.AcceptedEffects[1:]
	if err := fullPreviewDisposition(p, c); err == nil {
		t.Fatal("undispositioned effect accepted")
	}
	c.AcceptedEffects = nil
	if err := fullPreviewDisposition(p, c); err == nil {
		t.Fatal("converged-labelled actions exempted")
	}
}
func TestRC122HistoricalNonzeroFullPreviewCannotSatisfyGate(t *testing.T) {
	b, f, c, s := backendFixture(t)
	fencedFake(f)
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		raw, err := f.Run(ctx, dir, stdin, name, args...)
		if name == "upkeeper" && len(args) > 0 && args[0] == "pull" && contains(args, "--all") {
			return raw, errors.New("valid preview then exit40")
		}
		return raw, err
	})
	if err := b.prepare(context.Background(), c, s); err == nil || !strings.Contains(err.Error(), "exit40") {
		t.Fatal("historical exception survived", err)
	}
	if len(s.Extra["preview-full"]) != 0 || s.PackageDigest != "" || f.pushes != 0 {
		t.Fatal("failed producer supplied acceptance")
	}
}
