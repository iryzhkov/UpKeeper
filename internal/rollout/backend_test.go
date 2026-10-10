package rollout

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type backendFake struct {
	c               Config
	configHome      string
	calls           []string
	stdin           []string
	head            string
	changed         bool
	manifestDrift   bool
	scopeFault      string
	ciBad           bool
	remoteDrift     bool
	mask            string
	selfActive      bool
	controllerTimer bool
	fenced          map[string]bool
	upgraded        map[string]bool
	backupFault     string
	epochDrift      bool
	reconnect       int
	postCalls       int
	artifact        map[string][]byte
	releaseAssets   []map[string]any
	checksums       string
	pushes          int
	saved           bool
}

func backendFixture(t *testing.T) (*ProductionBackend, *backendFake, Config, *State) {
	t.Helper()
	c := fixture(t)
	home := attachLegacyCoordinatorConfig(t, &c)
	for _, h := range append([]string{c.Canary}, c.FleetOrder...) {
		c.AcceptedEffects = append(c.AcceptedEffects,
			AcceptedEffect{Host: h, Component: "t3-steward", State: "updated", Actions: []string{"install " + c.Version}, Reason: "reviewed binary update"},
			AcceptedEffect{Host: h, Component: "agent-tooling", State: "converged", Actions: []string{"mise upgrade --yes --no-prune"}, Reason: "retained reviewed tooling action"})
	}
	f := &backendFake{c: c, configHome: home, head: strings.Repeat("b", 40), mask: "masked", fenced: map[string]bool{}, upgraded: map[string]bool{}, artifact: map[string][]byte{}}
	for i, p := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		name := "t3-steward_" + c.Version + "_" + p + ".tar.gz"
		archive := testArchive(t, []byte(p))
		id := fmt.Sprint(i + 2)
		f.artifact[id] = archive
		f.releaseAssets = append(f.releaseAssets, map[string]any{"name": name, "id": i + 2})
		f.checksums += digest(archive) + "  " + name + "\n"
	}
	f.artifact["1"] = []byte(f.checksums)
	f.releaseAssets = append(f.releaseAssets, map[string]any{"name": "checksums.txt", "id": 1})
	b := NewProductionBackend()
	b.Runner = f
	b.Now = func() time.Time { return time.Unix(10000, 0) }
	b.Sleep = func(context.Context, time.Duration) error { return nil }
	b.inspectBinary = func(p []byte, tag, sha string) error {
		if tag != "v"+c.Version || sha != strings.Repeat("c", 40) {
			return errors.New("metadata mismatch")
		}
		return nil
	}
	s := &State{ReleaseCommit: f.head, Extra: map[string]json.RawMessage{}, Completed: map[string]bool{}, Save: func() error { f.saved = true; return nil }}
	return b, f, c, s
}
func testArchive(t *testing.T, p []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tr := tar.NewWriter(gz)
	if err := tr.WriteHeader(&tar.Header{Name: "t3-steward", Mode: 0755, Size: int64(len(p))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func jsonBytes(v any) []byte { p, _ := json.Marshal(v); return p }
func (f *backendFake) host(args []string) string {
	if len(args) > 1 {
		target := args[len(args)-2]
		for h, v := range f.c.Targets {
			if v == target {
				return h
			}
		}
	}
	return f.c.LocalHost
}
func (f *backendFake) snapshot(host string) map[string]string {
	up := f.upgraded[host]
	version, hash, pid, start := "0.11.0-rc.117", "old", "11", "before"
	if up {
		version, hash, pid, start = f.c.Version, digest([]byte("linux_amd64")), "22", "after"
	}
	m := map[string]string{"arch": "x86_64", "version": "t3-steward " + version, "binary_sha256": hash, "t3code.service.MainPID": "33", "t3code.service.ExecMainStartTimestamp": "same"}
	for _, unit := range []string{"t3-steward.service", "t3-steward-worker.service"} {
		m[unit+".ActiveState"] = "active"
		m[unit+".MainPID"] = pid
		m[unit+".ExecMainStartTimestamp"] = start
		m[unit+".exe"] = "/bin/t3-steward"
	}
	return m
}
func encodeKV(m map[string]string) []byte {
	var out strings.Builder
	for k, v := range m {
		fmt.Fprintf(&out, "%s=%s\n", k, v)
	}
	return []byte(out.String())
}
func (f *backendFake) manifest(after bool) []byte {
	version := "0.11.0-rc.117"
	if after {
		version = f.c.Version
	}
	m := map[string]any{"components": map[string]any{"t3-steward": map[string]any{"version": version}, "t3": map[string]any{"compatibility": map[string]any{"steward_version": version}}, "omarchy-setup": map[string]any{"commit": "pinned"}}}
	if after && f.manifestDrift {
		m["catalog"] = "changed"
	}
	return jsonBytes(m)
}
func (f *backendFake) preview(full bool) []byte {
	hosts := []any{}
	for _, h := range append([]string{f.c.Canary}, f.c.FleetOrder...) {
		if f.scopeFault == "missing" && h == f.c.Canary {
			continue
		}
		state := "updated"
		if f.scopeFault == "failed" && h == f.c.Canary {
			state = "failed"
		}
		components := []any{map[string]any{"name": "t3-steward", "state": state, "actions": []string{"install " + f.c.Version}}}
		if full || f.scopeFault == "extra" {
			components = append(components, map[string]any{"name": "agent-tooling", "state": "converged", "actions": []string{"mise upgrade --yes --no-prune"}})
		}
		hosts = append(hosts, map[string]any{"host": h, "components": components})
	}
	if full {
		hosts = append(hosts, map[string]any{"host": f.c.LaptopHost, "components": []any{map[string]any{"name": "t3-steward", "state": "converged", "actions": []string{}}}})
	}
	return jsonBytes(map[string]any{"hosts": hosts})
}
func (f *backendFake) Run(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
	if stdin == configTransactionScript {
		return runLegacyConfigCommand(ctx, f.configHome, stdin, name, args...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	f.stdin = append(f.stdin, stdin)
	if name == "bash" || name == "ssh" {
		h := f.host(args)
		switch {
		case strings.Contains(stdin, "show t3-steward-worker.service -p UnitFileState"):
			return []byte(f.mask), nil
		case stdin == timerScript:
			active := "active"
			if f.fenced[h] {
				active = "inactive"
			}
			self, controller := "inactive", "inactive"
			if f.selfActive {
				self = "active"
			}
			if f.controllerTimer {
				controller = "active"
			}
			return encodeKV(map[string]string{"ActiveState": active, "UnitFileState": "enabled", "SelfServiceActive": self, "ControllerTimer": controller}), nil
		case strings.Contains(stdin, "stop upkeeper-self.timer"):
			if !f.saved {
				return nil, errors.New("timer stopped before prior state persisted")
			}
			f.fenced[h] = true
			return nil, nil
		case stdin == snapshotScript:
			return encodeKV(f.snapshot(h)), nil
		case stdin == legacyBackupScript:
			if f.backupFault == "command" {
				return nil, errors.New("restore failed")
			}
			integrity, fk := "ok", "0"
			if f.backupFault == "integrity" {
				integrity = "failed"
			}
			if f.backupFault == "fk" {
				fk = "2"
			}
			return encodeKV(map[string]string{"snapshot": "/home/operator/snapshot", "scratch": "/home/operator/scratch", "restore_integrity": integrity, "restore_fk_violations": fk}), nil
		case strings.Contains(stdin, "worker enroll"):
			return nil, nil
		}
	}
	if name == "quiet" {
		return nil, nil
	}
	if name == "git" {
		switch args[0] {
		case "ls-remote":
			if contains(args, "--tags") {
				return []byte(strings.Repeat("c", 40) + "\trefs/tags/v" + f.c.Version), nil
			}
			head := f.head
			if f.remoteDrift && contains(args, f.c.Remotes[1]) {
				head = strings.Repeat("d", 40)
			}
			return []byte(head + "\trefs/heads/main"), nil
		case "symbolic-ref":
			return []byte("main"), nil
		case "rev-parse":
			return []byte(f.head), nil
		case "status":
			if f.changed {
				return []byte(" M desired/release.json\n"), nil
			}
			return nil, nil
		case "show":
			return f.manifest(false), nil
		case "diff":
			return []byte("binary-only diff"), nil
		}
	}
	if name == "cat" {
		return f.manifest(true), nil
	}
	if name == "gh" {
		if args[0] == "run" {
			if f.ciBad {
				return []byte("[{\"status\":\"completed\",\"conclusion\":\"failure\"}]"), nil
			}
			return []byte("[{\"status\":\"completed\",\"conclusion\":\"success\"}]"), nil
		}
		if strings.Contains(args[1], "/commits/") {
			return []byte(strings.Repeat("c", 40)), nil
		}
		if strings.Contains(args[1], "/releases/tags/") {
			return jsonBytes(map[string]any{"assets": f.releaseAssets}), nil
		}
		if strings.Contains(args[1], "/releases/assets/") {
			id := args[1][strings.LastIndex(args[1], "/")+1:]
			return f.artifact[id], nil
		}
	}
	if name == "upkeeper" {
		switch args[0] {
		case "author":
			if !contains(args, "--dry-run") {
				f.changed = true
			}
			return []byte("{}"), nil
		case "status":
			return seedStatus(f), nil
		case "fleet-plan":
			return jsonBytes(map[string]any{"release_sha256": strings.Repeat("e", 64)}), nil
		case "pull":
			if contains(args, "--dry-run") {
				return f.preview(contains(args, "--all")), nil
			}
			if !f.saved {
				return nil, errors.New("host pull before snapshot persisted")
			}
			host := args[2]
			f.upgraded[host] = true
			return []byte("{}"), nil
		case "rollback":
			if !contains(args, "--dry-run") {
				for i, a := range args {
					if a == "--hosts" {
						f.upgraded[args[i+1]] = false
					}
				}
			}
			return []byte("{}"), nil
		case "push":
			f.changed = false // authored publication commits the staged release
			f.pushes++
			f.head = strings.Repeat("f", 40)
			return []byte("{}"), nil
		case "hold":
			return []byte("{\"holds\":[]}"), nil
		}
	}
	if name == "t3-steward" {
		if args[0] == "coordinator" {
			epoch := 1
			release := "0.11.0-rc.117"
			health := "healthy"
			if f.upgraded[f.c.CoordinatorHost] {
				epoch = 2
				release = f.c.Version
			}
			if f.upgraded[f.c.Canary] {
				f.postCalls++
				if f.epochDrift {
					epoch++
				}
				if f.postCalls <= f.reconnect {
					health = "degraded"
				}
			}
			return jsonBytes(map[string]any{"health": health, "release": release, "epoch": epoch}), nil
		}
		if args[0] == "worker" {
			epoch := 1
			if f.upgraded[f.c.CoordinatorHost] {
				epoch = 2
			}
			rows := []any{}
			for _, h := range append(append([]string{}, f.c.ActiveWorkers...), f.c.DrainedHosts...) {
				state := "observed"
				if contains(f.c.DrainedHosts, h) {
					state = "draining"
				}
				release := "0.11.0-rc.117"
				if f.upgraded[h] {
					release = f.c.Version
				}
				rows = append(rows, map[string]any{"requirement": map[string]any{"workerId": h, "catalogRevision": 1}, "state": state, "health": "ready", "stale": false, "enrolled": true, "enrollment": map[string]any{"request": map[string]any{"catalogRevision": 1}}, "snapshot": map[string]any{"coordinatorEpoch": epoch, "inventory": map[string]any{"runtime": map[string]any{"release": release}, "telemetry": map[string]any{"running_attempts": 0}}}})
			}
			return jsonBytes(map[string]any{"workers": rows}), nil
		}
	}
	return nil, fmt.Errorf("unexpected fake command %s stdin=%s", call, stdin)
}
func seedAssets(t *testing.T, s *State) {
	t.Helper()
	if err := record(s, "assets", assetEvidence{Binaries: map[string]string{"linux_amd64": digest([]byte("linux_amd64")), "linux_arm64": digest([]byte("linux_arm64"))}}); err != nil {
		t.Fatal(err)
	}
}
func fencedFake(f *backendFake) {
	for _, h := range f.c.TimerHosts {
		f.fenced[h] = true
	}
}
func TestProductionCheckIsReadOnly(t *testing.T) {
	b, f, c, _ := backendFixture(t)
	if err := b.Check(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(c.Checkout)
	if err != nil || len(entries) != 0 {
		t.Fatalf("check wrote local files: %v %v", entries, err)
	}
	for i, call := range f.calls {
		if strings.Contains(call, "release download") || strings.Contains(f.stdin[i], "systemctl --user stop") || strings.Contains(call, "upkeeper author") || strings.Contains(call, "upkeeper push") {
			t.Fatalf("mutating check: %s", call)
		}
	}
}
func TestCriterion1ProductionAuthorScope(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(fmt.Sprint(drift), func(t *testing.T) {
			b, f, c, s := backendFixture(t)
			fencedFake(f)
			f.manifestDrift = drift
			err := b.Execute(context.Background(), c, "prepare", s)
			if (err != nil) != drift {
				t.Fatalf("scope result %v", err)
			}
			if !drift {
				if s.PackageDigest == "" {
					t.Fatal("package not frozen")
				}
				if _, err = os.Stat(c.StatePath + ".review-package.json"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestCriterion2AssetsFourPlatformsAndChecksums(t *testing.T) {
	b, f, c, _ := backendFixture(t)
	v, err := b.assets(context.Background(), c, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	if v.Tag != "v"+c.Version || v.ChecksumsSHA256 == "" || len(v.Binaries) != 4 {
		t.Fatalf("incomplete evidence: %+v", v)
	}
	f.artifact["3"] = []byte("tampered")
	if _, err = b.assets(context.Background(), c, strings.Repeat("c", 40)); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	f.ciBad = true
	if err = b.Check(context.Background(), c); err == nil {
		t.Fatal("red release CI accepted")
	}
	if err = validateBuildInfo([]byte("not executable"), "v"+c.Version, strings.Repeat("c", 40)); err == nil {
		t.Fatal("invalid build metadata accepted")
	}
}
func writeReview(t *testing.T, c *Config, s *State) {
	t.Helper()
	c.ReviewFile = filepath.Join(t.TempDir(), "review.md")
	p := "VERDICT: ACCEPT\n" + s.ExpectHead + "\n" + s.ReleaseSHA256 + "\nPACKAGE " + s.PackageDigest + "\n"
	if err := os.WriteFile(c.ReviewFile, []byte(p), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestCriterion3ReviewedFencesAndPackage(t *testing.T) {
	for _, fault := range []string{"none", "head", "release", "package", "verdict"} {
		t.Run(fault, func(t *testing.T) {
			b, f, c, s := backendFixture(t)
			fencedFake(f)
			if err := b.prepare(context.Background(), c, s); err != nil {
				t.Fatal(err)
			}
			writeReview(t, &c, s)
			switch fault {
			case "head":
				f.head = strings.Repeat("9", 40)
			case "release":
				s.ReleaseSHA256 = strings.Repeat("8", 64)
			case "package":
				if err := os.WriteFile(c.StatePath+".review-package.json", []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "verdict":
				if err := os.WriteFile(c.ReviewFile, []byte("VERDICT: CHANGES_REQUESTED"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := b.publish(context.Background(), c, s)
			if fault == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if f.pushes != 1 || s.ReleaseCommit != f.head {
					t.Fatal("no successful fenced publication")
				}
				last := ""
				for _, call := range f.calls {
					if strings.HasPrefix(call, "upkeeper push") {
						last = call
					}
				}
				if !strings.HasSuffix(last, strings.Join(c.Remotes, " ")) {
					t.Fatal("configured remotes missing")
				}
			} else if err == nil || f.pushes != 0 {
				t.Fatalf("unsafe publish accepted: %v", err)
			}
		})
	}
}
func TestCriterion4EveryScopedTargetAndNoOtherComponent(t *testing.T) {
	for _, fault := range []string{"missing", "failed", "extra"} {
		t.Run(fault, func(t *testing.T) {
			b, f, c, s := backendFixture(t)
			fencedFake(f)
			f.scopeFault = fault
			if err := b.prepare(context.Background(), c, s); err == nil {
				t.Fatal("scope fault accepted")
			}
		})
	}
}
func TestCriterion5FullEffectsRetainedTimersStayFenced(t *testing.T) {
	b, f, c, s := backendFixture(t)
	fencedFake(f)
	if err := b.prepare(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(s.Extra["preview-full"]), "mise upgrade --yes --no-prune") {
		t.Fatal("converged actionable full drift hidden")
	}
	for _, script := range f.stdin {
		if strings.Contains(script, "start upkeeper-self.timer") {
			t.Fatal("timer restored without separately reviewed full effect gate")
		}
	}
}
func TestCriterion6RecordedFenceAndLaptopSafety(t *testing.T) {
	b, f, c, s := backendFixture(t)
	if err := b.fence(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	for _, h := range c.TimerHosts {
		if s.TimerPrior[h] == "" || !f.fenced[h] {
			t.Fatalf("%s not recorded and stopped", h)
		}
	}
	f.mask = "enabled"
	if err := b.timers(context.Background(), c, true); err == nil {
		t.Fatal("unmasked reachable laptop accepted")
	}
	f.mask = "masked"
	f.selfActive = true
	if err := b.fence(context.Background(), c, s); err == nil {
		t.Fatal("running updater accepted")
	}
	f.selfActive = false
	f.controllerTimer = true
	if err := b.timers(context.Background(), c, false); err == nil {
		t.Fatal("controller pull timer accepted")
	}
}
func TestCriterion7StoppedBackupDrillAndFreshness(t *testing.T) {
	for _, fault := range []string{"", "command", "integrity", "fk"} {
		t.Run(fault, func(t *testing.T) {
			b, f, c, s := backendFixture(t)
			fencedFake(f)
			f.backupFault = fault
			err := b.backup(context.Background(), c, s)
			if (err != nil) != (fault != "") {
				t.Fatalf("backup result: %v", err)
			}
			if fault != "" && !s.BackupCreated.IsZero() {
				t.Fatal("failed drill recorded")
			}
			if fault == "" {
				if s.BackupSnapshot == "" || s.BackupScratch == "" {
					t.Fatal("snapshot evidence missing")
				}
				seedAssets(t, s)
				s.BackupCreated = b.Now().Add(-time.Hour - time.Second)
				if err = b.host(context.Background(), c, c.CoordinatorHost, s); err == nil {
					t.Fatal("stale backup allowed coordinator migration")
				}
			}
		})
	}
	for _, required := range []string{"stop t3-steward.service t3-steward-worker.service", "cannot inspect live process", "st.st_dev, st.st_ino", "wal_checkpoint(TRUNCATE)", "backlog backup verify", "backlog backup restore", "foreign_key_check", "integrity_check", "prod_inode", "T3_STEWARD_*", "trap '"} {
		if !strings.Contains(legacyBackupScript, required) {
			t.Fatalf("missing stopped drill guard %s", required)
		}
	}
	if strings.Contains(legacyBackupScript, "/mnt/backup") {
		t.Fatal("backup relies on NAS")
	}
}
func TestCriterion8ProductionCanaryReadinessAndEpoch(t *testing.T) {
	for _, fault := range []bool{false, true} {
		t.Run(fmt.Sprint(fault), func(t *testing.T) {
			b, f, c, s := backendFixture(t)
			fencedFake(f)
			seedAssets(t, s)
			seedPublication(t, c, s)
			f.epochDrift = fault
			f.reconnect = 5
			err := b.host(context.Background(), c, c.Canary, s)
			if (err != nil) != fault {
				t.Fatalf("host result %v", err)
			}
			if !fault {
				if f.postCalls < 7 {
					t.Fatal("readiness sampled only once")
				}
				if err = b.observe(context.Background(), c, s); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	b, f, c, s := backendFixture(t)
	fencedFake(f)
	seedAssets(t, s)
	if err := b.host(context.Background(), c, c.LaptopHost, s); err == nil {
		t.Fatal("excluded laptop targeted")
	}
	pre := f.snapshot(c.Canary)
	f.upgraded[c.Canary] = true
	post := f.snapshot(c.Canary)
	for _, field := range []string{"binary_sha256", "t3-steward.service.MainPID", "t3-steward-worker.service.MainPID", "t3code.service.MainPID"} {
		bad := map[string]string{}
		for k, v := range post {
			bad[k] = v
		}
		if field == "binary_sha256" {
			bad[field] = "bad"
		} else if field == "t3code.service.MainPID" {
			bad[field] = "changed"
		} else {
			bad[field] = pre[field]
		}
		if err := snapshotGate(pre, bad, c.Version, map[string]string{"linux_amd64": digest([]byte("linux_amd64"))}); err == nil {
			t.Fatalf("failed generation field %s accepted", field)
		}
	}
}
func TestCriterion9ProductionImmutableRollbackAndPriorState(t *testing.T) {
	b, f, c, s := backendFixture(t)
	seedPublication(t, c, s)
	fencedFake(f)
	seedAssets(t, s)
	if err := b.host(context.Background(), c, c.Canary, s); err != nil {
		t.Fatal(err)
	}
	if err := b.Execute(context.Background(), c, "rollback:"+c.Canary, s); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range f.calls {
		if strings.HasPrefix(call, "upkeeper rollback") && !strings.Contains(call, "--dry-run") {
			found = true
			if !strings.Contains(call, "--to "+c.PreviousRelease+" --hosts "+c.Canary+" --components t3-steward") {
				t.Fatal("rollback not pinned/scoped")
			}
		}
	}
	if !found || f.upgraded[c.Canary] {
		t.Fatal("immutable rollback missing")
	}
}
