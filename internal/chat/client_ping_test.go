package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClient_Ping covers the Pinger implementation: static-models
// short-circuit and the live /v1/models call.
func TestClient_Ping(t *testing.T) {
	// Static models path returns nil without any network.
	c := NewClient("https://example", "k", "m",
		WithStaticModelList([]ModelInfo{{ID: "x"}}))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("static Ping: %v", err)
	}

	// Live path hits the test server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4"}]}`))
	}))
	defer srv.Close()
	c2 := NewClient(srv.URL, "k", "gpt-4")
	if err := c2.Ping(context.Background()); err != nil {
		t.Errorf("live Ping: %v", err)
	}

	// Failure path — server returns 500.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oops`))
	}))
	defer bad.Close()
	c3 := NewClient(bad.URL, "k", "m")
	if err := c3.Ping(context.Background()); err == nil {
		t.Error("500 should propagate as Ping error")
	}
}
