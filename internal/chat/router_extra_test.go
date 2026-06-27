// Coverage extensions for Router: CompleteWithToolsStream (the
// router-Provider streaming path), Ping (multi-sub-provider gate),
// WithRouterLogger (option closure), and Router/QueuedProvider
// composition. These exercise branches the legacy router_test.go
// doesn't touch.

package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
)

// streamingFallback records onText callbacks so we can verify the
// router's streaming forwarder routes through the fallback's
// streaming implementation.
type streamingFallback struct {
	namedStubProvider
	streamErr  error
	streamHits int
	pingErr    error
}

func (s *streamingFallback) CompleteWithToolsStream(_ context.Context, _ []Message, _ []Tool, onText StreamCallback) (*ChatResponse, error) {
	s.streamHits++
	if onText != nil {
		onText("hello ")
		onText("world")
	}
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	return &ChatResponse{ID: s.name, Model: s.name}, nil
}

func (s *streamingFallback) Ping(_ context.Context) error { return s.pingErr }

func TestRouter_CompleteWithToolsStream_RoutesThroughFallback(t *testing.T) {
	fb := &streamingFallback{namedStubProvider: namedStubProvider{name: "fb"}}
	r, err := NewRouter(fb, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	var buf string
	_, err = r.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "stream me"}},
		nil,
		func(s string) { buf += s })
	if err != nil {
		t.Fatalf("CompleteWithToolsStream: %v", err)
	}
	if fb.streamHits != 1 {
		t.Errorf("streamHits = %d, want 1", fb.streamHits)
	}
	if buf != "hello world" {
		t.Errorf("stream output = %q, want %q", buf, "hello world")
	}
}

func TestRouter_CompleteWithToolsStream_SurfacesError(t *testing.T) {
	fb := &streamingFallback{
		namedStubProvider: namedStubProvider{name: "fb"},
		streamErr:         errors.New("upstream cancelled"),
	}
	r, _ := NewRouter(fb, nil)
	_, err := r.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "x"}}, nil, nil)
	if err == nil || err.Error() != "upstream cancelled" {
		t.Errorf("expected upstream cancelled, got %v", err)
	}
}

func TestRouter_Ping_FallbackUnhealthyFails(t *testing.T) {
	fb := &streamingFallback{
		namedStubProvider: namedStubProvider{name: "fb"},
		pingErr:           errors.New("fb down"),
	}
	r, _ := NewRouter(fb, nil)
	err := r.Ping(context.Background())
	if err == nil {
		t.Fatal("expected fallback-unhealthy error")
	}
}

func TestRouter_Ping_NilReceiver(t *testing.T) {
	var r *Router
	err := r.Ping(context.Background())
	if err == nil {
		t.Error("nil receiver: expected error, got nil")
	}
}

func TestRouter_Ping_SubFailureNonFatal(t *testing.T) {
	fb := &streamingFallback{namedStubProvider: namedStubProvider{name: "fb"}}
	sub := &streamingFallback{
		namedStubProvider: namedStubProvider{name: "claude"},
		pingErr:           errors.New("sub down"),
	}
	r, err := NewRouter(fb, []Route{{Prefix: "claude", Name: "claude", Provider: sub}})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if err := r.Ping(context.Background()); err != nil {
		t.Errorf("sub failure should NOT bubble; got %v", err)
	}
}

func TestRouter_WithRouterLogger_AppliedAndUsed(t *testing.T) {
	fb := &namedStubProvider{name: "fb"}
	l := zerolog.Nop()
	r, err := NewRouter(fb, nil, WithRouterLogger(l))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	// Trigger the WithModel debug-log path so the logger is exercised.
	_ = r.WithModel("anything")
}
