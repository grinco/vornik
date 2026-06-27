package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMintInstallationToken_Success(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var gotPath, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_testtoken",
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	}))
	defer srv.Close()

	token, expires, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 4040507, 139940331, key)
	if err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}
	if token != "ghs_testtoken" {
		t.Errorf("token = %q, want ghs_testtoken", token)
	}
	if time.Until(expires) <= 0 {
		t.Errorf("expiry %v should be in the future", expires)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/app/installations/139940331/access_tokens" {
		t.Errorf("path = %q, want the installation access_tokens endpoint", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization = %q, want a Bearer JWT", gotAuth)
	}
}

func TestMintInstallationToken_NotConfigured(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := []struct {
		name             string
		appID, installID int64
		key              *rsa.PrivateKey
	}{
		{"no app id", 0, 1, key},
		{"no installation id", 1, 0, key},
		{"no key", 1, 1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := MintInstallationToken(context.Background(), http.DefaultClient, "https://api.github.com", tc.appID, tc.installID, tc.key)
			if !errors.Is(err, ErrOutboundNotConfigured) {
				t.Errorf("err = %v, want ErrOutboundNotConfigured", err)
			}
		})
	}
}

func TestMintInstallationToken_HTTPError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 1, 1, key)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("err = %v, want an HTTP 404 error", err)
	}
}

func TestCheckContentsWrite(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(contents string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z","permissions":{"contents":"` + contents + `","pull_requests":"write"}}`))
		}))
	}

	srv := mk("write")
	ok, level, err := CheckContentsWrite(context.Background(), srv.Client(), srv.URL, 1, 2, key)
	srv.Close()
	if err != nil || !ok || level != "write" {
		t.Errorf("write perm: ok=%v level=%q err=%v", ok, level, err)
	}

	srv2 := mk("read")
	ok2, level2, err2 := CheckContentsWrite(context.Background(), srv2.Client(), srv2.URL, 1, 2, key)
	srv2.Close()
	if err2 != nil || ok2 || level2 != "read" {
		t.Errorf("read-only perm: ok=%v level=%q err=%v", ok2, level2, err2)
	}

	// missing creds → ErrOutboundNotConfigured
	if _, _, err := CheckContentsWrite(context.Background(), http.DefaultClient, "x", 0, 0, nil); err == nil {
		t.Error("missing creds should error")
	}
}
