package selfupdate

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/release"
)

// The token strings every test in this file hands out. Each is distinct, so
// that a test can tell which source the request used.
const (
	environmentToken = "ghp_fromGHTOKEN0123456789"
	githubToken      = "ghp_fromGITHUBTOKEN0123456789"
	ghCommandToken   = "gho_fromGhAuthToken0123456789"
)

var everyTestToken = []string{environmentToken, githubToken, ghCommandToken}

// githubAPI stands in for api.github.com: it answers the release-by-tag and
// asset requests for one token, and redirects an asset download to a storage
// server on another host, as GitHub does.
type githubAPI struct {
	mutex sync.Mutex
	// token is the only credential the API accepts.
	token string
	// release is the release document served for the tag; "" means the
	// default one that lists the program's asset.
	release string
	// releaseStatus, when set, answers the tag lookup with that status.
	releaseStatus int
	// body is what storage serves for the asset.
	body []byte
	// direct serves the asset from the API itself instead of redirecting.
	direct bool

	requests []string
	// storageAuthorization records the Authorization header each storage
	// request carried.
	storageAuthorization []string
	apiAuthorization     []string
	accept               []string
}

const testAssetID = 4242

func serveGitHub(t *testing.T, body []byte, token string) (*githubAPI, *httptest.Server) {
	t.Helper()
	api := &githubAPI{token: token, body: body}
	storage := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		api.mutex.Lock()
		defer api.mutex.Unlock()
		api.storageAuthorization = append(api.storageAuthorization, request.Header.Get("Authorization"))
		_, _ = writer.Write(api.body)
	}))
	t.Cleanup(storage.Close)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		api.mutex.Lock()
		defer api.mutex.Unlock()
		api.requests = append(api.requests, request.URL.Path)
		api.apiAuthorization = append(api.apiAuthorization, request.Header.Get("Authorization"))
		api.accept = append(api.accept, request.Header.Get("Accept"))
		if request.Header.Get("Authorization") != "Bearer "+api.token {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		repository := "/repos/" + release.ProgramRepository
		switch request.URL.Path {
		case repository + "/releases/tags/v0.1.31":
			if api.releaseStatus != 0 {
				writer.WriteHeader(api.releaseStatus)
				return
			}
			document := api.release
			if document == "" {
				document = `{"tag_name":"v0.1.31","assets":[` +
					`{"id":1,"name":"upkeeper-linux-arm64"},` +
					`{"id":4242,"name":"upkeeper-linux-amd64"}]}`
			}
			_, _ = writer.Write([]byte(document))
		case repository + "/releases/assets/4242":
			if request.Header.Get("Accept") != "application/octet-stream" {
				_, _ = writer.Write([]byte(`{"id":4242}`))
				return
			}
			if api.direct {
				_, _ = writer.Write(api.body)
				return
			}
			http.Redirect(writer, request, storage.URL+"/blob/upkeeper-linux-amd64", http.StatusFound)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return api, server
}

// isolateCredentials gives the test an environment with no GitHub credential
// and no gh on PATH, whatever the machine running it has.
func isolateCredentials(t *testing.T) string {
	t.Helper()
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	return bin
}

// fakeGh puts a gh on PATH whose `auth token` runs script.
func fakeGh(t *testing.T, bin, script string) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"if [ \"$1 $2\" != 'auth token' ]; then echo \"unexpected gh $*\" >&2; exit 2; fi\n" + script
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func privateInstaller(t *testing.T, releases, api *httptest.Server) *Installer {
	t.Helper()
	return &Installer{
		Layout: Layout{Home: t.TempDir()}, Client: &http.Client{Timeout: 30 * time.Second},
		BaseURL: releases.URL, APIBaseURL: api.URL, Target: testTarget,
	}
}

// assertNoToken fails when any token string appears in text or anywhere
// under the installation's home.
func assertNoToken(t *testing.T, text string, home string) {
	t.Helper()
	for _, token := range everyTestToken {
		if strings.Contains(text, token) {
			t.Errorf("a token appears in %q", text)
		}
	}
	_ = filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, token := range everyTestToken {
			if strings.Contains(string(content), token) {
				t.Errorf("a token was written to %s", path)
			}
		}
		return nil
	})
}

// A public release downloads anonymously, exactly as before: no credential is
// looked up and the API is never asked.
func TestAPublicReleaseDownloadsWithoutACredential(t *testing.T) {
	bin := isolateCredentials(t)
	fakeGh(t, bin, "echo called >> \"$0.calls\"\necho "+ghCommandToken+"\n")
	t.Setenv("GH_TOKEN", environmentToken)
	store, releases := serve(t)
	update := newProgram("v0.1.31", "b")
	store.publish(update)
	api, apiServer := serveGitHub(t, update.body, environmentToken)
	target := privateInstaller(t, releases, apiServer)
	result, err := target.Apply(update.pin())
	if err != nil || result.Generation != update.sha() {
		t.Fatalf("Apply: %+v, %v", result, err)
	}
	if len(api.requests) != 0 {
		t.Errorf("a public release asked the API: %v", api.requests)
	}
	if _, err := os.Stat(filepath.Join(bin, "gh.calls")); err == nil {
		t.Errorf("a public release ran gh auth token")
	}
}

// A private release, which github.com answers with 404, 401 or 403 when
// asked anonymously, is resolved through the API with a credential from the
// first source that has one: GH_TOKEN, GITHUB_TOKEN, then gh auth token.
func TestAPrivateReleaseDownloadsWithEachCredentialSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		setup  func(t *testing.T, bin string)
		token  string
	}{
		{"GH_TOKEN", http.StatusNotFound, func(t *testing.T, bin string) {
			t.Setenv("GH_TOKEN", environmentToken)
			t.Setenv("GITHUB_TOKEN", githubToken)
			fakeGh(t, bin, "echo "+ghCommandToken+"\n")
		}, environmentToken},
		{"GITHUB_TOKEN", http.StatusNotFound, func(t *testing.T, bin string) {
			t.Setenv("GITHUB_TOKEN", githubToken)
			fakeGh(t, bin, "echo "+ghCommandToken+"\n")
		}, githubToken},
		{"gh auth token", http.StatusNotFound, func(t *testing.T, bin string) {
			fakeGh(t, bin, "echo 'some warning on stderr' >&2\necho "+ghCommandToken+"\n")
		}, ghCommandToken},
		{"an anonymous 401", http.StatusUnauthorized, func(t *testing.T, bin string) {
			t.Setenv("GH_TOKEN", environmentToken)
		}, environmentToken},
		{"an anonymous 403", http.StatusForbidden, func(t *testing.T, bin string) {
			t.Setenv("GH_TOKEN", environmentToken)
		}, environmentToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := isolateCredentials(t)
			test.setup(t, bin)
			store, releases := serve(t)
			update := newProgram("v0.1.31", "b")
			store.status[assetPath(update.tag)] = test.status
			api, apiServer := serveGitHub(t, update.body, test.token)
			target := privateInstaller(t, releases, apiServer)
			result, err := target.Apply(update.pin())
			if err != nil {
				t.Fatalf("Apply: %v (API requests %v)", err, api.requests)
			}
			if result.Generation != update.sha() || runBinary(t, target.Layout) != versionOf(update) {
				t.Fatalf("result %+v", result)
			}
			wantRequests := []string{
				"/repos/" + release.ProgramRepository + "/releases/tags/v0.1.31",
				"/repos/" + release.ProgramRepository + "/releases/assets/4242",
			}
			if strings.Join(api.requests, " ") != strings.Join(wantRequests, " ") {
				t.Errorf("API requests %v, want %v", api.requests, wantRequests)
			}
			if api.accept[1] != "application/octet-stream" {
				t.Errorf("asset request accepted %q", api.accept[1])
			}
			// The credential goes to the API host and nowhere else: the
			// storage host the asset redirects to never sees it.
			if len(api.storageAuthorization) != 1 || api.storageAuthorization[0] != "" {
				t.Errorf("storage saw Authorization %q", api.storageAuthorization)
			}
			assertNoToken(t, "", target.Layout.Home)
		})
	}
}

// The authenticated body is held to the pin exactly as an anonymous one is.
func TestAnAuthenticatedDownloadIsVerifiedAgainstThePin(t *testing.T) {
	bin := isolateCredentials(t)
	_ = bin
	t.Setenv("GH_TOKEN", environmentToken)
	_, releases := serve(t)
	update := newProgram("v0.1.31", "b")
	impostor := newProgram("v0.1.31", "c")
	_, apiServer := serveGitHub(t, impostor.body, environmentToken)
	target := privateInstaller(t, releases, apiServer)
	_, err := target.Apply(update.pin())
	if err == nil || !strings.Contains(err.Error(), "has sha256") ||
		!strings.Contains(err.Error(), "nothing installed") {
		t.Fatalf("want a digest refusal, got %v", err)
	}
	if target.Layout.CurrentGeneration() != "" {
		t.Errorf("a refused download became current %q", target.Layout.CurrentGeneration())
	}
	assertNoToken(t, err.Error(), target.Layout.Home)
}

// Go strips Authorization on a redirect to another host; the installer does
// not rely on that, and this proves the storage host never sees the token,
// including when it is served from the API host directly.
func TestTheCredentialNeverFollowsARedirectToAnotherHost(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("GH_TOKEN", environmentToken)
	_, releases := serve(t)
	update := newProgram("v0.1.31", "b")
	api, apiServer := serveGitHub(t, update.body, environmentToken)
	target := privateInstaller(t, releases, apiServer)
	// A caller's own redirect policy is kept, and the credential is still
	// removed before it runs.
	policyCalls := 0
	target.Client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		policyCalls++
		if request.Header.Get("Authorization") != "" {
			t.Errorf("redirect to %s carries Authorization", request.URL.Host)
		}
		return nil
	}
	if _, err := target.Apply(update.pin()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if policyCalls != 1 {
		t.Errorf("the caller's redirect policy ran %d times", policyCalls)
	}
	if len(api.storageAuthorization) != 1 || api.storageAuthorization[0] != "" {
		t.Errorf("storage saw Authorization %q", api.storageAuthorization)
	}
	if api.apiAuthorization[0] != "Bearer "+environmentToken {
		t.Errorf("the API saw Authorization %q", api.apiAuthorization[0])
	}
}

// Every refusal names what went wrong and never the credential, and nothing it
// leaves on disk holds the credential either.
func TestPrivateDownloadRefusals(t *testing.T) {
	noCredential := "cannot download upkeeper-linux-amd64: the release is not public (HTTP 404) and " +
		"no GitHub credential is available on this host; run gh auth login on this host or set GH_TOKEN"
	for _, test := range []struct {
		name     string
		setup    func(t *testing.T, bin string, api *githubAPI)
		expected string
		// apiRequests is how many requests the API may see.
		apiRequests int
	}{
		{"no credential at all", func(t *testing.T, bin string, api *githubAPI) {}, noCredential, 0},
		{"gh is installed but not logged in", func(t *testing.T, bin string, api *githubAPI) {
			fakeGh(t, bin, "echo 'You are not logged into any GitHub hosts. "+environmentToken+"' >&2\nexit 1\n")
		}, noCredential, 0},
		{"gh prints nothing", func(t *testing.T, bin string, api *githubAPI) {
			fakeGh(t, bin, "exit 0\n")
		}, noCredential, 0},
		{"gh hangs", func(t *testing.T, bin string, api *githubAPI) {
			fakeGh(t, bin, "exec /bin/sleep 30\n")
			saved := ghTokenTimeout
			ghTokenTimeout = 200 * time.Millisecond
			t.Cleanup(func() { ghTokenTimeout = saved })
		}, noCredential, 0},
		{"a GH_TOKEN that is not a token", func(t *testing.T, bin string, api *githubAPI) {
			t.Setenv("GH_TOKEN", environmentToken+"\r\nX-Injected: "+githubToken)
		}, "GH_TOKEN is set but is not a GitHub token", 0},
		{"a rejected credential", func(t *testing.T, bin string, api *githubAPI) {
			t.Setenv("GH_TOKEN", githubToken)
		}, "the GitHub API answered HTTP 401 for release v0.1.31", 1},
		{"a credential that cannot see the release", func(t *testing.T, bin string, api *githubAPI) {
			t.Setenv("GH_TOKEN", environmentToken)
			api.releaseStatus = http.StatusNotFound
		}, "the GitHub API answered HTTP 404 for release v0.1.31", 1},
		{"a release without the asset", func(t *testing.T, bin string, api *githubAPI) {
			t.Setenv("GH_TOKEN", environmentToken)
			api.release = `{"tag_name":"v0.1.31","assets":[{"id":1,"name":"upkeeper-linux-arm64"}]}`
		}, "has no asset named upkeeper-linux-amd64", 1},
		{"an asset without an id", func(t *testing.T, bin string, api *githubAPI) {
			t.Setenv("GH_TOKEN", environmentToken)
			api.release = `{"assets":[{"name":"upkeeper-linux-amd64"}]}`
		}, "has no asset named upkeeper-linux-amd64", 1},
		{"a malformed release document", func(t *testing.T, bin string, api *githubAPI) {
			t.Setenv("GH_TOKEN", environmentToken)
			api.release = `{"assets":[{"id":"4242",`
		}, "unreadable release document", 1},
		{"a release document of the wrong shape", func(t *testing.T, bin string, api *githubAPI) {
			t.Setenv("GH_TOKEN", environmentToken)
			api.release = `{"assets":{"id":4242}}`
		}, "unreadable release document", 1},
		{"an oversized release document", func(t *testing.T, bin string, api *githubAPI) {
			t.Setenv("GH_TOKEN", environmentToken)
			padding, _ := json.Marshal(strings.Repeat("x", releaseDocumentLimit))
			api.release = `{"body":` + string(padding) + `,"assets":[]}`
		}, "larger than", 1},
		{"a truncated asset body", func(t *testing.T, bin string, api *githubAPI) {
			t.Setenv("GH_TOKEN", environmentToken)
			api.body = api.body[:10]
		}, "the pin says", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := isolateCredentials(t)
			_, releases := serve(t)
			update := newProgram("v0.1.31", "b")
			api, apiServer := serveGitHub(t, update.body, environmentToken)
			test.setup(t, bin, api)
			target := privateInstaller(t, releases, apiServer)
			started := time.Now()
			_, err := target.Apply(update.pin())
			if err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("want an error containing %q, got %v", test.expected, err)
			}
			if test.expected == noCredential && err.Error() != noCredential {
				t.Errorf("error %q, want exactly %q", err.Error(), noCredential)
			}
			if elapsed := time.Since(started); elapsed > 10*time.Second {
				t.Errorf("the refusal took %v", elapsed)
			}
			if len(api.requests) != test.apiRequests {
				t.Errorf("API requests %v, want %d", api.requests, test.apiRequests)
			}
			if target.Layout.CurrentGeneration() != "" {
				t.Errorf("a refused download became current")
			}
			assertNoToken(t, err.Error(), target.Layout.Home)
		})
	}
}

// A test that serves its own assets and names no API never sends a credential
// to GitHub, so the existing refusal for a missing asset is unchanged.
func TestATestReleaseServerWithoutAnAPINeverAuthenticates(t *testing.T) {
	bin := isolateCredentials(t)
	t.Setenv("GH_TOKEN", environmentToken)
	fakeGh(t, bin, "echo called >> \"$0.calls\"\necho "+ghCommandToken+"\n")
	_, releases := serve(t)
	target := installer(t, releases)
	_, err := target.Apply(newProgram("v0.1.31", "b").pin())
	if err == nil || !strings.HasSuffix(err.Error(), ": HTTP 404") {
		t.Fatalf("want the anonymous HTTP 404 refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(bin, "gh.calls")); err == nil {
		t.Errorf("gh auth token ran without an API to send the credential to")
	}
}

// The production defaults: github.com for assets and api.github.com for the
// authenticated lookup.
func TestTheAPIDefaultsToGitHub(t *testing.T) {
	if base := (&Installer{}).apiBase(); base != "https://api.github.com" {
		t.Errorf("default API base %q", base)
	}
	if base := (&Installer{BaseURL: "http://127.0.0.1:1"}).apiBase(); base != "" {
		t.Errorf("a test base URL without an API base authenticates against %q", base)
	}
	if base := (&Installer{BaseURL: "http://a", APIBaseURL: "http://b"}).apiBase(); base != "http://b" {
		t.Errorf("explicit API base %q", base)
	}
}
