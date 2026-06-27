// Coverage for stream.CompleteWithToolsStream early-error guards
// (empty endpoint / model / messages). These are immediate-return
// branches that don't touch the network — fast wins on coverage
// without faking the SSE stream.

package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientStream_EmptyEndpointRejected(t *testing.T) {
	c := NewClient("", "k", "m")
	_, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, nil)
	if !errors.Is(err, ErrEmptyEndpoint) {
		t.Errorf("err: got %v, want ErrEmptyEndpoint", err)
	}
}

func TestClientStream_EmptyModelRejected(t *testing.T) {
	c := NewClient("https://api.example.com", "k", "")
	_, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, nil)
	if !errors.Is(err, ErrEmptyModel) {
		t.Errorf("err: got %v, want ErrEmptyModel", err)
	}
}

func TestClientStream_EmptyMessagesRejected(t *testing.T) {
	c := NewClient("https://api.example.com", "k", "m")
	_, err := c.CompleteWithToolsStream(context.Background(), nil, nil, nil)
	if !errors.Is(err, ErrEmptyMessages) {
		t.Errorf("err: got %v, want ErrEmptyMessages", err)
	}
}

// Streaming with an HTTP non-200 lands in the error-bytes path —
// covers the recordMetrics(error) leg inside CompleteWithToolsStream.
func TestClientStream_HTTPNon200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`access denied`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m",
		WithHTTPClient(srv.Client()),
		WithTimeout(2))
	_, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Error("HTTP 403: expected error, got nil")
	}
}
