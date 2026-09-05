package auditredact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// recordingChatRepo is the inner repository the decorator wraps: it stores what
// it is handed, hashing the body itself the way the real repositories do.
type recordingChatRepo struct {
	prompts map[string]string
	entries []*persistence.ChatAuditEntry
}

func (r *recordingChatRepo) Insert(_ context.Context, e *persistence.ChatAuditEntry) error {
	cp := *e
	r.entries = append(r.entries, &cp)
	return nil
}

func (r *recordingChatRepo) List(context.Context, persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	return nil, nil
}

func (r *recordingChatRepo) GetByID(_ context.Context, id string) (*persistence.ChatAuditEntry, error) {
	for _, e := range r.entries {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, persistence.ErrNotFound
}

func (r *recordingChatRepo) GetChatAuditsByTurnIDs(context.Context, []string) (map[string]persistence.ChatAuditEntry, error) {
	return map[string]persistence.ChatAuditEntry{}, nil
}

func (r *recordingChatRepo) SavePrompt(_ context.Context, body string) (string, error) {
	if r.prompts == nil {
		r.prompts = map[string]string{}
	}
	h := persistence.HashChatSystemPrompt(body)
	r.prompts[h] = body
	return h, nil
}

func (r *recordingChatRepo) GetPrompt(_ context.Context, hash string) (string, error) {
	if b, ok := r.prompts[hash]; ok {
		return b, nil
	}
	return "", persistence.ErrNotFound
}

func TestRecordingChatRepo_KeepsTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "ChatAuditRepository.GetByID", (&recordingChatRepo{}).GetByID)
}

// A chat system prompt carrying a credential is stored REDACTED and the hash
// names the stored bytes — the seam design's rule, applied to the store that
// did not have it (chat-audit retention and redaction design §3).
func TestChatAudit_RedactsPromptBeforeStoreAndHashesAfter(t *testing.T) {
	inner := &recordingChatRepo{}
	audit := &recordingAudit{}
	logger := zerolog.Nop()
	c := NewChatAudit(inner, detector(t), audit, &logger)

	raw := "You are the assistant. Deploy with AKIAIOSFODNN7EXAMPLE if asked."
	hash, err := c.SavePrompt(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	stored := inner.prompts[hash]
	if strings.Contains(stored, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("credential stored in plaintext: %q", stored)
	}
	if !strings.Contains(stored, "[REDACTED:") {
		t.Fatalf("no redaction marker in the stored body: %q", stored)
	}
	if hash != persistence.HashChatSystemPrompt(stored) {
		t.Fatal("the returned hash must be the digest of the STORED (redacted) bytes")
	}
	if hash == persistence.HashChatSystemPrompt(raw) {
		t.Fatal("the hash must not name the raw bytes")
	}
	if len(audit.events) == 0 {
		t.Fatal("the redaction was not recorded in secret_redaction_audit")
	}

	// A clean prompt passes through byte-identical, under its own hash.
	clean := "You are the assistant."
	h2, err := c.SavePrompt(context.Background(), clean)
	if err != nil || h2 != persistence.HashChatSystemPrompt(clean) || inner.prompts[h2] != clean {
		t.Fatalf("clean body altered: %q %v", inner.prompts[h2], err)
	}
}

// The ROW is scanned too, not just the prompt: a key pasted into chat lands in
// user_message, and a tool that echoes one lands in tool_calls_json. Design
// §3.2 — half a seam is the failure mode the silent-controls audit describes.
func TestChatAudit_RedactsTheRowsFreeText(t *testing.T) {
	inner := &recordingChatRepo{}
	audit := &recordingAudit{}
	logger := zerolog.Nop()
	c := NewChatAudit(inner, detector(t), audit, &logger)

	err := c.Insert(context.Background(), &persistence.ChatAuditEntry{
		ID:            "turn-1",
		UserMessage:   "deploy with AKIAIOSFODNN7EXAMPLE please",
		Response:      "I used AKIAIOSFODNN7EXAMPLE",
		ToolCallsJSON: `[{"name":"shell","args":"export K=AKIAIOSFODNN7EXAMPLE"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := inner.entries[0]
	for field, val := range map[string]string{
		"user_message":    got.UserMessage,
		"response":        got.Response,
		"tool_calls_json": got.ToolCallsJSON,
	} {
		if strings.Contains(val, "AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("%s stored in plaintext: %q", field, val)
		}
		if !strings.Contains(val, "[REDACTED:") {
			t.Errorf("%s carries no redaction marker: %q", field, val)
		}
	}
	if len(audit.events) == 0 {
		t.Fatal("the row's redactions were not recorded in secret_redaction_audit")
	}
}

// Fields that never carry free text are untouched — a redaction pass that
// rewrote an id or a channel would break origin resolution.
func TestChatAudit_LeavesIdentityFieldsAlone(t *testing.T) {
	inner := &recordingChatRepo{}
	logger := zerolog.Nop()
	c := NewChatAudit(inner, detector(t), nil, &logger)

	in := &persistence.ChatAuditEntry{
		ID: "turn-2", ChatID: "telegram:42", UserID: "telegram:7",
		ProjectID: "p1", RoleUsed: "assistant", Model: "m",
		UserMessage: "hello",
	}
	if err := c.Insert(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got := inner.entries[0]
	if got.ID != "turn-2" || got.ChatID != "telegram:42" || got.UserID != "telegram:7" ||
		got.ProjectID != "p1" || got.RoleUsed != "assistant" || got.Model != "m" {
		t.Fatalf("identity fields altered: %+v", got)
	}
	if got.UserMessage != "hello" {
		t.Fatalf("clean text altered: %q", got.UserMessage)
	}
}

// A nil detector is a pass-through, so CE paths and tests that never wire
// secret scanning still persist chat audit.
func TestChatAudit_NilDetectorPassesThrough(t *testing.T) {
	inner := &recordingChatRepo{}
	c := NewChatAudit(inner, nil, nil, nil)

	raw := "key AKIAIOSFODNN7EXAMPLE"
	h, err := c.SavePrompt(context.Background(), raw)
	if err != nil || inner.prompts[h] != raw {
		t.Fatalf("pass-through altered the prompt: %q %v", inner.prompts[h], err)
	}
	if err := c.Insert(context.Background(), &persistence.ChatAuditEntry{ID: "t", UserMessage: raw}); err != nil {
		t.Fatal(err)
	}
	if inner.entries[0].UserMessage != raw {
		t.Fatalf("pass-through altered the row: %q", inner.entries[0].UserMessage)
	}
}

// failingChatRepo fails its writes so the decorator's error paths can be
// asserted.
type failingChatRepo struct{ recordingChatRepo }

func (r *failingChatRepo) SavePrompt(_ context.Context, body string) (string, error) {
	// Models the real repositories: the hash of the bytes it TRIED to store
	// comes back alongside the error.
	return persistence.HashChatSystemPrompt(body), errChatWriteFailed
}

var errChatWriteFailed = errors.New("chat store unavailable")

// The decorator must not mutate the caller's entry — the seam's own Repo.Log
// copies (auditredact.go:184) for the same reason: a decorator that edits its
// argument makes redaction visible at a distance, and a caller that retries or
// logs the entry afterwards sees a struct it did not write.
//
// Review finding (task_20260904104925_e1c9eb24c6a28f61, item 1).
func TestChatAudit_DoesNotMutateTheCallersEntry(t *testing.T) {
	inner := &recordingChatRepo{}
	logger := zerolog.Nop()
	c := NewChatAudit(inner, detector(t), nil, &logger)

	raw := "deploy with AKIAIOSFODNN7EXAMPLE please"
	entry := &persistence.ChatAuditEntry{ID: "turn-1", UserMessage: raw, Response: raw}
	require.NoError(t, c.Insert(context.Background(), entry))

	if entry.UserMessage != raw || entry.Response != raw {
		t.Fatalf("the caller's entry was mutated: %q / %q", entry.UserMessage, entry.Response)
	}
	stored := inner.entries[0]
	if strings.Contains(stored.UserMessage, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("the STORED row must be redacted: %q", stored.UserMessage)
	}
}

// A failed prompt write must still hand back the hash it tried to store, so
// the audit row keeps a pointer the operator can reconcile against: the same
// prompt saved successfully on a later turn lands under that exact hash.
// Returning "" would strand the row permanently even though the body is in the
// store (review finding item 2).
func TestChatAudit_ReturnsTheHashEvenWhenTheWriteFails(t *testing.T) {
	inner := &failingChatRepo{}
	logger := zerolog.Nop()
	c := NewChatAudit(inner, detector(t), nil, &logger)

	hash, err := c.SavePrompt(context.Background(), "You are the assistant.")
	if !errors.Is(err, errChatWriteFailed) {
		t.Fatalf("the write error must reach the caller, got %v", err)
	}
	if hash != persistence.HashChatSystemPrompt("You are the assistant.") {
		t.Fatalf("a failed save must still name the bytes it tried to store, got %q", hash)
	}
}
