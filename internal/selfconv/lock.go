package selfconv

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
)

// LockCheckout takes the exclusive lock that serializes UpKeeper's own use of
// the release checkout's HEAD. Refresh holds it while it moves HEAD, and a
// binary re-executed after a self-update holds it from its check of HEAD,
// through its program check, to the end of its convergence, so that the
// commit whose program it verified is the commit it converges.
//
// The lock is an flock on the checkout's root directory: it needs no file of
// its own and is released when the process exits. A person running git in
// the checkout does not take it, which is why convergence also compares HEAD
// with the commit it was given before loading anything.
//
// wait bounds how long another holder is waited for; 0 refuses at once.
func LockCheckout(root string, wait time.Duration) (func(), error) {
	handle, err := os.Open(root)
	if err != nil {
		return nil, failure.Value("cannot open the checkout %s to lock it: %v", root, err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = handle.Close()
			return nil, failure.Value("cannot lock the checkout %s: %v", root, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = handle.Close()
			return nil, failure.LockContention(
				"another upkeeper self-pull is using the checkout %s; it is unchanged", root)
		}
		time.Sleep(min(remaining, 100*time.Millisecond))
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(handle.Fd()), syscall.LOCK_UN)
			_ = handle.Close()
		})
	}, nil
}
