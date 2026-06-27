package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/forge"
)

// TestPostReview_SelfApprove422_FallsBackToComment is a regression for
// task_20260621234753_5a30265e08d1f09a: a forge review generated an APPROVE
// verdict, but vornik's posting identity was the PR author, so GitHub returned
// 422 "Can not approve your own pull request" and the whole task failed.
// PostReview must downgrade to a COMMENT and retry so the feedback still lands.
func TestPostReview_SelfApprove422_FallsBackToComment(t *testing.T) {
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &m)
		events = append(events, m["event"])
		if len(events) == 1 { // first attempt (APPROVE) → self-approve 422
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"Unprocessable Entity","errors":["Review Can not approve your own pull request"],"status":"422"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	defer srv.Close()

	err := testProvider(srv.URL).PostReview(context.Background(), "o/r", 1,
		forge.ReviewSpec{Body: "looks good", Event: forge.ReviewApprove})
	if err != nil {
		t.Fatalf("self-approve 422 must fall back to COMMENT, got: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 posts (APPROVE then COMMENT retry), got %d: %v", len(events), events)
	}
	if events[0] != "APPROVE" || events[1] != "COMMENT" {
		t.Fatalf("retry must downgrade APPROVE→COMMENT, got %v", events)
	}
}

// TestPostReview_OtherError_NoRetry confirms the fallback is narrow: a 422 that
// is NOT the self-approve case still surfaces as an error and is not retried.
func TestPostReview_OtherError_NoRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"Unprocessable Entity","errors":["Body is too long"],"status":"422"}`)
	}))
	defer srv.Close()

	err := testProvider(srv.URL).PostReview(context.Background(), "o/r", 1,
		forge.ReviewSpec{Body: "x", Event: forge.ReviewApprove})
	if err == nil {
		t.Fatal("a non-self-approve 422 must still error")
	}
	if calls != 1 {
		t.Fatalf("a non-self-approve 422 must not retry, got %d calls", calls)
	}
}
