package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/operatorhold"
	"github.com/iryzhkov/UpKeeper/internal/transport"
)

var operatorHoldNow = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// operatorHoldFleet is a controller (omarchy-pc) and the homes of the hosts it
// holds. A remote host's half runs the real `__hold` command in this process
// with HOME pointed at that host's home, so the test exercises the protocol
// without SSH. A host listed in refuse answers as an old binary would.
type operatorHoldFleet struct {
	controller string
	remote     map[string]string
	refuse     map[string]bool
	invoked    []string
}

func newOperatorHoldFleet(t *testing.T) *operatorHoldFleet {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ConfigRootEnvironment, root)
	fleet := &operatorHoldFleet{
		controller: t.TempDir(),
		remote:     map[string]string{},
		refuse:     map[string]bool{},
	}
	t.Setenv("HOME", fleet.controller)
	previousLocal, previousInvoke, previousClock := holdIsLocal, holdInvoke, holdClock
	holdIsLocal = func(spec inventory.HostSpec) bool { return spec.Name == "omarchy-pc" }
	holdInvoke = func(spec inventory.HostSpec, input string) transport.Outcome {
		fleet.invoked = append(fleet.invoked, spec.Name)
		if fleet.refuse[spec.Name] {
			return transport.Outcome{Code: exitcode.Usage, Stderr: "unrecognized arguments"}
		}
		home, known := fleet.remote[spec.Name]
		if !known {
			home = t.TempDir()
			fleet.remote[spec.Name] = home
		}
		controller := os.Getenv("HOME")
		os.Setenv("HOME", home)
		defer os.Setenv("HOME", controller)
		var out, errs bytes.Buffer
		code := Main("upkeeper", []string{holdAgentCommand},
			Streams{In: strings.NewReader(input), Out: &out, Err: &errs})
		return transport.Outcome{Code: code, Stdout: out.String(), Stderr: errs.String(), Transport: "ssh"}
	}
	holdClock = func() time.Time { return operatorHoldNow }
	t.Cleanup(func() { holdIsLocal, holdInvoke, holdClock = previousLocal, previousInvoke, previousClock })
	return fleet
}

func decodeDocument(t *testing.T, out string) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("not a JSON document: %v\n%s", err, out)
	}
	return document
}

// hold set records the hold on this host for every named host and in each
// remote host's own state, so its self-pull timer honours it too; show lists
// reason, setter and expiry; clear releases both copies; every step is logged.
func TestHoldSetRecordsTheHoldHereAndOnEachHeldHost(t *testing.T) {
	fleet := newOperatorHoldFleet(t)
	code, out, errs := runMain(t, "hold", "set", "--hosts", "omarchy-pc", "homelab",
		"--reason", "rc120 canary first", "--until", "36h", "--json")
	if code != exitcode.Success {
		t.Fatalf("exit %d, stdout=%s stderr=%s", code, out, errs)
	}
	document := decodeDocument(t, out)
	hosts := document["hosts"].([]any)
	if len(hosts) != 2 || hosts[0].(map[string]any)["delivery"] != "local" ||
		hosts[1].(map[string]any)["delivery"] != "delivered" || hosts[1].(map[string]any)["remote_result"] != "set" {
		t.Fatalf("hosts = %#v", hosts)
	}
	if strings.Join(fleet.invoked, ",") != "homelab" {
		t.Errorf("reached %v; only the remote host is reached", fleet.invoked)
	}

	until := operatorHoldNow.Add(36 * time.Hour)
	local := operatorhold.Directory(fleet.controller)
	remote := operatorhold.Directory(fleet.remote["homelab"])
	for _, place := range []struct{ directory, host string }{
		{local, "omarchy-pc"}, {local, "homelab"}, {remote, "homelab"},
	} {
		hold, present, err := operatorhold.Read(place.directory, place.host)
		if err != nil || !present {
			t.Fatalf("%s has no hold for %s: %v", place.directory, place.host, err)
		}
		if hold.Reason != "rc120 canary first" || !hold.SetAt.Equal(operatorHoldNow) || !hold.Until.Equal(until) {
			t.Errorf("hold for %s in %s = %#v", place.host, place.directory, hold)
		}
	}

	code, out, _ = runMain(t, "hold", "show", "--json")
	if code != exitcode.Success {
		t.Fatalf("show exit %d", code)
	}
	shown := decodeDocument(t, out)["operator_holds"].([]any)
	if len(shown) != 2 {
		t.Fatalf("show = %#v", shown)
	}
	entry := shown[0].(map[string]any)
	if entry["host"] != "homelab" || entry["reason"] != "rc120 canary first" ||
		entry["set_by"] != operatorhold.UserName() || entry["until"] != "2026-10-09T21:00:00Z" || entry["active"] != true {
		t.Errorf("shown hold = %#v", entry)
	}
	code, out, _ = runMain(t, "hold", "show")
	if code != exitcode.Success || !strings.Contains(out, "homelab: operator hold (held) set by") ||
		!strings.Contains(out, "reason: rc120 canary first") ||
		!strings.Contains(out, "clear with: upkeeper hold clear --hosts homelab --reason TEXT") {
		t.Errorf("show text = %q", out)
	}

	code, out, errs = runMain(t, "hold", "clear", "--hosts", "homelab", "--reason", "canary green")
	if code != exitcode.Success || !strings.Contains(out, "homelab: released here and on homelab (cleared)") {
		t.Fatalf("clear exit %d, stdout=%q stderr=%q", code, out, errs)
	}
	for _, directory := range []string{local, remote} {
		if _, held := operatorhold.Check(directory, "homelab", operatorHoldNow); held {
			t.Errorf("homelab still held in %s", directory)
		}
		raw, err := os.ReadFile(operatorhold.LogPath(directory, "homelab"))
		if err != nil || !strings.Contains(string(raw), `"event":"set"`) || !strings.Contains(string(raw), `"reason":"canary green"`) {
			t.Errorf("log in %s = %q %v", directory, raw, err)
		}
	}
	if _, held := operatorhold.Check(local, "omarchy-pc", operatorHoldNow); !held {
		t.Error("clearing homelab released omarchy-pc")
	}
}

// A host that cannot record the hold keeps only this host's copy, and the run
// says so instead of claiming the host is held.
func TestHoldSetReportsAHostItCouldNotReach(t *testing.T) {
	fleet := newOperatorHoldFleet(t)
	fleet.refuse["normandy"] = true
	code, out, errs := runMain(t, "hold", "set", "--hosts", "normandy", "--reason", "release day")
	if code != exitcode.Partial {
		t.Fatalf("exit %d, stdout=%q stderr=%q", code, out, errs)
	}
	if !strings.Contains(out, "normandy: held here only") || !strings.Contains(out, "predates operator holds") {
		t.Errorf("stdout = %q", out)
	}
	if _, held := operatorhold.Check(operatorhold.Directory(fleet.controller), "normandy", operatorHoldNow); !held {
		t.Error("the controller's own copy was not kept")
	}
}

func TestHoldSetAndClearRefuseMalformedRequests(t *testing.T) {
	newOperatorHoldFleet(t)
	for _, argv := range [][]string{
		{"hold", "set", "--reason", "x"},
		{"hold", "set", "--hosts", "homelab"},
		{"hold", "set", "homelab", "--reason", "x"},
		{"hold", "clear", "laptop", "--hosts", "homelab", "--reason", "x"},
		{"hold", "clear", "--hosts", "homelab", "--until", "1h", "--reason", "x"},
		{"hold", "set", "--hosts", "homelab", "--reason", "x", "--until", "2026-10-08T08:00:00Z"},
		{"hold", "set", "--hosts", "homelab", "--reason", "x", "--until", "tomorrow"},
		{"hold", "set", "--hosts", "nosuchhost", "--reason", "x"},
		{"hold", "set", "--hosts", "homelab", "homelab", "--reason", "x"},
	} {
		code, out, errs := runMain(t, argv...)
		if code != exitcode.Usage {
			t.Errorf("%v: exit %d, stdout=%q stderr=%q", argv, code, out, errs)
		}
	}
	if _, err := os.Stat(operatorhold.Directory(os.Getenv("HOME"))); err == nil {
		t.Error("a refused request created hold state")
	}
}

// An explicit pull to a held host is refused before anything is dispatched;
// --override-hold is pull's own flag and rollback does not take it.
func TestAnExplicitPullToAHeldHostIsRefusedUnlessOverridden(t *testing.T) {
	fleet := newOperatorHoldFleet(t)
	if err := pullHoldRefusal([]string{"homelab", "normandy"}, operatorHoldNow); err != nil {
		t.Fatalf("nothing is held yet: %v", err)
	}
	if _, err := operatorhold.Set(operatorhold.Directory(fleet.controller), operatorhold.Hold{
		Host: "homelab", Reason: "rc120", SetBy: "igor", SetAt: operatorHoldNow,
	}); err != nil {
		t.Fatal(err)
	}

	// Simulated reboot: a new process knows nothing but the state directory.
	err := pullHoldRefusal([]string{"normandy", "homelab"}, operatorHoldNow.Add(48*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "operator hold on homelab set by igor") ||
		!strings.Contains(err.Error(), "--override-hold") || !strings.Contains(err.Error(), "hold clear --hosts HOST") {
		t.Fatalf("refusal = %v", err)
	}

	parsed, parseErr := parse([]string{"pull", "--hosts", "homelab", "--override-hold"}, []string{"t3"})
	if parseErr != nil || !parsed.overrideHold {
		t.Fatalf("pull --override-hold = %+v %v", parsed, parseErr)
	}
	if _, parseErr = parse([]string{"rollback", "--to", "abc", "--all", "--override-hold"}, nil); parseErr == nil {
		t.Error("rollback accepted --override-hold")
	}
}

// A self-pull on a held host refreshes nothing and updates no binary; the
// gate fails closed on a record it cannot read and ignores an expired one.
func TestTheSelfPullGateHonoursThisHostsHold(t *testing.T) {
	fleet := newOperatorHoldFleet(t)
	directory := operatorhold.Directory(fleet.controller)
	if _, held := selfPullHeld("omarchy-pc", operatorHoldNow); held {
		t.Fatal("held with no hold")
	}
	if _, err := operatorhold.Set(directory, operatorhold.Hold{
		Host: "omarchy-pc", Reason: "rc120", SetBy: "igor", SetAt: operatorHoldNow, Until: operatorHoldNow.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if hold, held := selfPullHeld("omarchy-pc", operatorHoldNow.Add(time.Minute)); !held || hold.Reason != "rc120" {
		t.Errorf("inside the window: %#v %v", hold, held)
	}
	if _, held := selfPullHeld("omarchy-pc", operatorHoldNow.Add(2*time.Hour)); held {
		t.Error("an expired hold still holds the self-pull")
	}
	if err := os.WriteFile(operatorhold.RecordPath(directory, "omarchy-pc"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, held := selfPullHeld("omarchy-pc", operatorHoldNow.Add(2*time.Hour)); !held {
		t.Error("an unreadable hold did not hold")
	}
}

func TestTheHoldAgentRefusesWhatItCannotRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, input := range []string{
		`not json`,
		`{"schema_version":2,"action":"set","host":"homelab","reason":"x","user":"igor","at":"2026-10-08T09:00:00Z","until":null}`,
		`{"schema_version":1,"action":"drop","host":"homelab","reason":"x","user":"igor","at":"2026-10-08T09:00:00Z","until":null}`,
		`{"schema_version":1,"action":"set","host":"../x","reason":"x","user":"igor","at":"2026-10-08T09:00:00Z","until":null}`,
		`{"schema_version":1,"action":"set","host":"homelab","reason":"x","user":"igor","at":"2026-10-08T09:00:00Z","until":null,"extra":1}`,
	} {
		var out, errs bytes.Buffer
		code := Main("upkeeper", []string{holdAgentCommand}, Streams{In: strings.NewReader(input), Out: &out, Err: &errs})
		if code != exitcode.Preflight || out.Len() != 0 {
			t.Errorf("%s: exit %d stdout=%q stderr=%q", input, code, out.String(), errs.String())
		}
	}
}
