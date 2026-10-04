package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const t3QualificationSupervisorArg = "--upkeeper-internal-t3-qualification-supervisor"

// Re-exec keeps subreaper state and wait ownership out of the long-lived agent.
// This mode runs before main (also in the test binary) and starts only its own
// qualification tree. prctl(PR_SET_CHILD_SUBREAPER) requires no privileges and
// has the same interface on Linux amd64 and arm64.
func init() {
	if len(os.Args) == 4 && os.Args[1] == t3QualificationSupervisorArg {
		os.Exit(t3RunQualificationSupervisor(os.Args[2], os.Args[3]))
	}
}

func t3RunQualificationSupervisor(node, executable string) int {
	message, err := t3SuperviseQualification(node, executable)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cleanup qualification failed")
		return 1
	}
	if message == "isolated startup and PTY qualified" {
		fmt.Println(message + "; cleanup complete")
		return 0
	}
	for _, stage := range []string{"isolated startup", "isolated PTY ticket", "isolated PTY websocket", "isolated PTY open", "isolated PTY output"} {
		if message == stage+" qualification failed" {
			fmt.Fprintln(os.Stderr, message)
			return 1
		}
	}
	fmt.Fprintln(os.Stderr, "cleanup qualification failed")
	return 1
}

func t3SuperviseQualification(node, executable string) (message string, err error) {
	const prSetChildSubreaper = 36
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0, 0, 0, 0); errno != 0 {
		return "", errno
	}
	root, err := os.MkdirTemp("", "upkeeper-t3-qualification-")
	if err != nil {
		return "", err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(root); cleanupErr != nil {
			err = cleanupErr
		}
	}()
	output, err := os.CreateTemp(root, "helper-output-")
	if err != nil {
		return "", err
	}
	defer output.Close()
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stopped)
	child := exec.Command(node, "-e", t3ExecutableQualification, executable, root)
	child.Stdout, child.Stderr = output, output
	if err := child.Start(); err != nil {
		return "", err
	}
	// Deliberately no Cmd.Wait goroutine: this supervisor is the only reaper.
	defer child.Process.Release()
	deadline := time.Now().Add(25 * time.Second)
	nodeDone, nodeOK := false, false
	for !nodeDone && time.Now().Before(deadline) {
		select {
		case <-stopped:
			deadline = time.Now()
		default:
		}
		for time.Now().Before(deadline) {
			var status syscall.WaitStatus
			pid, waitErr := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if waitErr == syscall.EINTR {
				continue
			}
			if waitErr != nil || pid == 0 {
				break
			}
			if pid == child.Process.Pid {
				nodeDone, nodeOK = true, status.Exited() && status.ExitStatus() == 0
				break // Its unreaped birth identity held until this exact wait.
			}
		}
		if !nodeDone {
			time.Sleep(10 * time.Millisecond)
		}
	}
	// The Node helper exits on success, failure or its 20s deadline. All orphaned
	// descendants are adopted here, even after double-fork or setsid. Kill parents,
	// then repeat for newly adopted children; ECHILD proves complete reap.
	if err := t3ReapQualificationTree(5 * time.Second); err != nil {
		return "", err
	}
	if !nodeDone {
		return "", errors.New("qualification supervisor deadline")
	}
	if _, err := output.Seek(0, 0); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(output, 4096))
	if err != nil {
		return "", err
	}
	message = strings.TrimSpace(string(data))
	if !nodeOK && message == "isolated startup and PTY qualified" {
		return "", errors.New("qualification helper failed")
	}
	return message, nil
}

func t3ReapQualificationTree(bound time.Duration) error {
	started := time.Now()
	for time.Since(started) < bound {
		// Reap all exited children. Only this dedicated process can reap them.
		for time.Since(started) < bound {
			pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
			if err == syscall.ECHILD {
				return nil
			}
			if err == syscall.EINTR {
				continue
			}
			if err != nil {
				return err
			}
			if pid == 0 {
				break
			}
		}
		tasks, err := filepath.Glob("/proc/self/task/*/children")
		if err != nil {
			return err
		}
		if len(tasks) == 0 {
			return errors.New("no child ownership evidence")
		}
		for _, task := range tasks {
			data, err := os.ReadFile(task)
			if os.IsNotExist(err) {
				continue
			} // A runtime thread may have just exited.
			if err != nil {
				return err
			}
			for _, field := range strings.Fields(string(data)) {
				if time.Since(started) >= bound {
					return errors.New("qualification child reap deadline")
				}
				pid, err := strconv.Atoi(field)
				if err != nil {
					return err
				}
				// Validate direct-child ownership before opening a process handle. If alive,
				// this child cannot be reaped or have its PID reused until OUR next wait.
				// FindProcess uses a birth-pinned pidfd on supported Linux kernels; the
				// unreaped-child invariant also protects its fallback on older kernels.
				got, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
				if err == syscall.ECHILD {
					continue
				}
				if err == syscall.EINTR {
					continue
				}
				if err != nil {
					return err
				}
				if got != 0 {
					continue
				}
				process, err := os.FindProcess(pid)
				if err != nil {
					return err
				}
				sig := syscall.SIGTERM
				if time.Since(started) >= 300*time.Millisecond {
					sig = syscall.SIGKILL
				}
				err = process.Signal(sig)
				process.Release()
				if err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
					return err
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("qualification child reap deadline")
}
