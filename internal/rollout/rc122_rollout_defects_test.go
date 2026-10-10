package rollout

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The findings contain the real producer shape, not raw preview bytes. Retain
// that successful local row shape here; remote exit-40 rows are still refused
// by the producer-status gate. Fill out the walk from this one-host fixture.
const rc122ScopedRow = `{"host":"omarchy-pc","components":[{"name":"t3-steward","state":"updated","actions":["installed verified pinned steward 0.11.0-rc.122"]},{"name":"t3","state":"converged","actions":[]}]}`

func TestScopedPreviewDeclaredPair(t *testing.T) {
	c := rc122Fixture(t)
	c.Version = "0.11.0-rc.122"
	manifest := []byte(`{"components":{"t3-steward":{},"t3":{"compatibility":{"steward_version":"0.11.0-rc.122"}},"upkeeper":{}}}`)
	for _, fault := range []string{"none", "extra", "missing-pair", "duplicate", "updated-pair", "pair-actions", "failed", "conflicting-host"} {
		t.Run(fault, func(t *testing.T) {
			hosts := []any{}
			for _, host := range append([]string{c.Canary}, c.FleetOrder...) {
				var row map[string]any
				if err := json.Unmarshal([]byte(rc122ScopedRow), &row); err != nil {
					t.Fatal(err)
				}
				row["host"] = host
				components := row["components"].([]any)
				if host == c.Canary {
					switch fault {
					case "extra":
						row["components"] = append(components, map[string]any{"name": "huyang", "state": "converged"})
					case "missing-pair":
						row["components"] = components[:1]
					case "duplicate":
						row["components"] = []any{components[0], components[0]}
					case "updated-pair":
						components[1].(map[string]any)["state"] = "updated"
					case "pair-actions":
						components[1].(map[string]any)["actions"] = []string{"unreviewed change"}
					case "failed":
						components[0].(map[string]any)["error_class"] = "failed"
					case "conflicting-host":
						row["name"] = "unexpected"
					}
				}
				hosts = append(hosts, row)
			}
			err := scopedPreview(jsonBytes(map[string]any{"hosts": hosts}), c, manifest)
			if (err == nil) != (fault == "none") {
				t.Fatalf("%s: %v", fault, err)
			}
		})
	}
	// Historical releases without the compatibility component still select only
	// the declared Steward component.
	c.FleetOrder = []string{c.CoordinatorHost}
	c.Canary = "agent-b"
	rows := []any{}
	for _, h := range []string{c.Canary, c.CoordinatorHost} {
		rows = append(rows, map[string]any{"host": h, "components": []any{map[string]any{"name": "t3-steward", "state": "updated", "actions": []string{c.Version}}}})
	}
	if err := scopedPreview(jsonBytes(map[string]any{"hosts": rows}), c, []byte(`{"components":{"t3-steward":{}}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestPackageInventoryUsesPreviewPlanner(t *testing.T) {
	_, _, c, s := backendFixture(t)
	seedPublication(t, c, s)
	var manifest map[string]any
	if err := json.Unmarshal(s.Extra["release-after"], &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["components"].(map[string]any)["upkeeper"] = map[string]any{"kind": "github-release"}
	for _, fault := range []string{"none", "missing-applicable", "extra-program", "duplicate"} {
		t.Run(fault, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(s.Extra["preview-full"], &doc); err != nil {
				t.Fatal(err)
			}
			row := doc["hosts"].([]any)[0].(map[string]any)
			components := row["components"].([]any)
			switch fault {
			case "missing-applicable":
				row["components"] = components[1:]
			case "extra-program":
				row["components"] = append(components, map[string]any{"name": "upkeeper", "state": "converged"})
			case "duplicate":
				row["components"] = append(components, components[0])
			}
			pkg := &reviewedPackage{Head: s.ExpectHead, Release: s.ReleaseSHA256, Evidence: map[string]json.RawMessage{"release-after": jsonBytes(manifest), "preview-full": jsonBytes(doc)}}
			got, err := packageComponents(c, pkg)
			if (err == nil) != (fault == "none") {
				t.Fatalf("%s: %v", fault, err)
			}
			if err == nil {
				for host, names := range got {
					if names["upkeeper"] || !names["t3"] || !names["t3-steward"] {
						t.Fatalf("%s: %v", host, names)
					}
				}
			}
		})
	}
}

func TestReviewedInactiveServicesGates(t *testing.T) {
	_, f, c, _ := backendFixture(t)
	unit := "t3-steward.service"
	inactive := []string{unit}
	pre := f.snapshot("middle")
	f.upgraded["middle"] = true
	post := f.snapshot("middle")
	pre[unit+".ActiveState"], pre[unit+".MainPID"] = "inactive", "0"
	post[unit+".ActiveState"], post[unit+".MainPID"] = "inactive", "0"
	assets := map[string]string{"linux_amd64": digest([]byte("linux_amd64"))}
	if err := snapshotGate(pre, post, c.Version, assets, inactive); err != nil {
		t.Fatal(err)
	}
	if err := snapshotGate(pre, post, c.Version, assets); err == nil {
		t.Fatal("unlisted inactive accepted")
	}
	for _, fault := range []string{"active-before", "active-after", "missing-state", "pid", "digest", "worker-old", "t3-restarted"} {
		t.Run(fault, func(t *testing.T) {
			a, z := map[string]string{}, map[string]string{}
			for k, v := range pre {
				a[k] = v
			}
			for k, v := range post {
				z[k] = v
			}
			switch fault {
			case "active-before":
				a[unit+".ActiveState"], a[unit+".MainPID"] = "active", "11"
			case "active-after":
				z[unit+".ActiveState"], z[unit+".MainPID"] = "active", "22"
			case "missing-state":
				delete(z, unit+".ActiveState")
			case "pid":
				z[unit+".MainPID"] = "22"
			case "digest":
				z["binary_sha256"] = "wrong"
			case "worker-old":
				z["t3-steward-worker.service.MainPID"] = a["t3-steward-worker.service.MainPID"]
			case "t3-restarted":
				z["t3code.service.MainPID"] = "44"
			}
			if err := snapshotGate(a, z, c.Version, assets, inactive); err == nil {
				t.Fatal("fault accepted")
			}
		})
	}
	c.InactiveServices = map[string][]string{"middle": inactive}
	before := c.Digest()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.InactiveServices = nil
	if c.Digest() == before {
		t.Fatal("disposition missing from review digest")
	}
	for _, bad := range []map[string][]string{{"unknown": inactive}, {"middle": {"t3code.service"}}, {"middle": {unit, unit}}, {"middle": {}}, {"excluded": inactive}} {
		c.InactiveServices = bad
		if err := c.Validate(); err == nil {
			t.Fatalf("bad disposition accepted: %v", bad)
		}
	}
}

func TestHostRestoresReviewedStoppedService(t *testing.T) {
	for _, fault := range []string{"none", "unlisted", "active-before", "stop-ineffective"} {
		t.Run(fault, func(t *testing.T) {
			b, f, c, s := backendFixture(t)
			host, unit := "middle", "t3-steward.service"
			if fault != "unlisted" {
				c.InactiveServices = map[string][]string{host: {unit}}
			}
			f.c = c
			seedPublication(t, c, s)
			fencedFake(f)
			stopped := false
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				if stdin == snapshotScript && f.host(args) == host {
					snap := f.snapshot(host)
					if (!f.upgraded[host] && fault != "active-before") || stopped {
						snap[unit+".ActiveState"], snap[unit+".MainPID"] = "inactive", "0"
					}
					return encodeKV(snap), nil
				}
				if strings.Contains(stdin, "systemctl --user stop '"+unit+"'") {
					if fault != "stop-ineffective" {
						stopped = true
					}
					return nil, nil
				}
				return f.Run(ctx, dir, stdin, name, args...)
			})
			err := b.host(context.Background(), c, host, s)
			if (err == nil) != (fault == "none") {
				t.Fatalf("%s: %v", fault, err)
			}
			if fault == "none" && !stopped {
				t.Fatal("reviewed unit left running")
			}
			if (fault == "unlisted" || fault == "active-before") && productionPulls(f) != 0 {
				t.Fatal("preflight failure dispatched pull")
			}
		})
	}
}

func TestStandardLaptopOutsideWalk(t *testing.T) {
	b, f, _, s := backendFixture(t)
	c := rc122Fixture(t)
	c.LaptopHost = ""
	f.c = c
	f.mask = "enabled"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	steps, err := Plan(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateWalk(c); err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if strings.Contains(step.Action, "laptop") {
			t.Fatal("laptop added to walk")
		}
	}
	if err = b.Check(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, stdin := range f.stdin {
		if strings.Contains(stdin, "show t3-steward-worker.service -p UnitFileState") {
			t.Fatal("standard laptop mask checked")
		}
	}
	// Enrollment and full convergence are performed outside this walk; final
	// status must still include that independently converged laptop.
	seedPublication(t, c, s)
	s.ReleaseCommit = f.head
	fencedFake(f)
	if err = b.finalConfiguration(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		raw, e := f.Run(ctx, dir, stdin, name, args...)
		if name == "upkeeper" && len(args) > 0 && args[0] == "status" && e == nil {
			var doc map[string]any
			_ = json.Unmarshal(raw, &doc)
			for _, h := range doc["hosts"].([]any) {
				row := h.(map[string]any)
				if row["host"] == "laptop" {
					row["components"].([]any)[0].(map[string]any)["state"] = "updated"
				}
			}
			raw = jsonBytes(doc)
		}
		return raw, e
	})
	if err = b.finalConfiguration(context.Background(), c, s); err == nil {
		t.Fatal("laptop drift accepted")
	}
}
