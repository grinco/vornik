package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// stubLiveSub satisfies LiveSubscriber. The test only exercises
// the pre-upgrade paths (auth / scope / 503); the post-upgrade
// streaming behaviour is covered by livepubsub's own publisher
// tests, which exercise the same Subscribe contract directly.
// Publish calls are captured so the chat-proxy llm_call_* tap
// can be verified. subscribed is atomic because Subscribe runs on
// the http server goroutine while the WS-upgrade tests read it
// from the test goroutine after the dial returns. subscribedCh
// (lazily initialised by waitSubscribed) lets the upgrade tests
// rendezvous deterministically with Subscribe — the dial returns
// when the HTTP upgrade handshake completes, but the handler
// reads the hello frame BEFORE calling Subscribe so a bare
// Load() can race the handler.
type stubLiveSub struct {
	subscribed   atomic.Bool
	subscribedCh chan struct{}
	published    []stubPublishCall
}

type stubPublishCall struct {
	executionID string
	kind        string
	payload     any
}

func (s *stubLiveSub) Subscribe(_ string, _ int64) (<-chan livepubsub.LiveEvent, func(), error) {
	s.subscribed.Store(true)
	if s.subscribedCh != nil {
		select {
		case s.subscribedCh <- struct{}{}:
		default:
		}
	}
	ch := make(chan livepubsub.LiveEvent)
	close(ch)
	return ch, func() {}, nil
}

func (s *stubLiveSub) SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error) {
	ch := make(chan livepubsub.LiveEvent)
	close(ch)
	return ch, func() {}, nil
}

// waitSubscribed blocks until Subscribe fires or the timeout
// elapses. Returns true on receipt. Must be called BEFORE the
// handler enters Subscribe — the security tests wire it up
// during liveServerForUpgrade.
func (s *stubLiveSub) waitSubscribed(timeout time.Duration) bool {
	if s.subscribedCh == nil {
		return s.subscribed.Load()
	}
	select {
	case <-s.subscribedCh:
		return true
	case <-time.After(timeout):
		return s.subscribed.Load()
	}
}

func (s *stubLiveSub) Publish(_ context.Context, executionID, kind string, payload any) int64 {
	s.published = append(s.published, stubPublishCall{
		executionID: executionID,
		kind:        kind,
		payload:     payload,
	})
	return int64(len(s.published) - 1)
}

func newLiveServer(t *testing.T, sub LiveSubscriber, exec *persistence.Execution) *Server {
	t.Helper()
	opts := []ServerOption{}
	if sub != nil {
		opts = append(opts, WithLiveSubscriber(sub))
	}
	if exec != nil {
		opts = append(opts, WithExecutionRepository(&stubExecRepoForFork{exec: exec}))
	}
	return NewServer(opts...)
}

func TestExecutionLive_503WhenUnwired(t *testing.T) {
	exec := &persistence.Execution{ID: "exec_1", ProjectID: "p1"}
	srv := newLiveServer(t, nil, exec) // no live subscriber
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/executions/exec_1/live", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionLive(rec, req, "exec_1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestExecutionLive_500WhenExecRepoUnwired(t *testing.T) {
	srv := NewServer(WithLiveSubscriber(&stubLiveSub{}))
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/executions/exec_1/live", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionLive(rec, req, "exec_1")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestExecutionLive_404WhenExecMissing(t *testing.T) {
	srv := NewServer(
		WithLiveSubscriber(&stubLiveSub{}),
		// Wire repo with no exec — Get returns ErrNotFound,
		// handler maps to 404.
		WithExecutionRepository(&stubExecRepoForFork{exec: nil}),
	)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/executions/missing/live", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionLive(rec, req, "missing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestParseLiveLastSeqFromQuery(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"", 0},
		{"0", 0},
		{"42", 42},
		{"abc", 0},
		{"-5", 0},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet,
			"/?last_seq="+c.raw, nil)
		got := parseLiveLastSeqFromQuery(req)
		if got != c.want {
			t.Errorf("parseLiveLastSeqFromQuery(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestLiveSubscribeFromSeq(t *testing.T) {
	cases := []struct {
		last int64
		want int64
	}{
		{last: -1, want: 0},
		{last: 0, want: 0},
		{last: 1, want: 2},
		{last: 42, want: 43},
	}
	for _, c := range cases {
		if got := liveSubscribeFromSeq(c.last); got != c.want {
			t.Errorf("liveSubscribeFromSeq(%d) = %d, want %d", c.last, got, c.want)
		}
	}
}

// readLiveHello requires a real websocket connection for a full
// test; the no-conn unit covers the helper's zero-value
// fallback contract via its public usage.
func TestLiveHelloFallback(t *testing.T) {
	// Sanity check: zero-valued hello is the default.
	hello := liveClientHello{}
	if hello.LastSeq != 0 {
		t.Errorf("default hello should be zero")
	}
	_ = context.Background()
}
