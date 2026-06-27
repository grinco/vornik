package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/forge"
)

// TestNew_LoadsKeyAndDefaults covers the happy path: a real PEM key on disk is
// read + parsed, and the API base URL defaults to api.github.com (or honours an
// override).
func TestNew_LoadsKeyAndDefaults(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := New(forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.apiBaseURL != defaultAPIBaseURL {
		t.Errorf("default base url = %q", p.apiBaseURL)
	}

	p2, err := New(forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: path, APIBaseURL: "https://ghe.example/api/v3/"})
	if err != nil {
		t.Fatal(err)
	}
	if p2.apiBaseURL != "https://ghe.example/api/v3" {
		t.Errorf("override base url not trimmed: %q", p2.apiBaseURL)
	}
}

func TestNew_BadKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(path, []byte("not a key"), 0o600)
	if _, err := New(forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: path}); err == nil {
		t.Fatal("want error parsing a non-PEM key")
	}
	if _, err := New(forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: "/no/such/file"}); err == nil {
		t.Fatal("want error reading a missing key file")
	}
}

// TestTokenErrorPropagates: a mint failure surfaces through every method that
// needs auth, rather than proceeding unauthenticated.
func TestTokenErrorPropagates(t *testing.T) {
	p := testProvider("http://unused")
	p.mintFn = func(context.Context, *http.Client, string, int64, int64, *rsa.PrivateKey) (string, time.Time, error) {
		return "", time.Time{}, errors.New("mint boom")
	}
	ctx := context.Background()
	if _, err := p.FetchDiff(ctx, "o/r", 1); err == nil {
		t.Error("FetchDiff should propagate mint error")
	}
	if _, err := p.OpenChangeRequest(ctx, forge.ChangeRequestSpec{Repo: "o/r", Head: "b"}); err == nil {
		t.Error("OpenChangeRequest should propagate mint error")
	}
	if err := p.PostReview(ctx, "o/r", 1, forge.ReviewSpec{}); err == nil {
		t.Error("PostReview should propagate mint error")
	}
	if err := p.PushBranch(ctx, "/d", "o/r", "b", "s"); err == nil {
		t.Error("PushBranch should propagate mint error")
	}
}

func TestOpenChangeRequest_Errors(t *testing.T) {
	ctx := context.Background()

	// Bad repo (no owner).
	if _, err := testProvider("http://unused").OpenChangeRequest(ctx, forge.ChangeRequestSpec{Repo: "noslash", Head: "b"}); err == nil {
		t.Error("want error for repo without owner")
	}

	// List returns non-200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	if _, err := testProvider(srv.URL).OpenChangeRequest(ctx, forge.ChangeRequestSpec{Repo: "o/r", Head: "b"}); err == nil {
		t.Error("want error when list PRs fails")
	}
	srv.Close()

	// List ok (empty), create returns non-201.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "[]")
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, "invalid")
	}))
	if _, err := testProvider(srv2.URL).OpenChangeRequest(ctx, forge.ChangeRequestSpec{Repo: "o/r", Head: "b"}); err == nil {
		t.Error("want error when create PR fails")
	}
	srv2.Close()

	// Create returns 201 but missing html_url.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "[]")
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":5}`)
	}))
	if _, err := testProvider(srv3.URL).OpenChangeRequest(ctx, forge.ChangeRequestSpec{Repo: "o/r", Head: "b"}); err == nil {
		t.Error("want error when created PR lacks html_url")
	}
	srv3.Close()
}

func TestPostReview_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "no perms")
	}))
	defer srv.Close()
	if err := testProvider(srv.URL).PostReview(context.Background(), "o/r", 1, forge.ReviewSpec{Body: "x"}); err == nil {
		t.Error("want error on HTTP 403")
	}
}

func TestHelpers(t *testing.T) {
	if repoOwner("noslash") != "" {
		t.Error("repoOwner of a name without slash should be empty")
	}
	if repoOwner("/r") != "" {
		t.Error("repoOwner with empty owner should be empty")
	}
	long := make([]byte, errBodyExcerpt+50)
	if got := excerpt(long); len(got) != errBodyExcerpt+3 { // +"..."
		t.Errorf("excerpt did not truncate: len=%d", len(got))
	}
	if got := excerpt([]byte("short")); got != "short" {
		t.Errorf("excerpt mangled short body: %q", got)
	}
}

// TestRegisteredViaFactory: the init() registration makes the provider reachable
// through forge.New (the wiring the daemon actually uses). Uses a real temp key.
func TestRegisteredViaFactory(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	path := filepath.Join(t.TempDir(), "key.pem")
	_ = os.WriteFile(path, pemBytes, 0o600)

	p, err := forge.New(forge.Config{
		Provider: forge.ProviderGitHub,
		GitHub:   forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: path},
	})
	if err != nil {
		t.Fatalf("forge.New(github): %v", err)
	}
	if p.Name() != forge.ProviderGitHub {
		t.Errorf("name=%s", p.Name())
	}
}

func TestVerifyPushAccess(t *testing.T) {
	mk := func(contents string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z","permissions":{"contents":"`+contents+`"}}`)
		}))
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)

	srv := mk("write")
	p := &Provider{appID: 1, installationID: 2, key: key, apiBaseURL: srv.URL, httpClient: srv.Client()}
	if err := p.VerifyPushAccess(context.Background()); err != nil {
		t.Errorf("write should verify: %v", err)
	}
	srv.Close()

	srv2 := mk("read")
	p2 := &Provider{appID: 1, installationID: 2, key: key, apiBaseURL: srv2.URL, httpClient: srv2.Client()}
	if err := p2.VerifyPushAccess(context.Background()); err == nil {
		t.Error("read-only should fail verification")
	}
	srv2.Close()
}
