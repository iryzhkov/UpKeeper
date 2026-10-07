package selfupdate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// fetchTimeout bounds one download, redirects included.
const fetchTimeout = 5 * time.Minute

// probeTimeout bounds the staged binary's version probe.
const probeTimeout = 10 * time.Second

// fetch downloads a pinned asset into a fresh staging directory and verifies
// it against the pin. The returned cleanup removes whatever is left of the
// staging directory and is safe to call on every path, including after the
// directory has been installed as a generation.
//
// The asset is asked for anonymously first, so a public release never needs a
// credential. When github.com answers as it does for a release the caller may
// not see, the asset is resolved through the API with a credential from this
// host, and the body it serves is verified exactly as an anonymous one is.
func (i *Installer) fetch(pin *release.ProgramPin, target string, asset release.ProgramAsset) (string, func(), error) {
	staging, err := i.stagingDirectory()
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(staging) }
	client := i.Client
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	url := pin.URL(i.BaseURL, target)
	response, err := client.Get(url)
	if err != nil {
		return "", cleanup, failure.Value("cannot download %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	if notPublic(response.StatusCode) && i.apiBase() != "" {
		authenticated, source, err := i.authenticatedAsset(client, pin, asset, response.StatusCode)
		if err != nil {
			return "", cleanup, err
		}
		defer func() { _ = authenticated.Body.Close() }()
		response, url = authenticated, source
	}
	if response.StatusCode != http.StatusOK {
		return "", cleanup, failure.Value("cannot download %s: HTTP %d", url, response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != asset.Size {
		return "", cleanup, failure.Value(
			"%s is %d bytes and the pin says %d; nothing installed",
			asset.Asset, response.ContentLength, asset.Size)
	}
	staged := filepath.Join(staging, binaryName)
	handle, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", cleanup, failure.Value("cannot stage the download: %v", err)
	}
	hash := sha256.New()
	// One byte beyond the pinned size is read, so that a longer body is seen
	// as longer rather than silently truncated to the expected length.
	written, err := io.Copy(io.MultiWriter(handle, hash), io.LimitReader(response.Body, asset.Size+1))
	if err == nil {
		err = handle.Sync()
	}
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", cleanup, failure.Value("cannot download %s: %v", url, err)
	}
	if written != asset.Size {
		return "", cleanup, failure.Value(
			"%s is %s bytes and the pin says %d; nothing installed",
			asset.Asset, sizeText(written, asset.Size), asset.Size)
	}
	if digest := hex.EncodeToString(hash.Sum(nil)); digest != asset.SHA256 {
		return "", cleanup, failure.Value(
			"%s has sha256 %s and the pin says %s; nothing installed",
			asset.Asset, digest, asset.SHA256)
	}
	// Only verified bytes ever become executable, and only for the owner.
	if err := os.Chmod(staged, 0o500); err != nil {
		return "", cleanup, failure.Value("cannot make the staged binary executable: %v", err)
	}
	return staged, cleanup, nil
}

func sizeText(written, expected int64) string {
	if written > expected {
		return "more than " + itoa(expected)
	}
	return itoa(written)
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// copyVerified copies a local file into staging and reports its size and
// digest as copied, so that what is checked is exactly what is installed.
func copyVerified(source, staged string) (int64, string, error) {
	input, err := os.Open(source)
	if err != nil {
		return 0, "", failure.Value("cannot read %s: %v", source, err)
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", failure.Value("cannot stage %s: %v", source, err)
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(output, hash),
		io.LimitReader(input, release.ProgramAssetLimit+1))
	if err == nil && size > release.ProgramAssetLimit {
		err = errors.New("larger than any upkeeper binary may be")
	}
	if err == nil {
		err = output.Sync()
	}
	if err == nil {
		err = output.Chmod(0o500)
	}
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, "", failure.Value("cannot stage %s: %v", source, err)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

// probe runs a staged binary's `version` with an empty PATH, a disposable HOME
// and nothing else inherited, and requires its standard output to be exactly
// the expected line. A binary that cannot even say what it is, or says it is
// something else, is never installed.
func probe(path, expected string) error {
	home, err := os.MkdirTemp("", "upkeeper-probe-")
	if err != nil {
		return failure.Value("cannot create a probe home: %v", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	var stdout, stderr bytes.Buffer
	var runErr error
	// A binary written moments ago can be briefly busy if another goroutine
	// forked while it was open for writing, so ETXTBSY alone is retried.
	for attempt := 0; attempt < 5; attempt++ {
		stdout.Reset()
		stderr.Reset()
		context, cancel := context.WithTimeout(context.Background(), probeTimeout)
		command := exec.CommandContext(context, path, "version")
		command.Dir = home
		command.Env = []string{"PATH=", "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
			"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
			"XDG_CACHE_HOME=" + filepath.Join(home, ".cache")}
		command.Stdin = nil
		command.Stdout = &stdout
		command.Stderr = &stderr
		runErr = command.Run()
		cancel()
		if !errors.Is(runErr, syscall.ETXTBSY) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	if runErr != nil {
		return failure.Value("the staged binary failed its version probe: %v; nothing installed", runErr)
	}
	if stdout.String() != expected+"\n" {
		return failure.Value(
			"the staged binary reports %q, not %q; nothing installed",
			trimmed(stdout.String()), expected)
	}
	return nil
}

func trimmed(value string) string {
	if len(value) > 200 {
		value = value[:200]
	}
	return string(bytes.TrimRight([]byte(value), "\n"))
}

func randomSuffix() string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return itoa(time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
