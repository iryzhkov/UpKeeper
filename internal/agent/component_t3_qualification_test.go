package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func t3QualificationFixture(t *testing.T, modern bool) (*Context, string) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("linux-x64 fixtures")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node needed for package layout regression")
	}
	root := t.TempDir()
	entry := "dist/bin.mjs"
	manifest := `{"name":"t3","version":"0.0.38","type":"module","bin":{"t3":"./dist/bin.mjs"},"dependencies":{"node-pty":"^1.1.0"}}`
	if modern {
		entry = "bin/t3.js"
		manifest = `{"name":"t3","version":"0.0.45","bin":{"t3":"./bin/t3.js"},"optionalDependencies":{"@t3code/t3-linux-x64":"0.0.45"}}`
	}
	adapterTestWrite(t, filepath.Join(root, "package.json"), manifest)
	adapterTestWrite(t, filepath.Join(root, entry), "#!/bin/sh\necho 't3 v"+map[bool]string{true: "0.0.45", false: "0.0.38"}[modern]+"'\n")
	if err := os.Chmod(filepath.Join(root, entry), 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "path")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(node, filepath.Join(bin, "node")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, entry), filepath.Join(bin, "t3")); err != nil {
		t.Fatal(err)
	}
	return &Context{Env: []string{"PATH=" + bin + ":/usr/bin:/bin", "TMPDIR=" + os.TempDir()}, Runner: procx.Run}, root
}

func TestT3PackageQualificationRefusals(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("linux-x64 fixtures")
	}
	cases := []struct {
		name, problem string
		modern        bool
		mutate        func(*testing.T, string)
	}{
		{"missing platform executable", "package layout", true, func(t *testing.T, r string) {
			adapterTestWrite(t, filepath.Join(r, "node_modules/@t3code/t3-linux-x64/package.json"), `{"name":"@t3code/t3-linux-x64","version":"0.0.45","os":["linux"],"cpu":["x64"]}`)
		}},
		{"wrong platform metadata", "package layout", true, func(t *testing.T, r string) {
			pkg := filepath.Join(r, "node_modules/@t3code/t3-linux-x64")
			adapterTestWrite(t, filepath.Join(pkg, "package.json"), `{"name":"@t3code/t3-linux-x64","version":"0.0.45","os":["linux"],"cpu":["arm64"]}`)
			adapterTestWrite(t, filepath.Join(pkg, "t3"), "#!/bin/sh\necho 't3 v0.0.45'\n")
			if err := os.Chmod(filepath.Join(pkg, "t3"), 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{"non executable platform binary", "package layout", true, func(t *testing.T, r string) {
			pkg := filepath.Join(r, "node_modules/@t3code/t3-linux-x64")
			adapterTestWrite(t, filepath.Join(pkg, "package.json"), `{"name":"@t3code/t3-linux-x64","version":"0.0.45","os":["linux"],"cpu":["x64"]}`)
			adapterTestWrite(t, filepath.Join(pkg, "t3"), "#!/bin/sh\n")
		}},
		{"incorrect executable version", "platform executable version", true, func(t *testing.T, r string) {
			pkg := filepath.Join(r, "node_modules/@t3code/t3-linux-x64")
			adapterTestWrite(t, filepath.Join(pkg, "package.json"), `{"name":"@t3code/t3-linux-x64","version":"0.0.45","os":["linux"],"cpu":["x64"]}`)
			adapterTestWrite(t, filepath.Join(pkg, "t3"), "#!/bin/sh\necho 't3 v0.0.44'\n")
			if err := os.Chmod(filepath.Join(pkg, "t3"), 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown layout", "package layout", true, func(t *testing.T, r string) {
			adapterTestWrite(t, filepath.Join(r, "package.json"), `{"name":"t3","version":"0.0.45","bin":{"t3":"./bin/t3.js"}}`)
		}},
		{"malformed manifest", "package layout", true, func(t *testing.T, r string) { adapterTestWrite(t, filepath.Join(r, "package.json"), "{") }},
		{"legacy ABI failure", "legacy native", false, func(t *testing.T, r string) {
			adapterTestWrite(t, filepath.Join(r, "node_modules/node-pty/index.js"), "throw new Error('NODE_MODULE_VERSION mismatch');")
		}},
		{"incorrect CLI version", "version", false, func(t *testing.T, r string) {
			adapterTestWrite(t, filepath.Join(r, "node_modules/node-pty/index.js"), "module.exports={};")
			adapterTestWrite(t, filepath.Join(r, "dist/bin.mjs"), "#!/bin/sh\necho 't3 v0.0.37'\n")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, r := t3QualificationFixture(t, tc.modern)
			tc.mutate(t, r)
			version := "0.0.38"
			if tc.modern {
				version = "0.0.45"
			}
			err := t3VerifyInstalledCLI(c, version)
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("want %q refusal, got %v", tc.problem, err)
			}
		})
	}
}

func TestT3LegacyQualification(t *testing.T) {
	c, r := t3QualificationFixture(t, false)
	adapterTestWrite(t, filepath.Join(r, "node_modules/node-pty/index.js"), "module.exports={};")
	if err := t3VerifyInstalledCLI(c, "0.0.38"); err != nil {
		t.Fatal(err)
	}
}

func TestT3ModernStartupFailureCleansChildren(t *testing.T) { t3TestStartupCleanup(t, false) }

func TestT3ModernStartupTimeoutCleansChildren(t *testing.T) { t3TestStartupCleanup(t, true) }

func t3TestStartupCleanup(t *testing.T, timeout bool) {
	t.Helper()
	c, r := t3QualificationFixture(t, true)
	pkg := filepath.Join(r, "node_modules/@t3code/t3-linux-x64")
	adapterTestWrite(t, filepath.Join(pkg, "package.json"), `{"name":"@t3code/t3-linux-x64","version":"0.0.45","os":["linux"],"cpu":["x64"]}`)
	record := filepath.Join(r, "child")
	exe := filepath.Join(pkg, "t3")
	// Simulate a runtime that spawns a child then fails or hangs before opening its socket.
	delay := "0.2"
	if timeout {
		delay = "90"
	}
	adapterTestWrite(t, exe, fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 't3 v0.0.45'; exit; fi\n/bin/sleep 90 &\necho $! > %q\necho \"$HOME\" >> %q\nsleep %s\nexit 1\n", record, record, delay))
	if err := os.Chmod(exe, 0755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := t3VerifyInstalledCLI(c, "0.0.45")
	if err == nil || !strings.Contains(err.Error(), "isolated startup") {
		t.Fatalf("want startup refusal: %v", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("qualification exceeded its bound")
	}
	data, readErr := os.ReadFile(record)
	if readErr != nil {
		t.Fatal(readErr)
	}
	lines := strings.Fields(string(data))
	pid, _ := strconv.Atoi(lines[0])
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
		t.Fatalf("child %d was not fully reaped: %v", pid, err)
	}
	if _, err := os.Stat(lines[1]); !os.IsNotExist(err) {
		t.Fatalf("private qualification HOME remains: %v", err)
	}
}

// The outer subreaper is a cleanup harness: even a broken implementation cannot
// leave the recorded detached sleeper running or unreaped. It also owns an
// unrelated sentinel, whose survival demonstrates the supervisor's boundary.
func TestT3ImmediateExitDetachedChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux subreaper")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	record, executable := filepath.Join(root, "child"), filepath.Join(root, "t3")
	adapterTestWrite(t, executable, fmt.Sprintf("#!/bin/sh\n/usr/bin/setsid /bin/sleep 60 &\necho $! > %q\necho \"$HOME\" >> %q\nexit 1\n", record, record))
	if err := os.Chmod(executable, 0700); err != nil {
		t.Fatal(err)
	}
	const harness = `import ctypes, os, pathlib, signal, subprocess, sys, time
assert ctypes.CDLL(None).prctl(36, 1, 0, 0, 0) == 0
sentinel = subprocess.Popen(["/usr/bin/setsid", "/bin/sleep", "60"])
try:
    for trial in range(10):
        result = subprocess.run(sys.argv[1:5], capture_output=True, text=True, timeout=32)
        assert result.returncode == 1, result
        assert result.stderr.strip() == "isolated startup qualification failed", result
        pid, home = pathlib.Path(sys.argv[5]).read_text().split()
        assert not pathlib.Path("/proc/" + pid).exists(), "detached child not reaped: " + pid
        assert not pathlib.Path(home).exists(), "private HOME remains"
        assert sentinel.poll() is None, "unrelated process was killed"
    print("10 immediate-exit detached children fully reaped; unrelated sentinel survived; private HOME removed")
finally:
    # No other thread reaps these direct adopted children, so waitid(WNOWAIT)
    # pins ownership before signalling. Repeat for descendants adopted on exit.
    until = time.monotonic() + 5
    while True:
        try:
            os.waitid(os.P_ALL, 0, os.WEXITED | os.WNOHANG | os.WNOWAIT)
        except ChildProcessError:
            break
        assert time.monotonic() < until, "harness cleanup deadline"
        for field in pathlib.Path("/proc/self/task/%d/children" % os.getpid()).read_text().split():
            pid = int(field)
            try:
                os.waitid(os.P_PID, pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
                fd = os.pidfd_open(pid)
                try:
                    signal.pidfd_send_signal(fd, signal.SIGKILL)
                finally:
                    os.close(fd)
                os.waitpid(pid, os.WNOHANG)
            except (ChildProcessError, ProcessLookupError):
                pass
        time.sleep(0.01)
`
	result := procx.Run(procx.Command{Argv: []string{"/usr/bin/python3", "-I", "-c", harness, self, t3QualificationSupervisorArg, node, executable, record}, Timeout: 45 * time.Second, Env: os.Environ()})
	if !result.OK() {
		t.Fatalf("detached child harness: %+v", result)
	}
	t.Log(strings.TrimSpace(result.Stdout))
}

// Opt-in integration proof uses only a new private server, never the user unit.
func TestT3ActualExecutableQualification(t *testing.T) {
	if os.Getenv("UPKEEPER_TEST_T3_ACTUAL") != "1" {
		t.Skip("set UPKEEPER_TEST_T3_ACTUAL=1 to qualify installed .45")
	}
	c := &Context{Env: os.Environ(), Runner: procx.Run}
	if err := t3VerifyInstalledCLI(c, "0.0.45"); err != nil {
		t.Fatal(err)
	}
	t.Log("actual .45 package: version, isolated startup and PTY round trip qualified; cleanup complete")
}
