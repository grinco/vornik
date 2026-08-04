package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestA2ACallCov_NilStep covers the nil-step guard.
func TestA2ACallCov_NilStep(t *testing.T) {
	e := &Executor{}
	if _, err := e.handleA2ACallStep(context.Background(), "s", nil); err == nil {
		t.Fatal("nil step should error")
	}
}

// TestA2ACallCov_SubmitResponseUnparseable covers the parse-submit-
// response error branch: the partner returns 200 with a body that
// isn't valid JSON.
func TestA2ACallCov_SubmitResponseUnparseable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "s", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: srv.URL, Prompt: "x",
	})
	if err == nil || !contains(err.Error(), "parse submit response") {
		t.Fatalf("expected parse-submit error, got %v", err)
	}
}

// TestA2ACallCov_SubmitMissingFields covers the "submit response
// missing taskId or streamUrl" guard.
func TestA2ACallCov_SubmitMissingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 with an empty JSON object → both taskId + streamUrl empty.
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "s", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: srv.URL, Prompt: "x",
	})
	if err == nil || !contains(err.Error(), "missing taskId or streamUrl") {
		t.Fatalf("expected missing-fields error, got %v", err)
	}
}

// TestA2ACallCov_StreamURLResolveError covers the handler branch
// where resolveStreamURL fails: the partner returns a relative
// stream URL that doesn't start with '/'.
func TestA2ACallCov_StreamURLResolveError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"taskId":"t1","status":"submitted","streamUrl":"relative-no-slash"}`))
	}))
	defer srv.Close()
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "s", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: srv.URL, Prompt: "x",
	})
	if err == nil || !contains(err.Error(), "resolve stream URL") {
		t.Fatalf("expected stream-URL resolve error, got %v", err)
	}
}

// TestA2ACallCov_CanceledTerminalState covers the "canceled"
// terminal-state arm of the result switch (distinct from "failed").
func TestA2ACallCov_CanceledTerminalState(t *testing.T) {
	partner := newFakePartner(t, []string{
		sseFrame("status", `{"taskId":"task-fake-1","state":"canceled","final":true}`),
	})
	defer partner.Close()
	e := &Executor{}
	res, err := e.handleA2ACallStep(context.Background(), "s", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: partner.URL(), Prompt: "x",
	})
	if err == nil || res == nil || res.State != "canceled" {
		t.Fatalf("expected canceled terminal error, got res=%#v err=%v", res, err)
	}
}
