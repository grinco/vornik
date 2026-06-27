package hallucination

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// Live evidence motivating these tests:
//
//   judge abstained: LLM error: gateway error 429:
//     [{ "error": { "code": 429, "message": "The request queue is full.",
//        "status": "RESOURCE_EXHAUSTED" } } ]
//   judge abstained: LLM error: request failed: Post
//     "https://aiplatform.googleapis.com/...": unexpected EOF
//
// Both are transient infra failures the judge used to surface as
// terminal abstain verdicts. Post-fix the judge retries with
// capped exponential backoff before giving up.

func TestIsJudgeRetryableErr_GatewayStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{429, true}, // RESOURCE_EXHAUSTED
		{500, true},
		{502, true},
		{503, true},
		{504, true},
		{400, false}, // bad request — won't be helped by retry
		{401, false}, // unauthorized
		{403, false}, // forbidden
		{404, false}, // not found
		{200, false}, // wouldn't be an error path; defensive
	}
	for _, c := range cases {
		err := &chat.GatewayError{Status: c.status, Message: "test"}
		got := isJudgeRetryableErr(err)
		if got != c.want {
			t.Errorf("status %d: got retryable=%v want %v", c.status, got, c.want)
		}
	}
}

func TestIsJudgeRetryableErr_ConnectionShapes(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"unexpected EOF", true},                             // live: Vertex AI mid-stream drop
		{`request failed: Post "...": unexpected EOF`, true}, // live: full message shape
		{"connection reset by peer", true},
		{"connection refused", true},
		{"broken pipe", true},
		{"i/o timeout", true},
		{"context deadline exceeded", true},
		{"RESOURCE_EXHAUSTED", true},
		{"queue is full", true},
		{"invalid request: missing field 'model'", false}, // permanent
		{"unauthorized", false},                           // permanent
		{"model not found", false},                        // permanent
	}
	for _, c := range cases {
		err := errors.New(c.msg)
		if got := isJudgeRetryableErr(err); got != c.want {
			t.Errorf("%q: got retryable=%v want %v", c.msg, got, c.want)
		}
	}
}

func TestIsJudgeRetryableErr_NilSafe(t *testing.T) {
	if isJudgeRetryableErr(nil) {
		t.Error("nil err must NOT be retryable")
	}
}

// fakeProvider counts Complete calls and returns scripted errors
// in sequence. nil = success.
type fakeProvider struct {
	calls atomic.Int32
	seq   []error
}

func (f *fakeProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	idx := int(f.calls.Add(1)) - 1
	if idx >= len(f.seq) {
		return &chat.ChatResponse{}, nil
	}
	if f.seq[idx] == nil {
		return &chat.ChatResponse{}, nil
	}
	return nil, f.seq[idx]
}

// Other Provider methods unimplemented — judge only calls Complete.
func (f *fakeProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	panic("not implemented")
}
func (f *fakeProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	panic("not implemented")
}
func (f *fakeProvider) Model() string            { return "fake" }
func (f *fakeProvider) SetMetrics(*chat.Metrics) {}

func TestCompleteWithRetry_SucceedsAfterTransient(t *testing.T) {
	fp := &fakeProvider{
		seq: []error{
			&chat.GatewayError{Status: 429, Message: "queue full"},
			&chat.GatewayError{Status: 503, Message: "transient"},
			nil, // succeed on attempt 3
		},
	}
	// Capped backoff per attempt (500ms + 2s) — bound the test
	// runtime via a 5s context.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := completeWithRetry(ctx, fp, nil, 3)
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if got := fp.calls.Load(); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

func TestCompleteWithRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	fp := &fakeProvider{
		seq: []error{
			&chat.GatewayError{Status: 429, Message: "queue full"},
			&chat.GatewayError{Status: 429, Message: "queue full"},
			&chat.GatewayError{Status: 429, Message: "queue full"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := completeWithRetry(ctx, fp, nil, 3)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := fp.calls.Load(); got != 3 {
		t.Errorf("expected exactly maxAttempts=3 calls, got %d", got)
	}
}

func TestCompleteWithRetry_PermanentErrorNoRetry(t *testing.T) {
	fp := &fakeProvider{
		seq: []error{
			&chat.GatewayError{Status: 401, Message: "unauthorized"},
		},
	}
	_, err := completeWithRetry(context.Background(), fp, nil, 3)
	if err == nil {
		t.Fatal("expected error to bubble up")
	}
	if got := fp.calls.Load(); got != 1 {
		t.Errorf("permanent error should not retry; got %d calls", got)
	}
}

func TestCompleteWithRetry_ContextCancelStops(t *testing.T) {
	fp := &fakeProvider{
		seq: []error{
			&chat.GatewayError{Status: 503, Message: "down"},
			&chat.GatewayError{Status: 503, Message: "down"},
		},
	}
	// Context cancels before the second backoff completes.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := completeWithRetry(ctx, fp, nil, 3)
	if err == nil {
		t.Fatal("expected error on context cancel")
	}
	// Either ctx.Err() (cancelled mid-backoff) or the underlying
	// transient error if the first call burned the budget. Both
	// acceptable; the contract is "don't keep retrying past
	// cancellation".
	if got := fp.calls.Load(); got > 2 {
		t.Errorf("should stop at context cancel; got %d calls", got)
	}
}

func TestCompleteWithRetry_UnexpectedEOFRetried(t *testing.T) {
	// The exact shape Google Vertex AI emits when a long
	// connection drops. Pre-fix this caused immediate abstain.
	fp := &fakeProvider{
		seq: []error{
			io.ErrUnexpectedEOF,
			nil,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := completeWithRetry(ctx, fp, nil, 3)
	if err != nil {
		t.Fatalf("unexpected EOF should retry to success, got %v", err)
	}
	if got := fp.calls.Load(); got != 2 {
		t.Errorf("expected 2 attempts (one fail + one success), got %d", got)
	}
}
