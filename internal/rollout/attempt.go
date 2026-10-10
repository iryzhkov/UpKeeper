package rollout

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Each invocation has an immutable, synced intent independent of the replaced
// JSON checkpoint. Completion is appended only after every effect, observation,
// receipt write/fsync/rename/dirsync returned success. The final unbuffered
// acknowledgement is deliberately not followed by a fallible durability gate:
// losing it on power failure fails closed, while preceding receipts are durable.
type attemptIntent struct {
	Action, Host, ConfigDigest string
	Started                    time.Time
}

func attemptPath(c Config, action string) string {
	return filepath.Join(c.StatePath+".attempts", strings.ReplaceAll(action, ":", "-"))
}
func readAttempt(c Config, path string, now time.Time) (attemptIntent, bool, error) {
	var intent attemptIntent
	st, err := os.Lstat(path)
	if err != nil {
		return intent, false, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return intent, false, errors.New("unsafe attempt file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return intent, false, err
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) < 2 || len(lines) > 3 || lines[len(lines)-1] != "" {
		return intent, false, errors.New("corrupt attempt record")
	}
	dec := json.NewDecoder(strings.NewReader(lines[0]))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&intent); err != nil {
		return intent, false, err
	}
	if dec.Decode(new(any)) != io.EOF || intent.ConfigDigest != c.Digest() ||
		intent.Started.IsZero() || intent.Started.After(now) ||
		attemptPath(c, intent.Action) != path ||
		!(validPhaseIntent(intent.Action, intent.Host) || intent.Action == "verify" && intent.Host == "" ||
			intent.Action == "run:"+intent.Host && contains(rc122Order, intent.Host) ||
			intent.Action == "enroll:"+intent.Host && contains(c.ActiveWorkers, intent.Host)) {
		return intent, false, errors.New("mismatched or future attempt intent")
	}
	if len(lines) == 2 {
		return intent, false, nil
	}
	if lines[1] != "complete:"+digest([]byte(lines[0]+"\n")) {
		return intent, false, errors.New("corrupt completion acknowledgement")
	}
	return intent, true, nil
}
func attemptPending(c Config, now time.Time) error {
	dir := c.StatePath + ".attempts"
	st, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe attempt inventory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		_, complete, err := readAttempt(c, filepath.Join(dir, entry.Name()), now)
		if err != nil {
			return err
		}
		if !complete {
			return fmt.Errorf("unfinished invocation %s; rollback recovery only", entry.Name())
		}
	}
	return nil
}
func beginAttempt(c Config, action, host string, now time.Time) (func() error, error) {
	dir := c.StatePath + ".attempts"
	if err := durableDirectory(dir); err != nil {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("attempt inventory requires private directory")
	}
	raw, err := json.Marshal(attemptIntent{action, host, c.Digest(), now})
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	f, err := os.OpenFile(attemptPath(c, action), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if n, e := f.Write(raw); e != nil || n != len(raw) {
		f.Close()
		return nil, errors.Join(e, io.ErrShortWrite)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	d, err := os.Open(dir)
	if err != nil {
		f.Close()
		return nil, err
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil || closeErr != nil {
		f.Close()
		return nil, errors.Join(err, closeErr)
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	// The last protocol operation is one unbuffered native write. Descriptor
	// release does not flush a userspace buffer or certify a failed receipt.
	return func() error {
		f, err := os.OpenFile(attemptPath(c, action), os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		ack := []byte("complete:" + digest(raw) + "\n")
		n, e := f.Write(ack)
		if e != nil {
			return e
		}
		if n != len(ack) {
			return io.ErrShortWrite
		}
		return nil
	}, nil
}
