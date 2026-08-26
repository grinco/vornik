package mcp

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A 401 must be nameable. Before this, mcpHTTPStatusError produced
// fmt.Errorf("streamable-http server returned %d") — an untyped string that
// nothing could count, alert on, or gate a workflow step against, which is
// gap 1 of the 2026-08-25 P0. See
// https://docs.vornik.io §3.2.
func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		status int
		want   FailureClass
	}{
		{http.StatusUnauthorized, FailureAuth},
		{http.StatusForbidden, FailureAuth},
		{http.StatusTooManyRequests, FailureRateLimit},
		{http.StatusNotFound, FailureNotFound},
		{http.StatusBadRequest, FailureInvalidRequest},
		{http.StatusInternalServerError, FailureServer},
		{http.StatusBadGateway, FailureServer},
		{http.StatusTeapot, FailureInvalidRequest},
	}
	for _, tc := range cases {
		if got := ClassifyHTTPStatus(tc.status); got != tc.want {
			t.Errorf("ClassifyHTTPStatus(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// The class must survive errors.As through the wrapping the call path does,
// because that is how CallMCPTool reads it back out.
func TestCallErrorIsMatchable(t *testing.T) {
	base := &CallError{Server: "atlassian", Tool: "searchJiraIssuesUsingJql", Status: 401, Class: FailureAuth}
	wrapped := errors.New("outer: " + base.Error())
	_ = wrapped

	var got *CallError
	if !errors.As(error(base), &got) {
		t.Fatal("CallError must be matchable with errors.As")
	}
	if got.Class != FailureAuth {
		t.Fatalf("class lost: %q", got.Class)
	}
	if !IsAuthFailure(base) {
		t.Fatal("IsAuthFailure must report true for a 401 CallError")
	}
	if IsAuthFailure(errors.New("some other failure")) {
		t.Fatal("IsAuthFailure must not guess from arbitrary errors — no text sniffing")
	}
}

// The message must name the server and the status, because it is what an
// operator reads in a failed step.
func TestCallErrorMessageNamesServerAndStatus(t *testing.T) {
	e := &CallError{Server: "atlassian", Tool: "createJiraIssue", Status: 401, Class: FailureAuth}
	msg := e.Error()
	for _, want := range []string{"atlassian", "createJiraIssue", "401", "auth"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not mention %q", msg, want)
		}
	}
}
