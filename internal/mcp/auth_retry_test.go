package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

// A 401 on a token the daemon believes is still valid means the daemon's
// belief is wrong — the vendor revoked it, a console reset it, or the clock
// drifted. The stored expires_at cannot arbitrate that; only the vendor can.
// So a 401 invalidates the cached credential, forces ONE refresh, and replays
// the call once.
//
// This is not a relaxation of the auth design's "never loop": there is exactly
// one retry and no second attempt. See
// https://docs.vornik.io §5.
func TestAuthFailureTriggersOneRefreshAndRetry(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
			return
		}
		_ = n
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer srv.Close()

	var invalidated int64
	token := "stale"
	c := newTestClient(ServerConfig{
		Name:      "atlassian",
		Transport: "streamable-http",
		URL:       srv.URL,
		AuthHeaderProvider: func(context.Context) (map[string]string, error) {
			return map[string]string{"Authorization": "Bearer " + token}, nil
		},
		AuthInvalidator: func(context.Context) {
			atomic.AddInt64(&invalidated, 1)
			token = "good"
		},
	})

	res, err := c.CallTool(context.Background(), "searchJiraIssuesUsingJql", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("the call should have succeeded after one refresh-and-retry: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	if got := atomic.LoadInt64(&invalidated); got != 1 {
		t.Fatalf("want exactly 1 credential invalidation, got %d", got)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("want exactly 2 upstream attempts (original + one replay), got %d", got)
	}
}

// If the replay also 401s, the call FAILS with a typed auth error. There is no
// second retry — that is the loop the auth design forbids, and it would burn
// rate limit while hiding the condition.
func TestPersistentAuthFailureDoesNotLoop(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer srv.Close()

	c := newTestClient(ServerConfig{
		Name:      "atlassian",
		Transport: "streamable-http",
		URL:       srv.URL,
		AuthHeaderProvider: func(context.Context) (map[string]string, error) {
			return map[string]string{"Authorization": "Bearer dead"}, nil
		},
		AuthInvalidator: func(context.Context) {},
	})

	_, err := c.CallTool(context.Background(), "createJiraIssue", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a persistently rejected credential must fail the call")
	}
	if !IsAuthFailure(err) {
		t.Fatalf("want a typed auth failure, got %T: %v", err, err)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("want exactly 2 attempts and no loop, got %d", got)
	}
}

// A non-auth failure must not be retried at all — a 500 is the vendor's
// problem and replaying it doubles the load on a server already in trouble.
func TestNonAuthFailureIsNotRetried(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(ServerConfig{
		Name: "atlassian", Transport: "streamable-http", URL: srv.URL,
		AuthHeaderProvider: func(context.Context) (map[string]string, error) { return nil, nil },
		AuthInvalidator:    func(context.Context) {},
	})

	_, err := c.CallTool(context.Background(), "x", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if class, ok := ClassOf(err); !ok || class != FailureServer {
		t.Fatalf("want a typed server failure, got %v (class %q ok=%v)", err, class, ok)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("a 5xx must not be replayed; got %d attempts", got)
	}
}

// Without an invalidator there is nothing to refresh, so there is nothing to
// retry — a static credential that 401s is a config error, not a stale token.
func TestNoRetryWithoutAnInvalidator(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(ServerConfig{
		Name: "scraper", Transport: "streamable-http", URL: srv.URL,
		AuthHeaders: map[string]string{"Authorization": "Bearer static"},
	})

	_, err := c.CallTool(context.Background(), "x", json.RawMessage(`{}`))
	if !IsAuthFailure(err) {
		t.Fatalf("want a typed auth failure, got %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("a static credential must not be replayed; got %d attempts", got)
	}
}

func newTestClient(cfg ServerConfig) *Client {
	c := &Client{config: cfg, logger: zerolog.Nop(), httpClient: http.DefaultClient}
	return c
}
