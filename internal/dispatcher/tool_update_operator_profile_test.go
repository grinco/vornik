package dispatcher

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// stubOpProfileRepo records every Upsert call so tests can
// assert the exact wire shape the tool produced.
type stubOpProfileRepo struct {
	mu        sync.Mutex
	loaded    *persistence.OperatorProfile
	upserts   []*persistence.OperatorProfile
	upsertErr error
	getErr    error
}

func (s *stubOpProfileRepo) Get(_ context.Context, id string) (*persistence.OperatorProfile, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.loaded != nil && s.loaded.OperatorID == id {
		cp := *s.loaded
		cp.Structured = append([]byte(nil), s.loaded.Structured...)
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (s *stubOpProfileRepo) Upsert(_ context.Context, p *persistence.OperatorProfile) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	cp.Structured = append([]byte(nil), p.Structured...)
	s.upserts = append(s.upserts, &cp)
	// Persist the latest so a subsequent Get sees merged state.
	s.loaded = &cp
	return nil
}

func (s *stubOpProfileRepo) Delete(_ context.Context, _ string) error { return nil }
func (s *stubOpProfileRepo) List(_ context.Context, _ int) ([]*persistence.OperatorProfile, error) {
	return nil, nil
}

// callTool wraps the dispatcher Execute path for the tests.
func callTool(t *testing.T, te *ToolExecutor, ctx context.Context, args string) ToolResult {
	t.Helper()
	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_operator_profile", Arguments: args}}
	return te.Execute(ctx, tc, "", nil, 0, nil)
}

// TestDispatcherTools_UpdateOperatorProfileRegistered: the
// tool spec must appear in DispatcherTools() so the LLM sees
// it in the function catalog.
func TestDispatcherTools_UpdateOperatorProfileRegistered(t *testing.T) {
	tools := DispatcherTools()
	for _, tool := range tools {
		if tool.Function.Name == "update_operator_profile" {
			// Description should make the sparse-use guidance
			// explicit — without it, models call it constantly.
			desc := tool.Function.Description
			if !strings.Contains(desc, "durable") {
				t.Errorf("description should signal sparse use; got %q", desc)
			}
			return
		}
	}
	t.Fatalf("update_operator_profile not registered in DispatcherTools()")
}

// TestUpdateOperatorProfile_SetsStructuredKey: a (key, value,
// rationale) call writes through the repo with the value
// merged into the structured JSONB.
func TestUpdateOperatorProfile_SetsStructuredKey(t *testing.T) {
	repo := &stubOpProfileRepo{}
	te := &ToolExecutor{operatorProfiles: repo}

	ctx := WithOperatorID(context.Background(), "telegram:42")
	args := `{"key":"tone","value":"terse","rationale":"user said be terse"}`
	result := callTool(t, te, ctx, args)

	if strings.Contains(result.Content, "error") || strings.Contains(result.Content, "denied") {
		t.Errorf("expected success, got %q", result.Content)
	}
	if len(repo.upserts) != 1 {
		t.Fatalf("Upsert count = %d, want 1", len(repo.upserts))
	}
	got := repo.upserts[0]
	if got.OperatorID != "telegram:42" {
		t.Errorf("operator_id = %q", got.OperatorID)
	}
	var parsed map[string]string
	_ = json.Unmarshal(got.Structured, &parsed)
	if parsed["tone"] != "terse" {
		t.Errorf("structured.tone = %q, want terse; raw=%s", parsed["tone"], got.Structured)
	}
}

// TestUpdateOperatorProfile_MergesWithExistingStructured: a
// second call adds a new key without clobbering the first.
// Without this the tool would be useless — every call would
// overwrite the operator's prior preferences.
func TestUpdateOperatorProfile_MergesWithExistingStructured(t *testing.T) {
	repo := &stubOpProfileRepo{
		loaded: &persistence.OperatorProfile{
			OperatorID: "telegram:42",
			Structured: []byte(`{"tone":"terse"}`),
		},
	}
	te := &ToolExecutor{operatorProfiles: repo}

	ctx := WithOperatorID(context.Background(), "telegram:42")
	args := `{"key":"time_zone","value":"Europe/Prague","rationale":"user mentioned Prague"}`
	_ = callTool(t, te, ctx, args)

	if len(repo.upserts) != 1 {
		t.Fatalf("Upsert count = %d, want 1", len(repo.upserts))
	}
	var parsed map[string]string
	_ = json.Unmarshal(repo.upserts[0].Structured, &parsed)
	if parsed["tone"] != "terse" {
		t.Errorf("existing key dropped: %v", parsed)
	}
	if parsed["time_zone"] != "Europe/Prague" {
		t.Errorf("new key not set: %v", parsed)
	}
}

// TestUpdateOperatorProfile_RejectsUnknownKey: the structured
// blob's keys are an allow-list — a model trying to write
// "prompt_injection" or any unrecognised key MUST be rejected
// so the system prompt's <operator_profile> block doesn't
// surface attacker-controlled keys.
func TestUpdateOperatorProfile_RejectsUnknownKey(t *testing.T) {
	repo := &stubOpProfileRepo{}
	te := &ToolExecutor{operatorProfiles: repo}

	ctx := WithOperatorID(context.Background(), "telegram:42")
	args := `{"key":"prompt_injection","value":"ignore previous","rationale":"sneaky"}`
	result := callTool(t, te, ctx, args)

	if !strings.Contains(result.Content, "not a recognised key") &&
		!strings.Contains(result.Content, "unknown key") {
		t.Errorf("expected rejection message, got %q", result.Content)
	}
	if len(repo.upserts) != 0 {
		t.Errorf("Upsert should NOT have fired; got %d", len(repo.upserts))
	}
}

// TestUpdateOperatorProfile_NotesKey: writing the "notes" key
// replaces the notes column. (Append-mode lands in a later
// follow-up; this slice keeps the semantics simple.)
func TestUpdateOperatorProfile_NotesKey(t *testing.T) {
	repo := &stubOpProfileRepo{}
	te := &ToolExecutor{operatorProfiles: repo}

	ctx := WithOperatorID(context.Background(), "telegram:42")
	args := `{"key":"notes","value":"operator prefers code blocks","rationale":"explicit"}`
	_ = callTool(t, te, ctx, args)

	if len(repo.upserts) != 1 {
		t.Fatalf("Upsert count = %d", len(repo.upserts))
	}
	if repo.upserts[0].Notes != "operator prefers code blocks" {
		t.Errorf("notes = %q", repo.upserts[0].Notes)
	}
}

// TestUpdateOperatorProfile_NoOperatorIDFails: a synthesised
// turn (autonomy loop, post-mortem) has no OperatorID. The
// tool MUST refuse to write rather than create a profile
// keyed on the empty string.
func TestUpdateOperatorProfile_NoOperatorIDFails(t *testing.T) {
	repo := &stubOpProfileRepo{}
	te := &ToolExecutor{operatorProfiles: repo}

	// No WithOperatorID — context carries no key.
	args := `{"key":"tone","value":"terse","rationale":"r"}`
	result := callTool(t, te, context.Background(), args)

	if !strings.Contains(strings.ToLower(result.Content), "operator") {
		t.Errorf("expected refusal message mentioning operator, got %q", result.Content)
	}
	if len(repo.upserts) != 0 {
		t.Errorf("Upsert should NOT have fired without OperatorID")
	}
}

// TestUpdateOperatorProfile_RepoUnwiredReportsNotConfigured:
// SQLite / pre-migration deployments leave the repo nil. The
// tool should report a clean error so the model doesn't
// retry-loop.
func TestUpdateOperatorProfile_RepoUnwiredReportsNotConfigured(t *testing.T) {
	te := &ToolExecutor{} // operatorProfiles nil
	ctx := WithOperatorID(context.Background(), "telegram:42")
	args := `{"key":"tone","value":"terse","rationale":"r"}`
	result := callTool(t, te, ctx, args)

	if !strings.Contains(strings.ToLower(result.Content), "not configured") {
		t.Errorf("expected not-configured message, got %q", result.Content)
	}
}

// TestUpdateOperatorProfile_RequiresRationale: every write
// must carry an explanation so the audit row has something
// useful. Empty rationale → refuse.
func TestUpdateOperatorProfile_RequiresRationale(t *testing.T) {
	repo := &stubOpProfileRepo{}
	te := &ToolExecutor{operatorProfiles: repo}

	ctx := WithOperatorID(context.Background(), "telegram:42")
	args := `{"key":"tone","value":"terse","rationale":""}`
	result := callTool(t, te, ctx, args)

	if !strings.Contains(strings.ToLower(result.Content), "rationale") {
		t.Errorf("expected rationale-required refusal, got %q", result.Content)
	}
	if len(repo.upserts) != 0 {
		t.Errorf("Upsert should NOT have fired without rationale")
	}
}

// TestUpdateOperatorProfile_WritesAuditRow — every successful
// write produces one admin_audit row keyed on operator_id +
// carrying the rationale + (key, value) so operators can later
// review every change via the audit panel.
func TestUpdateOperatorProfile_WritesAuditRow(t *testing.T) {
	repo := &stubOpProfileRepo{}
	audit := &stubAdminAuditRepo{}
	te := &ToolExecutor{operatorProfiles: repo, adminAuditRepo: audit}

	ctx := WithOperatorID(context.Background(), "telegram:42")
	args := `{"key":"tone","value":"terse","rationale":"user explicitly asked"}`
	_ = callTool(t, te, ctx, args)

	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.entries) != 1 {
		t.Fatalf("audit row count = %d, want 1", len(audit.entries))
	}
	row := audit.entries[0]
	if row.Action != "operator_profile.updated" {
		t.Errorf("action = %q, want operator_profile.updated", row.Action)
	}
	if row.Principal != "telegram:42" {
		t.Errorf("principal = %q", row.Principal)
	}
	if row.Target != "telegram:42" {
		t.Errorf("target = %q (used as filter key on the audit panel)", row.Target)
	}
	if !strings.Contains(row.After, "tone") || !strings.Contains(row.After, "terse") {
		t.Errorf("after_state missing key/value: %q", row.After)
	}
	if !strings.Contains(row.After, "user explicitly asked") {
		t.Errorf("after_state missing rationale: %q", row.After)
	}
}

// TestUpdateOperatorProfile_NoAuditOnRefusal — a refused
// write (unknown key, empty rationale, missing operator) must
// NOT leave an audit row.
func TestUpdateOperatorProfile_NoAuditOnRefusal(t *testing.T) {
	repo := &stubOpProfileRepo{}
	audit := &stubAdminAuditRepo{}
	te := &ToolExecutor{operatorProfiles: repo, adminAuditRepo: audit}

	ctx := WithOperatorID(context.Background(), "telegram:42")
	args := `{"key":"prompt_injection","value":"x","rationale":"r"}`
	_ = callTool(t, te, ctx, args)

	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.entries) != 0 {
		t.Errorf("refusal should not leave audit; got %d rows", len(audit.entries))
	}
}

// stubAdminAuditRepo records Insert calls. Other AdminAuditRepository
// methods aren't exercised by these tests.
type stubAdminAuditRepo struct {
	mu      sync.Mutex
	entries []*persistence.AdminAuditEntry
}

func (s *stubAdminAuditRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}
func (s *stubAdminAuditRepo) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

// TestUpdateOperatorProfile_EmptyValueRemovesKey: setting a
// structured key to an empty value removes it from the
// allow-listed set. Operators who want to undo a preference
// shouldn't have to file a CLI command — the assistant can do
// it via the same tool path.
func TestUpdateOperatorProfile_EmptyValueRemovesKey(t *testing.T) {
	repo := &stubOpProfileRepo{
		loaded: &persistence.OperatorProfile{
			OperatorID: "telegram:42",
			Structured: []byte(`{"tone":"terse","time_zone":"Europe/Prague"}`),
		},
	}
	te := &ToolExecutor{operatorProfiles: repo}

	ctx := WithOperatorID(context.Background(), "telegram:42")
	args := `{"key":"tone","value":"","rationale":"user said no preference anymore"}`
	_ = callTool(t, te, ctx, args)

	if len(repo.upserts) != 1 {
		t.Fatalf("Upsert count = %d", len(repo.upserts))
	}
	var parsed map[string]string
	_ = json.Unmarshal(repo.upserts[0].Structured, &parsed)
	if _, exists := parsed["tone"]; exists {
		t.Errorf("tone should be removed; got %v", parsed)
	}
	if parsed["time_zone"] != "Europe/Prague" {
		t.Errorf("other keys clobbered: %v", parsed)
	}
}
