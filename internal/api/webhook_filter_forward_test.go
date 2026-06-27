package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestMatchesWebhookFilter covers the source-level event filter that gates
// task creation (so e.g. pull_request.synchronize doesn't spawn a task).
func TestMatchesWebhookFilter(t *testing.T) {
	event := map[string]any{
		"action": "opened",
		"issue":  map[string]any{"number": float64(7)},
	}
	cases := []struct {
		name   string
		filter string
		want   bool
	}{
		{"empty allows all", "", true},
		{"presence match", "${action}", true},
		{"presence miss", "${pull_request.number}", false},
		{"equality match", "${action}=opened", true},
		{"set membership match", "${action}=labeled,opened", true},
		{"set membership with spaces", "${action}=labeled, opened", true},
		{"equality miss", "${action}=synchronize", false},
		{"set miss", "${action}=labeled,closed", false},
		{"nested path", "${issue.number}=7", true},
		{"malformed drops", "action=opened", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesWebhookFilter(tc.filter, event); got != tc.want {
				t.Fatalf("matchesWebhookFilter(%q) = %v, want %v", tc.filter, got, tc.want)
			}
		})
	}
}

// TestEnqueueVerifiedWebhook_FilterDropsNonMatch: a delivery the filter
// excludes is acknowledged 200 (no provider retry) and creates no task.
func TestEnqueueVerifiedWebhook_FilterDropsNonMatch(t *testing.T) {
	srv, project, source := newWebhookTestServer(t)
	source.Filter = "${action}=opened,labeled"

	created := false
	srv.taskRepo.(*mocks.MockTaskRepository).CreateFunc = func(context.Context, *persistence.Task) error {
		created = true
		return nil
	}

	rec := httptest.NewRecorder()
	srv.enqueueVerifiedWebhook(context.Background(), rec, project, source, []byte(`{"action":"synchronize"}`), "d-1")

	if rec.Code != 200 {
		t.Fatalf("filtered delivery should ack 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "filtered") {
		t.Errorf("response should report filtered, got %s", rec.Body.String())
	}
	if created {
		t.Error("filtered delivery must not create a task")
	}
}

// TestEnqueueVerifiedWebhook_FilterAllowsMatch: a matching delivery still
// creates a task (202).
func TestEnqueueVerifiedWebhook_FilterAllowsMatch(t *testing.T) {
	srv, project, source := newWebhookTestServer(t)
	source.Filter = "${action}=opened,labeled"

	rec := httptest.NewRecorder()
	srv.enqueueVerifiedWebhook(context.Background(), rec, project, source, []byte(`{"action":"opened"}`), "d-2")

	if rec.Code != 202 {
		t.Fatalf("matching delivery should create a task (202), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestEnqueueVerifiedWebhook_ForwardPayloadSetsContext: with forward_payload
// the created task carries the verified event body in Context.prompt so the
// agent can act on the specific PR/issue.
func TestEnqueueVerifiedWebhook_ForwardPayloadSetsContext(t *testing.T) {
	srv, project, source := newWebhookTestServer(t)
	source.ForwardPayload = true

	var captured *persistence.Task
	srv.taskRepo.(*mocks.MockTaskRepository).CreateFunc = func(_ context.Context, task *persistence.Task) error {
		captured = task
		return nil
	}

	rec := httptest.NewRecorder()
	body := `{"action":"opened","pull_request":{"number":7}}`
	srv.enqueueVerifiedWebhook(context.Background(), rec, project, source, []byte(body), "d-3")

	if rec.Code != 202 {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if captured == nil {
		t.Fatal("no task created")
	}
	var payload struct {
		Context struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if err := json.Unmarshal(captured.Payload, &payload); err != nil {
		t.Fatalf("unmarshal task payload: %v", err)
	}
	if payload.Context.Prompt != body {
		t.Errorf("Context.prompt = %q, want the verified event body %q", payload.Context.Prompt, body)
	}
}

// TestEnqueueVerifiedWebhook_NoForwardPayloadLeavesContextEmpty: default
// behaviour is unchanged — no payload is forwarded.
func TestEnqueueVerifiedWebhook_NoForwardPayloadLeavesContextEmpty(t *testing.T) {
	srv, project, source := newWebhookTestServer(t)
	// source.ForwardPayload defaults false

	var captured *persistence.Task
	srv.taskRepo.(*mocks.MockTaskRepository).CreateFunc = func(_ context.Context, task *persistence.Task) error {
		captured = task
		return nil
	}

	rec := httptest.NewRecorder()
	srv.enqueueVerifiedWebhook(context.Background(), rec, project, source, []byte(`{"action":"opened"}`), "d-4")

	if captured == nil {
		t.Fatal("no task created")
	}
	var payload struct {
		Context json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(captured.Payload, &payload); err != nil {
		t.Fatalf("unmarshal task payload: %v", err)
	}
	if len(payload.Context) != 0 {
		t.Errorf("Context should be empty without forward_payload, got %s", payload.Context)
	}
}
