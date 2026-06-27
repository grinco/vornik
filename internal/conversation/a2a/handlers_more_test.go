package a2a

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// --- task submit: push-config integration --------------------

// TestTaskSubmit_PersistsInlinePushConfig confirms a submit body carrying a
// valid pushNotificationConfig persists the webhook keyed on the new task ID,
// including the bearer token.
func TestTaskSubmit_PersistsInlinePushConfig(t *testing.T) {
	h, creator := newTestHandler()
	store := newMemPushStore()
	h.PushConfigStore = store

	body := bytes.NewBufferString(`{
		"message":{"parts":[{"type":"text","text":"go"}]},
		"configuration":{"pushNotificationConfig":{"url":"https://caller.example.com/hook","token":"abc"}}
	}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	_ = creator
	// fakeTaskCreator returns id "task-<project>-1".
	saved, ok := store.m["task-demo-1"]
	if !ok {
		t.Fatalf("push config not persisted for the created task; store=%+v", store.m)
	}
	if saved.URL != "https://caller.example.com/hook" || saved.Token != "abc" {
		t.Errorf("persisted config = %+v", saved)
	}
}

// TestTaskSubmit_BadPushURLRejectedBeforeCreate proves the webhook is validated
// BEFORE task creation — a bad URL is a clean 422 and no task is created.
func TestTaskSubmit_BadPushURLRejectedBeforeCreate(t *testing.T) {
	h, creator := newTestHandler()
	h.PushConfigStore = newMemPushStore()

	body := bytes.NewBufferString(`{
		"message":{"parts":[{"type":"text","text":"go"}]},
		"configuration":{"pushNotificationConfig":{"url":"http://169.254.169.254/latest/meta-data"}}
	}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for link-local metadata URL", rec.Code)
	}
	if creator.lastParams.Prompt != "" {
		t.Errorf("task must NOT be created when the push URL is invalid")
	}
}

// TestTaskSubmit_PushConfigUnsupportedWhenNoStore: a submit carrying a webhook
// but no store wired is a 422 PUSH_UNSUPPORTED, not a silent drop.
func TestTaskSubmit_PushConfigUnsupportedWhenNoStore(t *testing.T) {
	h, _ := newTestHandler() // no PushConfigStore
	body := bytes.NewBufferString(`{
		"message":{"parts":[{"type":"text","text":"go"}]},
		"configuration":{"pushNotificationConfig":{"url":"https://caller.example.com/hook"}}
	}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 PUSH_UNSUPPORTED", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "PUSH_UNSUPPORTED") {
		t.Errorf("expected PUSH_UNSUPPORTED code, got %s", rec.Body.String())
	}
}

// TestTaskSubmit_NoCreatorIs503 covers the "TaskCreator not configured" arm.
func TestTaskSubmit_NoCreatorIs503(t *testing.T) {
	h, _ := newTestHandler()
	h.TaskCreator = nil
	body := bytes.NewBufferString(`{"message":{"parts":[{"type":"text","text":"go"}]}}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestTaskSubmit_CreatorErrorIs400 maps a creator failure to a 400
// TASK_CREATE_FAILED carrying the underlying error message.
func TestTaskSubmit_CreatorErrorIs400(t *testing.T) {
	h, creator := newTestHandler()
	creator.returnErr = errors.New("queue is full")
	body := bytes.NewBufferString(`{"message":{"parts":[{"type":"text","text":"go"}]}}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "queue is full") {
		t.Errorf("expected wrapped creator error, got %s", rec.Body.String())
	}
}

// TestTaskSubmit_TrailingJSONRejected: extra bytes after the JSON object are a
// 400 (the handler enforces a single object, no trailing garbage).
func TestTaskSubmit_TrailingJSONRejected(t *testing.T) {
	h, _ := newTestHandler()
	body := bytes.NewBufferString(`{"message":{"parts":[{"type":"text","text":"go"}]}}{"sneaky":true}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for trailing JSON", rec.Code)
	}
}

// TestTaskSubmit_MultiTextPartsConcatenated verifies the prompt joins multiple
// text parts (blank-line separated) and skips non-text / blank parts.
func TestTaskSubmit_MultiTextPartsConcatenated(t *testing.T) {
	h, creator := newTestHandler()
	body := bytes.NewBufferString(`{"message":{"parts":[
		{"type":"text","text":"first"},
		{"type":"file","text":"ignored"},
		{"type":"text","text":"  "},
		{"type":"text","text":"second"}
	]}}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if creator.lastParams.Prompt != "first\n\nsecond" {
		t.Errorf("prompt = %q, want %q", creator.lastParams.Prompt, "first\n\nsecond")
	}
}

func TestExtractTextPrompt_AllNonTextIsEmpty(t *testing.T) {
	got := extractTextPrompt([]struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{
		{Type: "file", Text: "x"},
		{Type: "image", Text: "y"},
	})
	if got != "" {
		t.Errorf("non-text parts must yield empty prompt, got %q", got)
	}
}

// --- HandleAgentRoute routing edges ---------------------------

func TestHandleAgentRoute_UnknownSuffixIs404(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/bananas", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown suffix", rec.Code)
	}
}

func TestHandleAgentRoute_CardMethodGuard(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/card", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 for POST on card", rec.Code)
	}
}

// --- pushNotificationConfig endpoint edges --------------------

// TestPushConfigSet_TrailingJSONRejected guards the set endpoint's strict
// single-object decode.
func TestPushConfigSet_TrailingJSONRejected(t *testing.T) {
	h := pushConfigHandler(t, newMemPushStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/a2a/v1/agents/demo/research/tasks/demo-task/pushNotificationConfig",
		strings.NewReader(`{"url":"https://caller.example.com/hook"}{}`))
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for trailing JSON", rec.Code)
	}
}

func TestPushConfig_MethodGuard(t *testing.T) {
	h := pushConfigHandler(t, newMemPushStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete,
		"/a2a/v1/agents/demo/research/tasks/demo-task/pushNotificationConfig", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 for DELETE", rec.Code)
	}
}

// TestPushConfig_SetStoreErrorIs500 covers the persistence-failure arm of the
// set handler.
func TestPushConfig_SetStoreErrorIs500(t *testing.T) {
	h := pushConfigHandler(t, errPushStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/a2a/v1/agents/demo/research/tasks/demo-task/pushNotificationConfig",
		strings.NewReader(`{"url":"https://caller.example.com/hook"}`))
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on store failure", rec.Code)
	}
}

// TestPushConfig_NotConfiguredWhenLookupUnwired covers the 503 arm taken when
// the task-lookup surface (streamDeps) isn't wired at all.
func TestPushConfig_NotConfiguredWhenLookupUnwired(t *testing.T) {
	h, _ := newTestHandler()
	h.PushConfigStore = newMemPushStore()
	prev := streamDeps
	WireSSE(nil) // no task lookup
	t.Cleanup(func() { streamDeps = prev })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/a2a/v1/agents/demo/research/tasks/demo-task/pushNotificationConfig", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when task lookup unwired", rec.Code)
	}
}

// errPushStore fails every Set so the 500 arm is reachable; Get is unused here.
type errPushStore struct{}

func (errPushStore) Set(context.Context, persistence.A2APushConfig) error {
	return errors.New("disk on fire")
}
func (errPushStore) Get(context.Context, string) (*persistence.A2APushConfig, error) {
	return nil, persistence.ErrNotFound
}

// --- push notifier: validation + retry/edge behaviour ---------

// TestValidateWebhookURL_AltEncodingsPassValidation pins the documented
// contract: alternate IPv4 encodings (decimal, hex, octal) are NOT rejected at
// validation time — net.ParseIP returns nil for them, so the connect-time
// dialer guard is the authority. This is a behavioural contract, not a wish.
func TestValidateWebhookURL_AltEncodingsPassValidation(t *testing.T) {
	// These all encode 127.0.0.1 but ParseIP can't see that from the host
	// string, so validation lets them through (caught later at dial).
	for _, u := range []string{
		"http://2130706433/x", // decimal
		"http://0x7f000001/x", // hex
	} {
		if err := ValidateWebhookURL(u); err != nil {
			t.Errorf("ValidateWebhookURL(%q) = %v; alt encodings are deferred to the connect-time guard, want nil", u, err)
		}
	}
}

func TestValidateWebhookURL_PublicIPv6Accepted(t *testing.T) {
	if err := ValidateWebhookURL("https://[2606:4700:4700::1111]/hook"); err != nil {
		t.Errorf("public IPv6 literal should pass, got %v", err)
	}
}

func TestValidateWebhookURL_NoHostRejected(t *testing.T) {
	// scheme but no host
	if err := ValidateWebhookURL("https:///path-only"); err == nil {
		t.Errorf("URL with no host must be rejected")
	}
}

func TestValidateWebhookURL_WhitespaceTrimmed(t *testing.T) {
	if err := ValidateWebhookURL("  https://caller.example.com/hook  "); err != nil {
		t.Errorf("surrounding whitespace should be trimmed, got %v", err)
	}
}

func TestIsBlockedPushIP_PublicAddressAllowed(t *testing.T) {
	if isBlockedPushIP(net.ParseIP("8.8.8.8")) {
		t.Errorf("8.8.8.8 is public and must NOT be blocked")
	}
	if isBlockedPushIP(net.ParseIP("2606:4700:4700::1111")) {
		t.Errorf("public IPv6 must NOT be blocked")
	}
	// nil parse → blocked (the guard treats an unparseable IP as unsafe).
	if !isBlockedPushIP(nil) {
		t.Errorf("nil IP must be treated as blocked")
	}
}

// TestPushNotifier_NilRepoIsNoOp: a notifier built with a nil repo never
// panics and never POSTs.
func TestPushNotifier_NilRepoIsNoOp(t *testing.T) {
	n := NewPushNotifier(nil, zerolog.Nop())
	// Must not panic.
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1"}, true, "done")
	n.NotifySteeringRequired(context.Background(), &persistence.Task{ID: "t1"}, "AWAITING_APPROVAL")
}

// TestPushNotifier_NilTaskIsNoOp: both notifier hooks tolerate a nil task.
func TestPushNotifier_NilTaskIsNoOp(t *testing.T) {
	n := NewPushNotifier(fakePushRepo{cfg: &persistence.A2APushConfig{TaskID: "t1", URL: "https://x/y"}}, zerolog.Nop())
	n.NotifyTaskCompleted(context.Background(), nil, true, "done")
	n.NotifySteeringRequired(context.Background(), nil, "AWAITING_INPUT")
}

// TestPushNotifier_RetriesOnServerError: a 5xx response triggers the single
// retry, so the webhook is hit exactly twice and the failure stays non-fatal.
func TestPushNotifier_RetriesOnServerError(t *testing.T) {
	allowLoopbackForTest(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	n := NewPushNotifier(fakePushRepo{cfg: &persistence.A2APushConfig{TaskID: "t1", URL: srv.URL}}, zerolog.Nop())
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1"}, true, "done")
	if hits != 2 {
		t.Errorf("5xx should trigger one retry → 2 hits, got %d", hits)
	}
}

// TestPushNotifier_EmptyURLConfigNoPost: a stored config with an empty URL is
// skipped, not dialed.
func TestPushNotifier_EmptyURLConfigNoPost(t *testing.T) {
	allowLoopbackForTest(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// URL empty → push() returns before building any request.
	n := NewPushNotifier(fakePushRepo{cfg: &persistence.A2APushConfig{TaskID: "t1", URL: ""}}, zerolog.Nop())
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1"}, true, "done")
	if hits != 0 {
		t.Errorf("empty-URL config must not POST, got %d hits", hits)
	}
}

// TestPushNotifier_NoAuthHeaderWhenTokenEmpty: omitting the token means no
// Authorization header is sent (vs. a "Bearer " with an empty value).
func TestPushNotifier_NoAuthHeaderWhenTokenEmpty(t *testing.T) {
	allowLoopbackForTest(t)
	gotAuth := "unset"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	n := NewPushNotifier(fakePushRepo{cfg: &persistence.A2APushConfig{TaskID: "t1", URL: srv.URL}}, zerolog.Nop())
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1"}, true, "done")
	if gotAuth != "" {
		t.Errorf("no token → no Authorization header, got %q", gotAuth)
	}
}
