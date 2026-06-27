package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// chatStub is a chat.Provider that returns a fixed response body
// + records call count. Used to drive the parser's LLM-backed
// path inside the REST handler without standing up a real
// upstream.
type chatStub struct {
	body  string
	err   error
	calls atomic.Int64
}

func (s *chatStub) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	resp := &chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: s.body}, FinishReason: "stop"})
	return resp, nil
}

func (s *chatStub) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return nil, nil
}
func (s *chatStub) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, nil
}
func (s *chatStub) Model() string              { return "stub" }
func (s *chatStub) SetMetrics(_ *chat.Metrics) {}

// reminderRepoSpy is a minimal in-memory ReminderRepository.
// Only Insert is exercised by these tests; the other methods
// return zero values so the interface is satisfied without
// dragging in a full mock impl.
type reminderRepoSpy struct {
	inserted    []*persistence.Reminder
	rows        map[string]*persistence.Reminder
	cancelled   []string
	deleted     []string
	insertErr   error
	lastListArg persistence.ReminderListFilter
}

func (r *reminderRepoSpy) Insert(_ context.Context, rem *persistence.Reminder) error {
	if r.insertErr != nil {
		return r.insertErr
	}
	if rem.ID == "" {
		rem.ID = "fake-id"
	}
	r.inserted = append(r.inserted, rem)
	return nil
}

func (r *reminderRepoSpy) Get(_ context.Context, id string) (*persistence.Reminder, error) {
	if r.rows != nil {
		rem, ok := r.rows[id]
		if !ok {
			return nil, persistence.ErrNotFound
		}
		cp := *rem
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}
func (r *reminderRepoSpy) List(_ context.Context, filter persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
	r.lastListArg = filter
	if r.rows != nil {
		out := make([]*persistence.Reminder, 0, len(r.rows))
		for _, rem := range r.rows {
			cp := *rem
			out = append(out, &cp)
		}
		return out, nil
	}
	return nil, nil
}
func (r *reminderRepoSpy) LeaseDue(_ context.Context, _ time.Time, _ int) ([]*persistence.Reminder, error) {
	return nil, nil
}
func (r *reminderRepoSpy) MarkFired(_ context.Context, _ string) error      { return nil }
func (r *reminderRepoSpy) MarkErrored(_ context.Context, _, _ string) error { return nil }
func (r *reminderRepoSpy) Reschedule(_ context.Context, _ string, _ time.Time) error {
	return nil
}
func (r *reminderRepoSpy) MarkExpired(_ context.Context, _ string) error { return nil }
func (r *reminderRepoSpy) Cancel(_ context.Context, id string) error {
	r.cancelled = append(r.cancelled, id)
	return nil
}
func (r *reminderRepoSpy) Delete(_ context.Context, id string) error {
	r.deleted = append(r.deleted, id)
	return nil
}
func (r *reminderRepoSpy) CountPendingByOperator(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (r *reminderRepoSpy) UpdateFields(_ context.Context, _ string, _ time.Time, _ string) error {
	return nil
}

// validParserBody is the canonical happy-path LLM response —
// future-dated, parseable, high confidence.
const validParserBody = `{
  "kind": "one_shot",
  "fire_at_utc": "2099-01-01T09:00:00Z",
  "content": "check the deploy",
  "confidence": 0.92,
  "reasoning": "operator said 'in 3 hours'"
}`

func newFromTextServer(t *testing.T, repo persistence.ReminderRepository, provider chat.Provider) *Server {
	t.Helper()
	return NewServer(
		WithLogger(zerolog.Nop()),
		WithReminderRepository(repo),
		WithChatProvider(provider),
	)
}

func postFromText(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/from-text", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.CreateReminderFromText(rec, req)
	return rec
}

func validRequestBody() string {
	return `{
		"text": "in 3 hours check the deploy",
		"operator_id": "telegram:42",
		"channel": "telegram",
		"channel_ref": "42"
	}`
}

// TestCreateFromText_HappyPathCommits — successful parse +
// commit returns 201 with the parsed intent AND the created
// reminder row. The repo received exactly one Insert.
func TestCreateFromText_HappyPathCommits(t *testing.T) {
	repo := &reminderRepoSpy{}
	s := newFromTextServer(t, repo, &chatStub{body: validParserBody})
	rec := postFromText(t, s, validRequestBody())

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp FromTextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Reminder == nil {
		t.Fatalf("missing reminder in response: %+v", resp)
	}
	if resp.Reminder.Content != "check the deploy" {
		t.Errorf("content = %q, want 'check the deploy'", resp.Reminder.Content)
	}
	if len(repo.inserted) != 1 {
		t.Fatalf("expected 1 Insert, got %d", len(repo.inserted))
	}
	if repo.inserted[0].CreatedVia != "api_nl" {
		t.Errorf("created_via=%q, want api_nl", repo.inserted[0].CreatedVia)
	}
}

// TestCreateFromText_DryRunSkipsCommit — DryRun=true returns
// the parsed intent without touching the repo.
func TestCreateFromText_DryRunSkipsCommit(t *testing.T) {
	repo := &reminderRepoSpy{}
	s := newFromTextServer(t, repo, &chatStub{body: validParserBody})
	body := strings.Replace(validRequestBody(), `"channel_ref": "42"`, `"channel_ref": "42", "dry_run": true`, 1)
	rec := postFromText(t, s, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp FromTextResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Reminder != nil {
		t.Errorf("dry run produced reminder: %+v", resp.Reminder)
	}
	if len(repo.inserted) != 0 {
		t.Errorf("dry run hit the repo; inserted=%d", len(repo.inserted))
	}
	if resp.Intent.Content == "" {
		t.Errorf("dry run should still return parsed intent")
	}
}

// TestCreateFromText_RecurringCommitsCronRow exercises the
// cron-recurrence happy path landed alongside migration 67. The
// parser emits a 5-field cron + an optional bound; the handler
// must persist both, and the response surfaces them so the CLI
// confirmation prompt can show the parsed schedule.
func TestCreateFromText_RecurringCommitsCronRow(t *testing.T) {
	const recurringBody = `{
		"kind": "recurring",
		"cron_expr": "0 9 * * 1",
		"recurrence_until_utc": "2027-01-01T00:00:00Z",
		"content": "send the news digest",
		"confidence": 0.95,
		"reasoning": "operator said 'every Monday at 9'"
	}`
	repo := &reminderRepoSpy{}
	s := newFromTextServer(t, repo, &chatStub{body: recurringBody})
	rec := postFromText(t, s, validRequestBody())

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.inserted) != 1 {
		t.Fatalf("inserted=%d, want 1 (recurring commits like any reminder)", len(repo.inserted))
	}
	got := repo.inserted[0]
	if got.CronExpr != "0 9 * * 1" {
		t.Errorf("CronExpr = %q, want '0 9 * * 1'", got.CronExpr)
	}
	if got.RecurrenceUntil == nil {
		t.Errorf("RecurrenceUntil should be persisted")
	}

	// Response carries the parsed intent so the operator can
	// confirm the schedule before commit-via-dry-run-then-real.
	var resp FromTextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Intent.Kind != "recurring" {
		t.Errorf("Intent.Kind = %q, want recurring", resp.Intent.Kind)
	}
	if resp.Intent.CronExpr != "0 9 * * 1" {
		t.Errorf("Intent.CronExpr = %q", resp.Intent.CronExpr)
	}
	if resp.Intent.RecurrenceUntil == "" {
		t.Errorf("Intent.RecurrenceUntil empty; want RFC3339")
	}
	if resp.Reminder == nil || resp.Reminder.CronExpr != "0 9 * * 1" {
		t.Errorf("response reminder row missing cron_expr; got=%+v", resp.Reminder)
	}
}

// TestCreateFromText_RecurringInvalidCronYields422: an invalid
// LLM-emitted cron must not commit; the operator gets
// PARSE_FAILED with the bad expression in the reason.
func TestCreateFromText_RecurringInvalidCronYields422(t *testing.T) {
	const badCronBody = `{
		"kind": "recurring",
		"cron_expr": "not a cron",
		"content": "x",
		"confidence": 0.9,
		"reasoning": "hallucinated"
	}`
	repo := &reminderRepoSpy{}
	s := newFromTextServer(t, repo, &chatStub{body: badCronBody})
	rec := postFromText(t, s, validRequestBody())

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "PARSE_FAILED") {
		t.Errorf("body missing PARSE_FAILED; got=%s", rec.Body.String())
	}
	if len(repo.inserted) != 0 {
		t.Errorf("invalid cron should not commit; inserted=%d", len(repo.inserted))
	}
}

// TestCreateFromText_PastFireAtYields422 — guard against
// committing reminders that fire in the past (the LLM
// occasionally drifts).
func TestCreateFromText_PastFireAtYields422(t *testing.T) {
	const pastBody = `{
		"kind": "one_shot",
		"fire_at_utc": "2000-01-01T00:00:00Z",
		"content": "x",
		"confidence": 0.9,
		"reasoning": "ran late"
	}`
	repo := &reminderRepoSpy{}
	s := newFromTextServer(t, repo, &chatStub{body: pastBody})
	rec := postFromText(t, s, validRequestBody())

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "FIRE_AT_IN_PAST") {
		t.Errorf("error code missing; body=%s", rec.Body.String())
	}
	if len(repo.inserted) != 0 {
		t.Errorf("past fire_at should not commit")
	}
}

// TestCreateFromText_LowConfidenceYields422 — the parser
// rejects low-confidence; the handler surfaces PARSE_FAILED so
// the operator knows to rephrase.
func TestCreateFromText_LowConfidenceYields422(t *testing.T) {
	const lowConfBody = `{
		"kind": "one_shot",
		"fire_at_utc": "2099-01-01T00:00:00Z",
		"content": "x",
		"confidence": 0.3,
		"reasoning": "ambiguous"
	}`
	repo := &reminderRepoSpy{}
	s := newFromTextServer(t, repo, &chatStub{body: lowConfBody})
	rec := postFromText(t, s, validRequestBody())

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "PARSE_FAILED") {
		t.Errorf("error code missing; body=%s", rec.Body.String())
	}
}

// TestCreateFromText_MissingRepoYields503 — when the daemon is
// running without reminders wired the endpoint returns 503 so
// the CLI can fall back gracefully.
func TestCreateFromText_MissingRepoYields503(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()), WithChatProvider(&chatStub{body: validParserBody}))
	rec := postFromText(t, s, validRequestBody())
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "REMINDERS_DISABLED") {
		t.Errorf("error code missing; body=%s", rec.Body.String())
	}
}

// TestCreateFromText_MissingProviderYields503 — same fallback
// when the chat provider isn't configured (operators using the
// manual CLI must still get a clear error, not a panic).
func TestCreateFromText_MissingProviderYields503(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(&reminderRepoSpy{}))
	rec := postFromText(t, s, validRequestBody())
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "PARSER_DISABLED") {
		t.Errorf("error code missing; body=%s", rec.Body.String())
	}
}

// TestCreateFromText_RequiredFieldsValidated — the handler
// rejects requests that lack any of (text, operator_id,
// channel, channel_ref) with 400. Defensive against the CLI
// dropping a required flag.
func TestCreateFromText_RequiredFieldsValidated(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty text", `{"text":"","operator_id":"o","channel":"c","channel_ref":"r"}`},
		{"empty operator", `{"text":"t","operator_id":"","channel":"c","channel_ref":"r"}`},
		{"empty channel", `{"text":"t","operator_id":"o","channel":"","channel_ref":"r"}`},
		{"empty channel_ref", `{"text":"t","operator_id":"o","channel":"c","channel_ref":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFromTextServer(t, &reminderRepoSpy{}, &chatStub{body: validParserBody})
			rec := postFromText(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status=%d, want 400", rec.Code)
			}
		})
	}
}

// TestCreateFromText_BadJSONYields400 — malformed body
// surfaces as 400, not a server-side panic.
func TestCreateFromText_BadJSONYields400(t *testing.T) {
	s := newFromTextServer(t, &reminderRepoSpy{}, &chatStub{body: validParserBody})
	rec := postFromText(t, s, `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

// TestCreateFromText_WrongMethodYields405 — only POST is
// allowed; GET / PUT / DELETE return 405.
func TestCreateFromText_WrongMethodYields405(t *testing.T) {
	s := newFromTextServer(t, &reminderRepoSpy{}, &chatStub{body: validParserBody})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/reminders/from-text", strings.NewReader(""))
		rec := httptest.NewRecorder()
		s.CreateReminderFromText(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method=%s status=%d, want 405", method, rec.Code)
		}
	}
}

// TestCreateFromText_LLMUpstreamErrorYields502 — when the
// chat provider returns a real error, surface 502 so the CLI
// can retry / inform the operator. Not a 500 — the daemon
// itself is healthy.
func TestCreateFromText_LLMUpstreamErrorYields502(t *testing.T) {
	s := newFromTextServer(t, &reminderRepoSpy{}, &chatStub{err: errors.New("upstream timeout")})
	rec := postFromText(t, s, validRequestBody())
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateFromText_RepoInsertErrorYields500 — DB errors at
// commit time surface as 500 since this IS a daemon-side fault.
func TestCreateFromText_RepoInsertErrorYields500(t *testing.T) {
	repo := &reminderRepoSpy{insertErr: errors.New("disk full")}
	s := newFromTextServer(t, repo, &chatStub{body: validParserBody})
	rec := postFromText(t, s, validRequestBody())
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", rec.Code)
	}
}

// TestRemindersRouter_FromTextDispatches confirms the path
// rewrite in remindersRouter sends /from-text to the new
// handler (not to ShowReminder treating "from-text" as an id).
func TestRemindersRouter_FromTextDispatches(t *testing.T) {
	s := newFromTextServer(t, &reminderRepoSpy{}, &chatStub{body: validParserBody})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/from-text", strings.NewReader(validRequestBody()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.remindersRouter(rec, req)
	if rec.Code != http.StatusCreated {
		bodyStr := readAll(t, rec.Body)
		t.Errorf("router didn't dispatch from-text correctly; status=%d body=%s", rec.Code, bodyStr)
	}
}

func TestCreateFromTextRejectsProjectOutsideCallerScope(t *testing.T) {
	repo := &reminderRepoSpy{}
	s := newFromTextServer(t, repo, &chatStub{body: validParserBody})
	body := strings.Replace(validRequestBody(), `"channel_ref": "42"`, `"channel_ref": "42", "project_id": "project-b"`, 1)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/from-text", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-a"}))
	rec := httptest.NewRecorder()
	s.CreateReminderFromText(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.inserted) != 0 {
		t.Fatalf("cross-project reminder should not be inserted")
	}
}

func TestCreateFromTextCapsRequestBody(t *testing.T) {
	s := newFromTextServer(t, &reminderRepoSpy{}, &chatStub{body: validParserBody})
	oversized := `{"text":"` + strings.Repeat("x", maxReminderFromTextBodyBytes+1) + `","operator_id":"telegram:42","channel":"telegram","channel_ref":"42"}`
	rec := postFromText(t, s, oversized)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRemindersListFiltersRowsOutsideCallerScope(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"a": testReminder("a", "project-a", "telegram:42"),
		"b": testReminder("b", "project-b", "telegram:99"),
		"g": testReminder("g", "", "telegram:global"),
	}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-a"}))
	rec := httptest.NewRecorder()
	s.ListReminders(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", rec.Code, rec.Body.String())
	}
	var resp ReminderListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].ID != "a" {
		t.Fatalf("entries=%+v, want only project-a reminder", resp.Entries)
	}
}

func TestShowReminderRejectsRowsOutsideCallerScope(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"b": testReminder("b", "project-b", "telegram:99"),
	}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders/b", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-a"}))
	rec := httptest.NewRecorder()
	s.ShowReminder(rec, req, "b")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCancelReminderRejectsRowsOutsideCallerScope(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"b": testReminder("b", "project-b", "telegram:99"),
	}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/b/cancel", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-a"}))
	rec := httptest.NewRecorder()
	s.CancelReminder(rec, req, "b")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.cancelled) != 0 {
		t.Fatalf("cross-project reminder should not be cancelled: %v", repo.cancelled)
	}
}

// TestDeleteReminder_PhysicallyRemovesRow pins the B-12 manual-
// cleanup primitive. /api/v1/reminders/{id} on DELETE must
// physically remove the row (distinct from cancel which only
// flips status).
func TestDeleteReminder_PhysicallyRemovesRow(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"r-1": testReminder("r-1", "project-a", "telegram:42"),
	}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/reminders/r-1", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.DeleteReminder(rec, req, "r-1")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != "r-1" {
		t.Fatalf("expected r-1 in deleted list, got %v", repo.deleted)
	}
}

// TestDeleteReminder_RejectsCrossProject — same scope enforcement
// as Cancel. A project-A key cannot delete a project-B reminder.
func TestDeleteReminder_RejectsCrossProject(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"b": testReminder("b", "project-b", "telegram:99"),
	}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/reminders/b", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-a"}))
	rec := httptest.NewRecorder()
	s.DeleteReminder(rec, req, "b")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.deleted) != 0 {
		t.Fatalf("cross-project reminder should not be deleted: %v", repo.deleted)
	}
}

// TestDeleteReminder_404OnMissingRow — DELETE on a non-existent id
// returns 404 cleanly so cleanup scripts can ignore "already gone".
func TestDeleteReminder_404OnMissingRow(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/reminders/ghost", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.DeleteReminder(rec, req, "ghost")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRemindersAdminCanSeeGlobalRows(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"g": testReminder("g", "", "telegram:global"),
	}}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithReminderRepository(repo),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders", nil)
	req = req.WithContext(context.WithValue(req.Context(), apiKeyKey, "sk-admin"))
	rec := httptest.NewRecorder()
	s.ListReminders(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", rec.Code, rec.Body.String())
	}
	var resp ReminderListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 1 || resp.Entries[0].ID != "g" {
		t.Fatalf("entries=%+v, want admin-visible global reminder", resp.Entries)
	}
}

// Regression: with API auth disabled (single-tenant local
// install) the list/show/cancel handlers used to drop every row
// because there was no API-key principal to compare against and
// neither the UI nor vornikctl sends X-Operator-Id. Mirrors the
// fix that admin.Middleware + requireAdminGate now apply.
func TestRemindersListWhenAuthDisabledShowsAllRows(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"a": testReminder("a", "project-a", "telegram:42"),
		"b": testReminder("b", "project-b", "telegram:99"),
		"g": testReminder("g", "", "telegram:global"),
	}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.ListReminders(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", rec.Code, rec.Body.String())
	}
	var resp ReminderListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 3 {
		t.Fatalf("entries=%d, want 3 (all rows visible with auth off); got %+v",
			len(resp.Entries), resp.Entries)
	}
}

func TestShowReminderWhenAuthDisabledReturnsRow(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"g": testReminder("g", "", "telegram:global"),
	}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders/g", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.ShowReminder(rec, req, "g")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCancelReminderWhenAuthDisabledCancels(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"g": testReminder("g", "", "telegram:global"),
	}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/g/cancel", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.CancelReminder(rec, req, "g")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.cancelled) != 1 || repo.cancelled[0] != "g" {
		t.Fatalf("expected cancel of 'g'; got %v", repo.cancelled)
	}
}

// Gap 5 regression — `?operator=X` on /api/v1/reminders used to be
// passed straight through to the DB scope, leaking the existence of
// arbitrary operator IDs via timing / row-count side channels even
// though the per-row visibility filter rejected mismatched rows.

func TestRemindersList_ScopedKeyCannotProbeOperatorIDs(t *testing.T) {
	repo := &reminderRepoSpy{}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))

	// Auth-on, scoped to project-a, NOT an admin key. Operator ID
	// from the matched API key (api_key_id:key_42) is what the
	// caller is allowed to see — anything else they try in the
	// query string must be ignored.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders?operator=victim", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, apiKeyKey, "sk-scoped")
	ctx = context.WithValue(ctx, apiKeyIDKey, "key_42")
	ctx = context.WithValue(ctx, projectIDKey, []string{"project-a"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.ListReminders(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", rec.Code, rec.Body.String())
	}
	// The DB filter the handler forwarded MUST be the verified
	// principal — not "victim".
	if repo.lastListArg.OperatorID == "victim" {
		t.Fatalf("scoped key probed operator=victim through DB filter; want it overwritten")
	}
	if repo.lastListArg.OperatorID != "api_key_id:key_42" {
		t.Fatalf("expected operator filter forced to caller's principal, got %q",
			repo.lastListArg.OperatorID)
	}
}

func TestRemindersList_AdminCanQueryAnyOperator(t *testing.T) {
	repo := &reminderRepoSpy{}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithReminderRepository(repo),
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders?operator=telegram:42", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, apiKeyKey, "sk-admin")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.ListReminders(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", rec.Code, rec.Body.String())
	}
	if repo.lastListArg.OperatorID != "telegram:42" {
		t.Fatalf("admin should keep ?operator= override; got %q",
			repo.lastListArg.OperatorID)
	}
}

func TestRemindersList_AuthOffAllowsAnyOperatorQuery(t *testing.T) {
	repo := &reminderRepoSpy{}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders?operator=telegram:42", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.ListReminders(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", rec.Code, rec.Body.String())
	}
	if repo.lastListArg.OperatorID != "telegram:42" {
		t.Fatalf("single-tenant should keep ?operator= override; got %q",
			repo.lastListArg.OperatorID)
	}
}

func testReminder(id, projectID, operatorID string) *persistence.Reminder {
	return &persistence.Reminder{
		ID:         id,
		OperatorID: operatorID,
		Channel:    "telegram",
		ChannelRef: "42",
		ProjectID:  projectID,
		FireAt:     time.Now().Add(time.Hour),
		Content:    "content",
		Status:     persistence.ReminderStatusPending,
		CreatedAt:  time.Now(),
		CreatedVia: "test",
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
