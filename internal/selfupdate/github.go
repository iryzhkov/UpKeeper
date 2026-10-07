package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// gitHubAPI is where a release that is not public is resolved.
const gitHubAPI = "https://api.github.com"

// ghTokenTimeout bounds `gh auth token`. It is a variable so that a test of a
// gh that hangs does not wait for the real bound.
var ghTokenTimeout = 10 * time.Second

// releaseDocumentLimit bounds the release document the API returns.
const releaseDocumentLimit = 4 << 20

// tokenPattern is what a GitHub credential may look like. Anything else, such
// as a value with a line break in it, is never put in a request header.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,255}$`)

// apiBase is the API an authenticated download asks, or "" when this
// installer must never authenticate.
func (i *Installer) apiBase() string {
	if i.APIBaseURL != "" {
		return strings.TrimRight(i.APIBaseURL, "/")
	}
	if i.BaseURL != "" {
		return ""
	}
	return gitHubAPI
}

// notPublic reports whether an anonymous answer for a release asset is how
// github.com answers for a release the caller may not see.
func notPublic(status int) bool {
	return status == http.StatusNotFound || status == http.StatusUnauthorized ||
		status == http.StatusForbidden
}

// authenticatedAsset resolves a release asset github.com would not serve
// anonymously through the REST API, with a credential found on this host: the
// release by its tag, then the asset by its id. It returns the asset response
// and the URL it came from, which names no credential.
//
// The credential travels only in the Authorization header, and only to the
// API host: never in a URL, an error or a file, and never to the storage host
// the asset redirects to.
func (i *Installer) authenticatedAsset(
	client *http.Client, pin *release.ProgramPin, asset release.ProgramAsset, status int,
) (*http.Response, string, error) {
	token, err := gitHubToken()
	if err != nil {
		return nil, "", err
	}
	if token == "" {
		return nil, "", failure.Value(
			"cannot download %s: the release is not public (HTTP %d) and no GitHub credential is "+
				"available on this host; run gh auth login on this host or set GH_TOKEN",
			asset.Asset, status)
	}
	base := i.apiBase()
	endpoint, err := url.Parse(base)
	if err != nil || endpoint.Host == "" {
		return nil, "", failure.Value("cannot download %s: the GitHub API address is invalid", asset.Asset)
	}
	authenticated := credentialStaysOn(client, endpoint)
	repository := base + "/repos/" + pin.Repository
	releaseURL := repository + "/releases/tags/" + url.PathEscape(pin.Tag)
	response, err := gitHubRequest(authenticated, releaseURL, "application/vnd.github+json", token)
	if err != nil {
		return nil, "", failure.Value("cannot download %s: cannot reach the GitHub API: %v",
			asset.Asset, withoutQuery(err))
	}
	document, problem, hint := readRelease(response)
	if problem != "" {
		return nil, "", failure.Value("cannot download %s: %s for release %s of %s%s",
			asset.Asset, problem, pin.Tag, pin.Repository, hint)
	}
	var id int64
	for _, candidate := range document.Assets {
		if candidate.Name == asset.Asset && candidate.ID > 0 {
			id = candidate.ID
			break
		}
	}
	if id == 0 {
		return nil, "", failure.Value("cannot download %s: release %s of %s has no asset named %s",
			asset.Asset, pin.Tag, pin.Repository, asset.Asset)
	}
	assetURL := repository + "/releases/assets/" + strconv.FormatInt(id, 10)
	response, err = gitHubRequest(authenticated, assetURL, "application/octet-stream", token)
	if err != nil {
		return nil, "", failure.Value("cannot download %s: %v", assetURL, withoutQuery(err))
	}
	return response, assetURL, nil
}

// releaseDocument is the part of the API's release document that is used.
type releaseDocument struct {
	Assets []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"assets"`
}

// readRelease reads and closes the answer to a release lookup. problem is ""
// when the document was read; hint, when set, says what to do about it.
func readRelease(response *http.Response) (document releaseDocument, problem, hint string) {
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusUnauthorized:
			hint = "; the GitHub credential on this host was rejected"
		case http.StatusForbidden:
			hint = "; the GitHub credential on this host cannot see it, or its API rate limit is spent"
		case http.StatusNotFound:
			hint = "; the GitHub credential on this host cannot see it"
		}
		return document, "the GitHub API answered HTTP " + strconv.Itoa(response.StatusCode), hint
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, releaseDocumentLimit+1))
	if err != nil {
		return document, "the GitHub API release document could not be read", ""
	}
	if len(body) > releaseDocumentLimit {
		return document, "the GitHub API release document is larger than " +
			strconv.Itoa(releaseDocumentLimit) + " bytes", ""
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return document, "the GitHub API returned an unreadable release document", ""
	}
	return document, "", ""
}

// withoutQuery drops the query from the URL a request error names. A release
// asset redirects to a signed storage URL whose query grants access to the
// asset for a while, so it is never repeated in an error or a report.
func withoutQuery(err error) error {
	var failed *url.Error
	if !errors.As(err, &failed) {
		return err
	}
	cleaned := *failed
	if parsed, parseErr := url.Parse(failed.URL); parseErr == nil {
		parsed.RawQuery, parsed.Fragment, parsed.User = "", "", nil
		cleaned.URL = parsed.String()
	} else {
		cleaned.URL = "(unparsable URL)"
	}
	return &cleaned
}

// gitHubRequest sends one authenticated GET to the API.
func gitHubRequest(client *http.Client, address, accept, token string) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("Authorization", "Bearer "+token)
	return client.Do(request)
}

// credentialStaysOn returns a copy of client that removes the Authorization
// header from every redirect that leaves the API's scheme and host, before
// the caller's own redirect policy, if any, sees the request. Go's client
// strips it on a cross-host redirect too; this does not depend on that.
func credentialStaysOn(client *http.Client, api *url.URL) *http.Client {
	copied := *client
	policy := client.CheckRedirect
	copied.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != api.Scheme || request.URL.Host != api.Host {
			request.Header.Del("Authorization")
		}
		if policy != nil {
			return policy(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &copied
}

// gitHubToken finds a GitHub credential on this host, in order: GH_TOKEN,
// GITHUB_TOKEN, then what `gh auth token` prints. "" means there is none. A
// variable that is set to something that is not a token is a refusal rather
// than a reason to try the next source, so that a mistake is seen; the error
// names the variable, never its value.
func gitHubToken() (string, error) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			continue
		}
		if !tokenPattern.MatchString(value) {
			return "", failure.Value("%s is set but is not a GitHub token; nothing downloaded", name)
		}
		return value, nil
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", nil
	}
	bounded, cancel := context.WithTimeout(context.Background(), ghTokenTimeout)
	defer cancel()
	command := exec.CommandContext(bounded, gh, "auth", "token", "--hostname", "github.com")
	command.Stdin = nil
	// gh's diagnostics are discarded rather than kept: nothing it says is
	// reported, so nothing it says can carry a credential into a report.
	command.Stderr = io.Discard
	// gh runs in its own process group, and the whole group is killed at the
	// bound, so nothing it started outlives the lookup.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	output, err := command.Output()
	if err != nil {
		return "", nil
	}
	token := strings.TrimSpace(string(output))
	if !tokenPattern.MatchString(token) {
		return "", nil
	}
	return token, nil
}
