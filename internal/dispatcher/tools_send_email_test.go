// Tests for the send_email tool. Slice 1 of the outbound-email
// rollout: the LLM can compose a fresh email to any recipient on
// the active project's allowlist. Inbound-reply was already
// handled by ChannelReceiver.sendReply; this is the missing fresh-
// compose path.
//
// TDD discipline (feedback_tdd_coverage): tests first, every
// touched file gets ≥1 unit test, touched-surface coverage ≥90%.
package dispatcher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
)

// stubEmailSender captures every SendEmail call so tests can pin
// the routing + argument plumbing without touching SMTP. Records
// the projectID separately because the tool's job is to route to
// the correct project's channel — getting that wrong silently
// would email the wrong domain.
type stubEmailSender struct {
	mu sync.Mutex

	calls       []stubEmailCall
	returnMsgID string
	returnErr   error
}

type stubEmailCall struct {
	ProjectID string
	Req       EmailSendRequest
}

func (s *stubEmailSender) SendEmail(_ context.Context, projectID string, req EmailSendRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubEmailCall{ProjectID: projectID, Req: req})
	if s.returnErr != nil {
		return "", s.returnErr
	}
	if s.returnMsgID != "" {
		return s.returnMsgID, nil
	}
	return "msg-001@vornik.local", nil
}

func (s *stubEmailSender) snapshot() []stubEmailCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubEmailCall(nil), s.calls...)
}

func withEmailSender(es EmailSender) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.emailSender = es }
}

// TestSendEmail_NotConfigured — without an EmailSender wired the
// tool surfaces a clear "not configured" message rather than 500ing.
// The LLM uses this hint to fall back to "I can't send mail, here's
// the content instead" — which is what the user wanted from the
// outset.
func TestSendEmail_NotConfigured(t *testing.T) {
	te := &ToolExecutor{logger: zerolog.Nop()}
	tc := chat.ToolCall{Function: chat.FunctionCall{
		Name:      "send_email",
		Arguments: `{"to":"alice@example.com","subject":"hi","body":"hello"}`,
	}}
	res := te.Execute(context.Background(), tc, "proj-1", nil, 0, nil)
	if !strings.Contains(strings.ToLower(res.Content), "not configured") &&
		!strings.Contains(strings.ToLower(res.Content), "not available") {
		t.Errorf("expected 'not configured' message, got %q", res.Content)
	}
}

// TestSendEmail_InvalidJSON — bad payload shape fails fast with a
// "invalid arguments" message; doesn't reach the sender.
func TestSendEmail_InvalidJSON(t *testing.T) {
	sender := &stubEmailSender{}
	te := newExecutor(withEmailSender(sender))
	tc := chat.ToolCall{Function: chat.FunctionCall{
		Name: "send_email", Arguments: `{not even close to json`,
	}}
	res := te.Execute(context.Background(), tc, "proj-1", nil, 0, nil)
	if !strings.Contains(strings.ToLower(res.Content), "invalid arguments") {
		t.Errorf("expected 'invalid arguments', got %q", res.Content)
	}
	if len(sender.snapshot()) != 0 {
		t.Error("sender must not be called on invalid JSON")
	}
}

// TestSendEmail_MissingRequiredFields — each of to/subject/body is
// required. Surfaced clearly per field so the LLM knows which one
// to supply on the retry.
func TestSendEmail_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"missing to", `{"subject":"x","body":"y"}`, "to is required"},
		{"missing subject", `{"to":"a@b.c","body":"y"}`, "subject is required"},
		{"missing body", `{"to":"a@b.c","subject":"x"}`, "body is required"},
		{"empty strings", `{"to":"","subject":"","body":""}`, "to is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &stubEmailSender{}
			te := newExecutor(withEmailSender(sender))
			call := chat.ToolCall{Function: chat.FunctionCall{Name: "send_email", Arguments: tc.payload}}
			res := te.Execute(context.Background(), call, "proj-1", nil, 0, nil)
			if !strings.Contains(strings.ToLower(res.Content), tc.want) {
				t.Errorf("got %q, want substring %q", res.Content, tc.want)
			}
			if len(sender.snapshot()) != 0 {
				t.Error("sender must not be called on validation failure")
			}
		})
	}
}

// TestSendEmail_NoActiveProject — the tool routes by activeProject;
// an unset active project is unrecoverable from inside the tool so
// surface a clear error. The LLM should call switch_project first.
func TestSendEmail_NoActiveProject(t *testing.T) {
	sender := &stubEmailSender{}
	te := newExecutor(withEmailSender(sender))
	tc := chat.ToolCall{Function: chat.FunctionCall{
		Name:      "send_email",
		Arguments: `{"to":"alice@example.com","subject":"hi","body":"hello"}`,
	}}
	res := te.Execute(context.Background(), tc, "", nil, 0, nil)
	if !strings.Contains(strings.ToLower(res.Content), "project") {
		t.Errorf("expected error mentioning project, got %q", res.Content)
	}
	if len(sender.snapshot()) != 0 {
		t.Error("sender must not be called without an active project")
	}
}

// TestSendEmail_ProjectNotAllowed — session-scope guard. A chat
// pinned to projects [A] mustn't be able to email from project B's
// channel even if B has an email block.
func TestSendEmail_ProjectNotAllowed(t *testing.T) {
	sender := &stubEmailSender{}
	te := newExecutor(withEmailSender(sender))
	tc := chat.ToolCall{Function: chat.FunctionCall{
		Name:      "send_email",
		Arguments: `{"to":"alice@example.com","subject":"hi","body":"hello"}`,
	}}
	res := te.Execute(context.Background(), tc, "proj-B", []string{"proj-A"}, 0, nil)
	if !strings.Contains(strings.ToLower(res.Content), "not permitted") &&
		!strings.Contains(strings.ToLower(res.Content), "not allowed") {
		t.Errorf("expected access denial, got %q", res.Content)
	}
	if len(sender.snapshot()) != 0 {
		t.Error("sender must not be called when project is out of scope")
	}
}

// TestSendEmail_HappyPath — valid payload routes to the active
// project's channel with the right args, returns the Message-ID
// to the LLM so it can confirm the send to the operator.
func TestSendEmail_HappyPath(t *testing.T) {
	sender := &stubEmailSender{returnMsgID: "abc123@vornik.local"}
	te := newExecutor(withEmailSender(sender))
	tc := chat.ToolCall{Function: chat.FunctionCall{
		Name: "send_email",
		Arguments: `{
			"to":"vadim@grinco.eu",
			"subject":"News summary 2026-05-20",
			"body":"Top stories today...\n\n- foo\n- bar"
		}`,
	}}
	res := te.Execute(context.Background(), tc, "assistant", []string{"assistant"}, 0, nil)
	if !strings.Contains(res.Content, "abc123@vornik.local") {
		t.Errorf("response missing Message-ID; got %q", res.Content)
	}
	if !strings.Contains(strings.ToLower(res.Content), "sent") &&
		!strings.Contains(strings.ToLower(res.Content), "delivered") {
		t.Errorf("response should confirm send; got %q", res.Content)
	}

	calls := sender.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sender calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.ProjectID != "assistant" {
		t.Errorf("ProjectID = %q, want assistant", c.ProjectID)
	}
	if c.Req.To != "vadim@grinco.eu" {
		t.Errorf("To = %q", c.Req.To)
	}
	if c.Req.Subject != "News summary 2026-05-20" {
		t.Errorf("Subject = %q", c.Req.Subject)
	}
	if !strings.Contains(c.Req.Body, "Top stories today") {
		t.Errorf("Body = %q", c.Req.Body)
	}
}

// TestSendEmail_SenderError — transport failures surface to the LLM
// in a form the operator can read. We don't try to retry inside the
// tool; the LLM decides whether to retry, supply a fallback, etc.
func TestSendEmail_SenderError(t *testing.T) {
	sender := &stubEmailSender{returnErr: errors.New("smtp: 550 alias not permitted")}
	te := newExecutor(withEmailSender(sender))
	tc := chat.ToolCall{Function: chat.FunctionCall{
		Name:      "send_email",
		Arguments: `{"to":"a@b.c","subject":"x","body":"y"}`,
	}}
	res := te.Execute(context.Background(), tc, "proj-1", []string{"proj-1"}, 0, nil)
	if !strings.Contains(res.Content, "550 alias not permitted") {
		t.Errorf("sender error must surface verbatim; got %q", res.Content)
	}
	// Sender was called even though it errored — that's correct.
	if len(sender.snapshot()) != 1 {
		t.Errorf("sender calls = %d, want 1", len(sender.snapshot()))
	}
}

// TestSendEmail_OptionalInReplyTo — the in_reply_to threading
// header passes through when supplied so the LLM can thread a reply
// onto an existing inbound conversation (e.g. "respond to that
// thread with the summary").
func TestSendEmail_OptionalInReplyTo(t *testing.T) {
	sender := &stubEmailSender{}
	te := newExecutor(withEmailSender(sender))
	tc := chat.ToolCall{Function: chat.FunctionCall{
		Name: "send_email",
		Arguments: `{
			"to":"a@b.c","subject":"Re: hello","body":"replying",
			"in_reply_to":"<original-msg-id@b.c>"
		}`,
	}}
	te.Execute(context.Background(), tc, "proj-1", []string{"proj-1"}, 0, nil)
	calls := sender.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Req.InReplyTo != "<original-msg-id@b.c>" {
		t.Errorf("InReplyTo = %q", calls[0].Req.InReplyTo)
	}
}

// TestDispatcherTools_IncludesSendEmail — the tool definition must
// be registered or the LLM can't discover it. Pin the name + that
// the schema mentions the three required fields so a future edit
// can't drop one and silently break the LLM's contract.
func TestDispatcherTools_IncludesSendEmail(t *testing.T) {
	var found *chat.Tool
	for _, tl := range DispatcherTools() {
		if tl.Function.Name == "send_email" {
			tool := tl
			found = &tool
			break
		}
	}
	if found == nil {
		t.Fatal("send_email tool not registered in DispatcherTools()")
	}
	schema := string(found.Function.Parameters)
	for _, want := range []string{"to", "subject", "body"} {
		if !strings.Contains(schema, want) {
			t.Errorf("tool schema missing %q; got %s", want, schema)
		}
	}
	if !strings.Contains(strings.ToLower(found.Function.Description), "email") {
		t.Errorf("tool description must mention email; got %q", found.Function.Description)
	}
}

// TestExecute_DispatchesSendEmail — the Execute switch routes
// "send_email" to the new handler. Mirrors the existing
// TestExecute_DispatchesAllNamedTools shape for the one new entry.
func TestExecute_DispatchesSendEmail(t *testing.T) {
	te := newExecutor(withEmailSender(&stubEmailSender{}))
	tc := chat.ToolCall{Function: chat.FunctionCall{
		Name:      "send_email",
		Arguments: "{}",
	}}
	res := te.Execute(context.Background(), tc, "proj-1", []string{"proj-1"}, 0, nil)
	if strings.HasPrefix(res.Content, "Unknown tool:") {
		t.Errorf("send_email dispatch missed: %q", res.Content)
	}
}
