package units

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// These are the canary drop-ins scripts/*.service.d shipped until the Python
// units were retired, byte for byte. The test keeps its own copy so that a
// change to the installer's accepted bytes cannot pass unnoticed.
const (
	historicalSelfDropIn = "# Canary drop-in: run the compiled UpKeeper binary instead of the Python\n" +
		"# launcher, without editing the committed unit.\n" +
		"#\n" +
		"# Install it as ~/.config/systemd/user/upkeeper-self.service.d/10-upkeeper-binary.conf\n" +
		"# and run `systemctl --user daemon-reload`. Rolling back is `rm` of this file\n" +
		"# plus another daemon-reload: the committed unit and scripts/upkeeper-self-pull\n" +
		"# are never touched, so the Python path stays exactly one command away.\n" +
		"[Service]\n" +
		"ExecStart=\n" +
		"ExecStart=%h/.local/bin/upkeeper self-pull\n"
	historicalSelfDropInSHA256 = "4e466672a17fbd9e886119f9c6f1b62ad56e517f7c02bedd806181400022dd0a"

	historicalPullDropIn = "# Canary drop-in for the controller timer on normandy: run the compiled\n" +
		"# UpKeeper binary instead of the Python launcher, without editing the committed\n" +
		"# unit.\n" +
		"#\n" +
		"# Install it as ~/.config/systemd/user/upkeeper-pull.service.d/10-upkeeper-binary.conf\n" +
		"# and run `systemctl --user daemon-reload`. Rolling back is `rm` of this file\n" +
		"# plus another daemon-reload.\n" +
		"[Service]\n" +
		"ExecStart=\n" +
		"ExecStart=%h/.local/bin/upkeeper scheduled-pull\n"
	historicalPullDropInSHA256 = "9c9db7f1322c079f65aec263fc5de39e0d8c10f0ac41e82cfd81b966ef2da0da"
)

type legacyCase struct {
	name        string
	definitions []Definition
	timer       string
	service     string
	dropIn      string
}

func legacyCases() []legacyCase {
	return []legacyCase{
		{"self", SelfTimer(), "upkeeper-self.timer", "upkeeper-self.service", historicalSelfDropIn},
		{"pull", FleetTimer(), "upkeeper-pull.timer", "upkeeper-pull.service", historicalPullDropIn},
	}
}

func systemdUserDirectory(home string) string {
	return filepath.Join(home, ".config", "systemd", "user")
}

func writeDropIn(t *testing.T, home, service, name, content string) string {
	t.Helper()
	directory := filepath.Join(systemdUserDirectory(home), service+".d")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// recordingRunner answers every systemctl call with success and remembers it.
func recordingRunner(calls *[]string) procx.Runner {
	return func(command procx.Command) procx.Result {
		*calls = append(*calls, strings.Join(command.Argv, " "))
		return procx.Result{Code: 0}
	}
}

func assertInstalled(t *testing.T, home string, definitions []Definition) {
	t.Helper()
	for _, definition := range definitions {
		content, err := os.ReadFile(filepath.Join(systemdUserDirectory(home), definition.Name))
		if err != nil {
			t.Fatalf("%s was not installed: %v", definition.Name, err)
		}
		if string(content) != definition.Content {
			t.Errorf("%s is not the embedded definition:\n%s", definition.Name, content)
		}
	}
}

func assertNothingInstalled(t *testing.T, home string, definitions []Definition) {
	t.Helper()
	for _, definition := range definitions {
		if _, err := os.Lstat(filepath.Join(systemdUserDirectory(home), definition.Name)); err == nil {
			t.Errorf("the installer wrote %s despite refusing", definition.Name)
		}
	}
}

// The installer accepts exactly the shipped bytes for each unit and nothing
// else.
func TestTheInstallerAcceptsExactlyTheHistoricalCanaryBytes(t *testing.T) {
	want := map[string][]string{
		"upkeeper-self.service": {historicalSelfDropIn},
		"upkeeper-pull.service": {historicalPullDropIn},
	}
	if len(legacyDropIns) != len(want) {
		t.Fatalf("the installer knows legacy drop-ins for %d units, want %d",
			len(legacyDropIns), len(want))
	}
	for unit, contents := range want {
		if strings.Join(legacyDropIns[unit], "\x00") != strings.Join(contents, "\x00") {
			t.Errorf("the installer's legacy drop-in for %s is not the shipped bytes", unit)
		}
	}
}

func TestTheTestCopiesAreTheHistoricalCanaryBytes(t *testing.T) {
	for content, digest := range map[string]string{
		historicalSelfDropIn: historicalSelfDropInSHA256,
		historicalPullDropIn: historicalPullDropInSHA256,
	} {
		sum := sha256.Sum256([]byte(content))
		if hex.EncodeToString(sum[:]) != digest {
			t.Errorf("the test copy does not hash to %s", digest)
		}
	}
}

// A host still carrying the drop-in UpKeeper itself shipped is moved onto the
// Go base unit: the unit is written, the drop-in and its emptied directory are
// removed, and systemd is reloaded and the timer enabled.
func TestInstallReconcilesTheLegacyCanaryDropIn(t *testing.T) {
	for _, tc := range legacyCases() {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dropIn := writeDropIn(t, home, tc.service, "10-upkeeper-binary.conf", tc.dropIn)

			var calls []string
			done, err := install(tc.definitions, tc.timer, recordingRunner(&calls))
			if err != nil {
				t.Fatalf("the installer refused UpKeeper's own legacy drop-in: %v", err)
			}
			assertInstalled(t, home, tc.definitions)
			wantRemoved := []string{dropIn, filepath.Dir(dropIn)}
			if strings.Join(done.Removed, "\n") != strings.Join(wantRemoved, "\n") {
				t.Errorf("reported removed = %q, want %q", done.Removed, wantRemoved)
			}
			if len(done.Written) != len(tc.definitions) {
				t.Errorf("reported written = %q", done.Written)
			}
			if _, err := os.Lstat(dropIn); !os.IsNotExist(err) {
				t.Errorf("the legacy drop-in is still there: %v", err)
			}
			if _, err := os.Lstat(filepath.Dir(dropIn)); !os.IsNotExist(err) {
				t.Errorf("the emptied drop-in directory is still there: %v", err)
			}
			want := []string{
				"systemctl --user daemon-reload",
				"systemctl --user enable --now " + tc.timer,
			}
			if strings.Join(calls, "\n") != strings.Join(want, "\n") {
				t.Errorf("systemctl calls = %q, want %q", calls, want)
			}
		})
	}
}

// The legacy drop-in directory may hold files systemd ignores; those are left
// alone, and so is the directory that still holds them.
func TestInstallKeepsADropInDirectoryThatIsNotEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dropIn := writeDropIn(t, home, "upkeeper-self.service", "10-upkeeper-binary.conf",
		historicalSelfDropIn)
	notes := writeDropIn(t, home, "upkeeper-self.service", "README", "operator notes\n")

	var calls []string
	if _, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Lstat(dropIn); !os.IsNotExist(err) {
		t.Errorf("the legacy drop-in is still there: %v", err)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("a file that is not a drop-in was removed: %v", err)
	}
}

// Anything that is not byte-for-byte a drop-in UpKeeper shipped for that very
// unit belongs to somebody else. The installer refuses before writing or
// removing anything, names the file to inspect, and no longer describes
// deleting a drop-in as a rollback.
func TestInstallRefusesADropInItDidNotShip(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
	}{
		{"modified legacy drop-in", "10-upkeeper-binary.conf",
			historicalSelfDropIn + "Environment=EXTRA=1\n"},
		{"unknown drop-in", "20-local.conf", "[Service]\nNice=5\n"},
		{"legacy bytes under another name", "20-upkeeper-binary.conf", historicalSelfDropIn},
		{"the controller's drop-in on the host unit", "10-upkeeper-binary.conf",
			historicalPullDropIn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			foreign := writeDropIn(t, home, "upkeeper-self.service", tc.file, tc.content)

			var calls []string
			_, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls))
			if err == nil {
				t.Fatal("the installer accepted a drop-in it did not ship")
			}
			if !strings.Contains(err.Error(), foreign) {
				t.Errorf("the refusal does not name the file to inspect (%s): %v", foreign, err)
			}
			// The temporary home is part of the message and may contain anything.
			prose := strings.ReplaceAll(err.Error(), home, "~")
			if strings.Contains(strings.ToLower(prose), "roll") {
				t.Errorf("the refusal still promises a drop-in rollback: %v", err)
			}
			if len(calls) != 0 {
				t.Errorf("the installer ran %q despite refusing", calls)
			}
			assertNothingInstalled(t, home, SelfTimer())
			if content, err := os.ReadFile(foreign); err != nil || string(content) != tc.content {
				t.Errorf("the foreign drop-in was changed: %v", err)
			}
		})
	}
}

// A legacy drop-in next to a foreign one is not removed either: the refusal
// happens before any effect, so the host is exactly as the operator left it.
func TestInstallRemovesNothingWhenAnotherDropInRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := writeDropIn(t, home, "upkeeper-self.service", "10-upkeeper-binary.conf",
		historicalSelfDropIn)
	foreign := writeDropIn(t, home, "upkeeper-self.timer", "override.conf", "[Timer]\n")

	var calls []string
	_, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls))
	if err == nil || !strings.Contains(err.Error(), foreign) {
		t.Fatalf("the installer did not refuse the timer drop-in by name: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("the legacy drop-in was removed although the install refused: %v", err)
	}
	assertNothingInstalled(t, home, SelfTimer())
	if len(calls) != 0 {
		t.Errorf("the installer ran %q despite refusing", calls)
	}
}

// A removal that fails after the base unit was written stops before reloading
// systemd, leaves the drop-in and the new unit (both run the binary), and a
// rerun once the drop-in can be removed converges.
func TestAFailedRemovalLeavesARunnableUnitAndARerunConverges(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission this test relies on")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dropIn := writeDropIn(t, home, "upkeeper-self.service", "10-upkeeper-binary.conf",
		historicalSelfDropIn)
	directory := filepath.Dir(dropIn)
	if err := os.Chmod(directory, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o755) })

	var calls []string
	done, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls))
	if err == nil || !strings.Contains(err.Error(), dropIn) {
		t.Fatalf("the failed removal was not reported by name: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("systemd was reloaded although the drop-in is still there: %q", calls)
	}
	if len(done.Removed) != 0 {
		t.Errorf("reported removed = %q after a failed removal", done.Removed)
	}
	assertInstalled(t, home, SelfTimer())
	if _, err := os.Stat(dropIn); err != nil {
		t.Fatalf("the drop-in vanished: %v", err)
	}

	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	done, err = install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls))
	if err != nil {
		t.Fatalf("the rerun did not converge: %v", err)
	}
	if len(done.Removed) != 2 {
		t.Errorf("reported removed = %q on the rerun", done.Removed)
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Errorf("the drop-in directory survived the rerun: %v", err)
	}
}

// A base unit that is a symbolic link, for instance into a checkout, is
// replaced by the embedded unit; the link's target is never written.
func TestInstallReplacesASymlinkedUnitInsteadOfWritingThroughIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	destination := systemdUserDirectory(home)
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "checkout-unit.service")
	if err := os.WriteFile(target, []byte("[Service]\nExecStart=/usr/bin/python3 x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(destination, "upkeeper-self.service")); err != nil {
		t.Fatal(err)
	}

	var calls []string
	if _, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls)); err != nil {
		t.Fatalf("install: %v", err)
	}
	assertInstalled(t, home, SelfTimer())
	if info, err := os.Lstat(filepath.Join(destination, "upkeeper-self.service")); err != nil ||
		!info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
		t.Errorf("the installed unit is not a 0644 regular file: %v %v", info, err)
	}
	if content, _ := os.ReadFile(target); !strings.Contains(string(content), "python3") {
		t.Errorf("the installer wrote through the link into %s", target)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", entry.Name())
		}
	}
}

// An installer killed after writing the Go base unit and before removing the
// drop-in leaves both: the unit still runs the binary through either. A rerun
// recognises the same drop-in and finishes the job.
func TestARerunAfterAnInterruptedReconciliationConverges(t *testing.T) {
	for _, tc := range legacyCases() {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dropIn := writeDropIn(t, home, tc.service, "10-upkeeper-binary.conf", tc.dropIn)
			for _, definition := range tc.definitions {
				if err := os.WriteFile(filepath.Join(systemdUserDirectory(home), definition.Name),
					[]byte(definition.Content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var calls []string
			if _, err := install(tc.definitions, tc.timer, recordingRunner(&calls)); err != nil {
				t.Fatalf("the rerun did not converge: %v", err)
			}
			assertInstalled(t, home, tc.definitions)
			if _, err := os.Lstat(dropIn); !os.IsNotExist(err) {
				t.Errorf("the legacy drop-in survived the rerun: %v", err)
			}
			if len(calls) != 2 {
				t.Errorf("systemctl calls = %q, want a reload and an enable", calls)
			}
		})
	}
}
