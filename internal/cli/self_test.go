package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if parsed.afterUpdate != commit {
		t.Errorf("afterUpdate %q", parsed.afterUpdate)
	}
	for _, argv := range [][]string{
		{"self-pull", "--after-update", "abc1234"},
		{"self-pull", "--after-update", strings.Repeat("B", 40)},
		{"self-pull", "--after-update"},
		{"self-pull", "--after-update", commit, "--dry-run"},
		{"self-pull", "--converge", "--after-update", commit},
		{"scheduled-pull", "--after-update", commit},
	} {
		if _, err := parse(argv, release.ComponentOrder); err == nil {
			t.Errorf("%v was accepted", argv)
		}
	}
	if strings.Contains(helpText("upkeeper", "self-pull"), "after-update") {
		t.Error("the self-pull help documents the hidden option")
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
		rollback: func(reason string) (selfupdate.Result, error) {
			f.steps = append(f.steps, "rollback")
			return selfupdate.Result{Generation: "previous-generation"}, nil
		},
		generation: func() string { return "active-generation" },
		exec: func(path string, argv []string, env []string) error {
			f.steps = append(f.steps, "exec "+path)
			f.execArgv = argv
			return f.execErr
		},
		converge: func(root string, details map[string]any) (map[string]any, int, error) {
			f.steps = append(f.steps, "converge")
			f.details = details
			return map[string]any{"run_id": "r"}, 0, nil
		},
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
	refusal := failure.Wrap(selfconv.ErrProgramDiffers, failure.KindValue, "program differs")
	cases := []struct {
		name     string
		fake     fakeUpdate
		expected string
		steps    []string
	}{
		{"a release that pins no binary", fakeUpdate{}, "pins no upkeeper binary",
			[]string{"load " + refreshed}},
		{"a pin that is not the gated program", fakeUpdate{pin: testPin(), verifyErr: map[string]error{
			strings.Repeat("b", 40): errors.New("differs")}}, "not the gated program",
			[]string{"load " + refreshed, "verify " + strings.Repeat("b", 40)}},
		{"an install that refuses", fakeUpdate{pin: testPin(), installErr: failure.Value("has sha256 x")},
			"self-update to upkeeper v0.1.31 refused: has sha256 x",
			[]string{"load " + refreshed, "verify " + strings.Repeat("b", 40), "install v0.1.31"}},
		{"an exec that fails after a switch", fakeUpdate{pin: testPin(), execErr: errors.New("ENOEXEC"),
			result: selfupdate.Result{Path: "/g/upkeeper", Switched: true}}, "rolled back to generation previous-generation",
			[]string{"load " + refreshed, "verify " + strings.Repeat("b", 40), "install v0.1.31", "exec /g/upkeeper", "rollback"}},
		{"an exec that fails without a switch", fakeUpdate{pin: testPin(), execErr: errors.New("ENOEXEC"),
			result: selfupdate.Result{Path: "/g/upkeeper"}}, "cannot start",
			[]string{"load " + refreshed, "verify " + strings.Repeat("b", 40), "install v0.1.31", "exec /g/upkeeper"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			replaced, err := test.fake.seam().updateProgram("/checkout", refreshed, refusal)
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
	replaced, err := programUpdate("/checkout", strings.Repeat("c", 40), unidentified)
	if replaced || err != unidentified {
		t.Fatalf("replaced=%v err=%v", replaced, err)
	}
}

func TestAfterUpdateChecksTheCheckoutAndTheProgram(t *testing.T) {
	commit := strings.Repeat("c", 40)
	binary := strings.Repeat("b", 40)

	moved := &fakeUpdate{head: strings.Repeat("d", 40), binary: binary}
	if _, _, err := moved.seam().afterUpdate("/checkout", commit); err == nil ||
		!strings.Contains(err.Error(), "moved") {
		t.Errorf("a moved checkout produced %v", err)
	}
	if slices.Contains(moved.steps, "rollback") || slices.Contains(moved.steps, "converge") {
		t.Errorf("a moved checkout did %v", moved.steps)
	}

	wrong := &fakeUpdate{head: commit, binary: binary, verifyErr: map[string]error{binary: errors.New("differs")}}
	if _, _, err := wrong.seam().afterUpdate("/checkout", commit); err == nil ||
		!strings.Contains(err.Error(), "rolled back to generation previous-generation") {
		t.Errorf("a wrong program produced %v", err)
	}
	if !slices.Equal(wrong.steps, []string{"verify " + binary, "rollback"}) {
		t.Errorf("a wrong program did %v", wrong.steps)
	}

	right := &fakeUpdate{head: commit, binary: binary}
	if _, code, err := right.seam().afterUpdate("/checkout", commit); err != nil || code != 0 {
		t.Fatalf("the updated binary did not converge: %d %v", code, err)
	}
	if !slices.Equal(right.steps, []string{"verify " + binary, "converge"}) {
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
	seam.rollback = installer.Rollback
	previous := newSelfUpdate
	t.Cleanup(func() { newSelfUpdate = previous })
	newSelfUpdate = func() (*selfUpdate, error) { return seam, nil }

	refusal := failure.Wrap(selfconv.ErrProgramDiffers, failure.KindValue, "program differs")
	replaced, err := programUpdate("/checkout", commit, refusal)
	if err != nil || !replaced {
		t.Fatalf("replaced=%v err=%v steps=%v", replaced, err, fake.steps)
	}
	generation := filepath.Join(home, ".local/lib/upkeeper/generations", digest, "upkeeper")
	if !slices.Equal(fake.execArgv, []string{generation, "self-pull", "--after-update", commit}) {
		t.Errorf("re-exec argv %v", fake.execArgv)
	}
	if selfupdate.ActiveGeneration(home) != digest {
		t.Errorf("active generation %q", selfupdate.ActiveGeneration(home))
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
	if !strings.Contains(errs.String(), "no previous upkeeper generation") {
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
