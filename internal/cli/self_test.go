package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
	"github.com/iryzhkov/UpKeeper/internal/selfupdate"
)

func TestTheAfterUpdateOptionIsHiddenAndExact(t *testing.T) {
	if afterUpdateFlag != selfupdate.AfterUpdateFlag {
		t.Fatalf("the parser accepts %s and a self-update passes %s", afterUpdateFlag, selfupdate.AfterUpdateFlag)
	}
	commit := strings.Repeat("b", 40)
	parsed := parseOrFail(t, "self-pull", "--after-update", commit)
	if parsed.afterUpdate != commit || parsed.updateTransaction != "" {
		t.Errorf("afterUpdate %q transaction %q", parsed.afterUpdate, parsed.updateTransaction)
	}
	if transactionFlag != selfupdate.TransactionFlag {
		t.Fatalf("the parser accepts %s and a self-update passes %s", transactionFlag, selfupdate.TransactionFlag)
	}
	transaction := strings.Repeat("7", 32)
	parsed = parseOrFail(t, "self-pull", "--after-update", commit, "--update-transaction", transaction)
	if parsed.updateTransaction != transaction {
		t.Errorf("updateTransaction %q", parsed.updateTransaction)
	}
	for _, argv := range [][]string{
		{"self-pull", "--after-update", "abc1234"},
		{"self-pull", "--after-update", strings.Repeat("B", 40)},
		{"self-pull", "--after-update"},
		{"self-pull", "--after-update", commit, "--dry-run"},
		{"self-pull", "--converge", "--after-update", commit},
		{"scheduled-pull", "--after-update", commit},
		{"self-pull", "--update-transaction", strings.Repeat("7", 32)},
		{"self-pull", "--after-update", commit, "--update-transaction", "7777"},
		{"self-pull", "--after-update", commit, "--update-transaction", strings.Repeat("G", 32)},
	} {
		if _, err := parse(argv, release.ComponentOrder); err == nil {
			t.Errorf("%v was accepted", argv)
		}
	}
	if help := helpText("upkeeper", "self-pull"); strings.Contains(help, "after-update") ||
		strings.Contains(help, "update-transaction") {
		t.Error("the self-pull help documents the hidden option")
	}
}

// The agent's one option is the program fence, as the controller spells it.
func TestTheAgentTakesOnlyTheProgramFence(t *testing.T) {
	if agentProgramFlag != converge.AgentProgramFlag {
		t.Fatalf("the agent accepts %s and a controller sends %s", agentProgramFlag, converge.AgentProgramFlag)
	}
	commit := strings.Repeat("b", 40)
	if parsed := parseOrFail(t, "__agent", "--expect-program", commit); parsed.agentProgram != commit {
		t.Errorf("agentProgram %q", parsed.agentProgram)
	}
	if parsed := parseOrFail(t, "__agent"); parsed.agentProgram != "" {
		t.Errorf("agentProgram %q", parsed.agentProgram)
	}
	for _, argv := range [][]string{
		{"__agent", "--expect-program"},
		{"__agent", "--expect-program", "abc1234"},
		{"__agent", "--expect-program", commit, "--expect-program", commit},
		{"__agent", "--json"},
		{"__probe", "--expect-program", commit},
	} {
		if _, err := parse(argv, release.ComponentOrder); err == nil {
			t.Errorf("%v was accepted", argv)
		}
	}
}

func TestSelfCommandLines(t *testing.T) {
	digest := strings.Repeat("1", 64)
	parsed := parseOrFail(t, "self", "install", "--expect-sha256", digest, "--json")
	if parsed.selfAction != "install" || parsed.selfExpectSHA256 != digest || !parsed.asJSON {
		t.Errorf("parsed %+v", parsed)
	}
	for _, argv := range [][]string{
		{"self"},
		{"self", "upgrade"},
		{"self", "install"},
		{"self", "rollback", "--expect-sha256", digest},
		{"self", "status", "--all"},
	} {
		if _, err := parse(argv, release.ComponentOrder); err == nil {
			t.Errorf("%v was accepted", argv)
		}
	}
	if !strings.Contains(helpText("upkeeper", "self"), "--expect-sha256") {
		t.Error("the self help does not document --expect-sha256")
	}
}

// fakeUpdate is a seam whose every step is recorded and whose results are set
// by the test.
type fakeUpdate struct {
	pin        *release.ProgramPin
	verifyErr  map[string]error
	installErr error
	result     selfupdate.Result
	execErr    error
	head       string
	binary     string
	steps      []string
	execArgv   []string
	details    map[string]any
	held       string
}

func (f *fakeUpdate) seam() *selfUpdate {
	return &selfUpdate{
		loadPin: func(root, commit string) (*release.ProgramPin, error) {
			f.steps = append(f.steps, "load "+commit)
			return f.pin, nil
		},
		verify: func(root, binaryCommit, checkoutCommit string) error {
			f.steps = append(f.steps, "verify "+binaryCommit)
			return f.verifyErr[binaryCommit]
		},
		checkout:     func(string) (string, error) { return f.head, nil },
		binaryCommit: func() string { return f.binary },
		install: func(pin *release.ProgramPin) (selfupdate.Result, error) {
			f.steps = append(f.steps, "install "+pin.Tag)
			return f.result, f.installErr
		},
		rollback: func(transaction, reason string) (selfupdate.Result, error) {
			f.steps = append(f.steps, "rollback")
			return selfupdate.Result{Generation: "previous-generation"}, nil
		},
		generation: func() string { return "active-generation" },
		exec: func(path string, argv []string, env []string) error {
			f.steps = append(f.steps, "exec "+path)
			f.execArgv = argv
			return f.execErr
		},
		converge: func(root, commit string, details map[string]any) (map[string]any, int, error) {
			f.steps = append(f.steps, "converge "+commit)
			f.details = details
			return map[string]any{"run_id": "r"}, 0, nil
		},
		lockCheckout: func(string) (func(), error) {
			f.steps = append(f.steps, "lock")
			return func() { f.steps = append(f.steps, "unlock") }, nil
		},
		scheduledHold: func(string) (string, bool) { return f.held, f.held != "" },
	}
}

func testPin() *release.ProgramPin {
	return &release.ProgramPin{
		Repository: release.ProgramRepository, Tag: "v0.1.31", SourceCommit: strings.Repeat("b", 40),
		Toolchain: "go1.24.13", Assets: map[string]release.ProgramAsset{},
	}
}

func TestUpdateProgramRefusals(t *testing.T) {
	refreshed := strings.Repeat("c", 40)
	cases := []struct {
		name     string
		fake     fakeUpdate
		expected string
		steps    []string
	}{
		{"a pin that is not the gated program", fakeUpdate{pin: testPin(), verifyErr: map[string]error{
			strings.Repeat("b", 40): errors.New("differs")}}, "not the gated program",
			[]string{"verify " + strings.Repeat("b", 40)}},
		{"an install that refuses", fakeUpdate{pin: testPin(), installErr: failure.Value("has sha256 x")},
			"self-update to upkeeper v0.1.31 refused: has sha256 x",
			[]string{"verify " + strings.Repeat("b", 40), "install v0.1.31"}},
		{"an exec that fails after a switch", fakeUpdate{pin: testPin(), execErr: errors.New("ENOEXEC"),
			result: selfupdate.Result{Path: "/g/upkeeper", Switched: true}}, "rolled back to generation previous-generation",
			[]string{"verify " + strings.Repeat("b", 40), "install v0.1.31", "exec /g/upkeeper", "rollback"}},
		{"an exec that fails without a switch", fakeUpdate{pin: testPin(), execErr: errors.New("ENOEXEC"),
			result: selfupdate.Result{Path: "/g/upkeeper"}}, "cannot start",
			[]string{"verify " + strings.Repeat("b", 40), "install v0.1.31", "exec /g/upkeeper"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			replaced, err := test.fake.seam().updateProgram("/checkout", refreshed, test.fake.pin)
			if replaced || err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("replaced=%v error %v, want %q", replaced, err, test.expected)
			}
			if !failure.IsExpected(err) {
				t.Errorf("the refusal would exit 1 rather than 30: %v", err)
			}
			if !slices.Equal(test.fake.steps, test.steps) {
				t.Errorf("steps %v, want %v", test.fake.steps, test.steps)
			}
		})
	}
}

// Only a changed program is updated; every other refusal stays the refusal.
func TestOnlyAChangedProgramIsUpdated(t *testing.T) {
	previous := newSelfUpdate
	t.Cleanup(func() { newSelfUpdate = previous })
	newSelfUpdate = func() (*selfUpdate, error) {
		t.Fatal("a refusal that is not a changed program reached the self-update")
		return nil, nil
	}
	unidentified := failure.Value("this binary does not name a clean source commit")
	replaced, err := programUpdate("/checkout", "homelab", strings.Repeat("c", 40), unidentified, io.Discard)
	if replaced || err != unidentified {
		t.Fatalf("replaced=%v err=%v", replaced, err)
	}
}

// What a self-pull does with each answer of its program check.
func TestProgramUpdateDecisions(t *testing.T) {
	refreshed := strings.Repeat("c", 40)
	pinned := strings.Repeat("b", 40)
	differs := failure.Wrap(selfconv.ErrProgramDiffers, failure.KindValue, "program differs")
	switched := selfupdate.Result{Path: "/g/upkeeper", Switched: true}
	cases := []struct {
		name     string
		verified error
		fake     fakeUpdate
		replaced bool
		expected string // "" means no error
		notice   string
		steps    []string
	}{
		{"a changed program without a pin is refused", differs, fakeUpdate{binary: "a"},
			false, "pins no upkeeper binary", "", []string{"load " + refreshed}},
		{"a changed program is updated", differs, fakeUpdate{binary: "a", pin: testPin(), result: switched},
			true, "", "", []string{"load " + refreshed, "verify " + pinned, "install v0.1.31", "exec /g/upkeeper"}},
		{"a changed program on a held host is refused", differs, fakeUpdate{binary: "a", pin: testPin(), held: "/h/homelab.hold"},
			false, "scheduled hold /h/homelab.hold", "", []string{"load " + refreshed}},
		{"the pinned commit needs nothing", nil, fakeUpdate{binary: pinned, pin: testPin()},
			false, "", "", []string{"load " + refreshed}},
		{"the same program without a pin needs nothing", nil, fakeUpdate{binary: "a"},
			false, "", "", []string{"load " + refreshed}},
		// The fence on the SSH path compares commits, so a host left on
		// another commit of the same program would be refused by every pull.
		{"the same program from another commit is updated", nil, fakeUpdate{binary: "a", pin: testPin(), result: switched},
			true, "", "", []string{"load " + refreshed, "verify " + pinned, "install v0.1.31", "exec /g/upkeeper"}},
		{"a failed update of the same program still converges", nil,
			fakeUpdate{binary: "a", pin: testPin(), installErr: failure.Value("self-update is held")},
			false, "", "update to the pinned binary skipped", []string{"load " + refreshed, "verify " + pinned, "install v0.1.31"}},
		{"the same program on a held host converges without updating", nil,
			fakeUpdate{binary: "a", pin: testPin(), held: "/h/homelab.hold"},
			false, "", "", []string{"load " + refreshed}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fake := test.fake
			previous := newSelfUpdate
			t.Cleanup(func() { newSelfUpdate = previous })
			newSelfUpdate = func() (*selfUpdate, error) { return fake.seam(), nil }
			var notice bytes.Buffer
			replaced, err := programUpdate("/checkout", "homelab", refreshed, test.verified, &notice)
			if replaced != test.replaced {
				t.Errorf("replaced=%v", replaced)
			}
			switch {
			case test.expected == "" && err != nil:
				t.Errorf("error %v", err)
			case test.expected != "" && (err == nil || !strings.Contains(err.Error(), test.expected)):
				t.Errorf("error %v, want %q", err, test.expected)
			}
			if !strings.Contains(notice.String(), test.notice) || (test.notice == "" && notice.Len() != 0) {
				t.Errorf("notice %q, want %q", notice.String(), test.notice)
			}
			if !slices.Equal(fake.steps, test.steps) {
				t.Errorf("steps %v, want %v", fake.steps, test.steps)
			}
		})
	}
}

func TestAfterUpdateChecksTheCheckoutAndTheProgram(t *testing.T) {
	commit := strings.Repeat("c", 40)
	binary := strings.Repeat("b", 40)

	moved := &fakeUpdate{head: strings.Repeat("d", 40), binary: binary}
	if _, _, err := moved.seam().afterUpdate("/checkout", commit, ""); err == nil ||
		!strings.Contains(err.Error(), "moved") {
		t.Errorf("a moved checkout produced %v", err)
	}
	if !slices.Equal(moved.steps, []string{"lock", "unlock"}) {
		t.Errorf("a moved checkout did %v", moved.steps)
	}

	wrong := &fakeUpdate{head: commit, binary: binary, verifyErr: map[string]error{binary: errors.New("differs")}}
	if _, _, err := wrong.seam().afterUpdate("/checkout", commit, strings.Repeat("7", 32)); err == nil ||
		!strings.Contains(err.Error(), "rolled back to generation previous-generation") {
		t.Errorf("a wrong program produced %v", err)
	}
	if !slices.Equal(wrong.steps, []string{"lock", "verify " + binary, "rollback", "unlock"}) {
		t.Errorf("a wrong program did %v", wrong.steps)
	}

	right := &fakeUpdate{head: commit, binary: binary}
	if _, code, err := right.seam().afterUpdate("/checkout", commit, ""); err != nil || code != 0 {
		t.Fatalf("the updated binary did not converge: %d %v", code, err)
	}
	// The checkout stays locked from the check of HEAD through the program
	// check to the end of convergence, which is given exactly the commit
	// that was checked rather than reading HEAD again.
	if !slices.Equal(right.steps, []string{"lock", "verify " + binary, "converge " + commit, "unlock"}) {
		t.Errorf("steps %v", right.steps)
	}
	if right.details["after_update"] != commit || right.details["binary_commit"] != binary ||
		right.details["generation"] != "active-generation" {
		t.Errorf("self-update details %v", right.details)
	}
}

// The acceptance path through the real installer: a host running program A
// with a release pinning program B downloads B from the release, verifies it,
// switches to it and re-executes it with --after-update and the gated commit.
func TestAChangedProgramSelfUpdatesAndReexecutes(t *testing.T) {
	commit := strings.Repeat("c", 40)
	body := []byte("#!/bin/sh\necho 'upkeeper v0.1.31 (commit " + strings.Repeat("b", 40) + ")'\n")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/iryzhkov/UpKeeper/releases/download/v0.1.31/upkeeper-linux-amd64" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(body)
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	binary := filepath.Join(home, ".local/bin/upkeeper")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'upkeeper v0.1.30 (commit "+
		strings.Repeat("a", 40)+")'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	installer := &selfupdate.Installer{
		Layout: selfupdate.Layout{Home: home}, Client: server.Client(), BaseURL: server.URL,
		Target: "linux-amd64",
	}
	pin := testPin()
	pin.Assets["linux-amd64"] = release.ProgramAsset{Asset: "upkeeper-linux-amd64", SHA256: digest, Size: int64(len(body))}
	fake := &fakeUpdate{pin: pin}
	seam := fake.seam()
	seam.install = installer.Apply
	seam.rollback = installer.UndoSwitch
	previous := newSelfUpdate
	t.Cleanup(func() { newSelfUpdate = previous })
	newSelfUpdate = func() (*selfUpdate, error) { return seam, nil }

	refusal := failure.Wrap(selfconv.ErrProgramDiffers, failure.KindValue, "program differs")
	replaced, err := programUpdate("/checkout", "homelab", commit, refusal, io.Discard)
	if err != nil || !replaced {
		t.Fatalf("replaced=%v err=%v steps=%v", replaced, err, fake.steps)
	}
	generation := filepath.Join(home, ".local/lib/upkeeper/generations", digest, "upkeeper")
	if len(fake.execArgv) != 6 ||
		!slices.Equal(fake.execArgv[:5], []string{generation, "self-pull", "--after-update", commit, "--update-transaction"}) ||
		!transactionPattern.MatchString(fake.execArgv[5]) {
		t.Errorf("re-exec argv %v", fake.execArgv)
	}
	if selfupdate.ActiveGeneration(home) != digest {
		t.Errorf("active generation %q", selfupdate.ActiveGeneration(home))
	}
}

// realUpdate is a host on an adopted program A, a release server for program
// B, and a seam whose installation steps are the real installer's.
func realUpdate(t *testing.T) (*selfupdate.Installer, *fakeUpdate, *selfUpdate, *release.ProgramPin, string) {
	t.Helper()
	body := []byte("#!/bin/sh\necho 'upkeeper v0.1.31 (commit " + strings.Repeat("b", 40) + ")'\n")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(body)
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	binary := filepath.Join(home, ".local/bin/upkeeper")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'upkeeper v0.1.30 (commit "+
		strings.Repeat("a", 40)+")'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	installer := &selfupdate.Installer{
		Layout: selfupdate.Layout{Home: home}, Client: server.Client(), BaseURL: server.URL,
		Target: "linux-amd64",
	}
	pin := testPin()
	pin.Assets["linux-amd64"] = release.ProgramAsset{Asset: "upkeeper-linux-amd64", SHA256: digest, Size: int64(len(body))}
	fake := &fakeUpdate{pin: pin, binary: strings.Repeat("a", 40)}
	seam := fake.seam()
	seam.install = installer.Apply
	seam.rollback = installer.UndoSwitch
	return installer, fake, seam, pin, digest
}

// An operator who rolls back while a self-update's re-exec is pending has
// the last word: when that re-exec then fails, the automatic rollback finds
// the installation changed since its own switch and leaves it alone, rather
// than swapping current and previous again and reactivating the generation
// the operator rejected.
func TestAnOperatorRollbackDuringTheReexecIsKept(t *testing.T) {
	installer, fake, seam, pin, digest := realUpdate(t)
	var chosen string
	seam.exec = func(path string, argv []string, env []string) error {
		fake.execArgv = argv
		restored, err := installer.Rollback("operator while the re-exec is pending")
		if err != nil {
			t.Fatalf("operator rollback: %v", err)
		}
		chosen = restored.Generation
		return errors.New("exec failed")
	}
	replaced, err := seam.updateProgram("/checkout", strings.Repeat("c", 40), pin)
	if replaced || err == nil {
		t.Fatalf("a failed re-exec was not refused: replaced=%v err=%v", replaced, err)
	}
	if !failure.IsExpected(err) {
		t.Errorf("the refusal would exit 1 rather than 30: %v", err)
	}
	if chosen == "" || chosen == digest {
		t.Fatalf("the operator's rollback restored %q", chosen)
	}
	if current := installer.Layout.CurrentGeneration(); current != chosen {
		t.Fatalf("current is %s; the operator chose %s and the update had switched to %s", current, chosen, digest)
	}
	if !strings.Contains(err.Error(), "changed") {
		t.Errorf("the refusal does not say the installation changed: %v", err)
	}
}

// The same holds for the re-executed binary's own program check: an operator
// rollback between the switch and that check is not undone by it.
func TestAnOperatorRollbackBeforeThePostUpdateCheckIsKept(t *testing.T) {
	installer, fake, seam, pin, digest := realUpdate(t)
	commit := strings.Repeat("c", 40)
	result, err := installer.Apply(pin)
	if err != nil || result.Generation != digest {
		t.Fatalf("apply %+v, %v", result, err)
	}
	restored, err := installer.Rollback("operator")
	if err != nil {
		t.Fatal(err)
	}
	fake.head = commit
	fake.binary = strings.Repeat("b", 40)
	fake.verifyErr = map[string]error{fake.binary: errors.New("differs")}
	if _, _, err := seam.afterUpdate("/checkout", commit, result.Transaction); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("the post-update check produced %v", err)
	}
	if current := installer.Layout.CurrentGeneration(); current != restored.Generation {
		t.Fatalf("current is %s; the operator chose %s", current, restored.Generation)
	}
	if slices.Contains(fake.steps, "converge "+commit) {
		t.Errorf("a refused binary converged: %v", fake.steps)
	}
}

func TestSelfStatusAndARollbackWithNothingToRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errs bytes.Buffer
	if code := Main("upkeeper", []string{"self", "status"}, Streams{Out: &out, Err: &errs}); code != 0 {
		t.Fatalf("status exit %d: %s", code, errs.String())
	}
	if !strings.Contains(out.String(), "does not exist") {
		t.Errorf("status output %q", out.String())
	}
	out.Reset()
	errs.Reset()
	if code := Main("upkeeper", []string{"self", "rollback"}, Streams{Out: &out, Err: &errs}); code != exitcode.Preflight {
		t.Fatalf("rollback exit %d", code)
	}
	if !strings.Contains(errs.String(), "nothing to roll back") {
		t.Errorf("rollback refusal %q", errs.String())
	}
	out.Reset()
	errs.Reset()
	if code := Main("upkeeper", []string{"self", "install", "--expect-sha256", strings.Repeat("1", 64)},
		Streams{Out: &out, Err: &errs}); code != exitcode.Preflight {
		t.Fatalf("installing a test binary exit %d", code)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local/bin/upkeeper")); err == nil {
		t.Error("a refused install changed ~/.local/bin/upkeeper")
	}
}
