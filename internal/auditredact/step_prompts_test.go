package auditredact

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

type recordingPromptRepo struct {
	saved map[string]string
}

func (r *recordingPromptRepo) Save(_ context.Context, _ persistence.StepPromptPart, body string) (string, error) {
	if r.saved == nil {
		r.saved = map[string]string{}
	}
	h := persistence.HashStepPrompt(body)
	r.saved[h] = body
	return h, nil
}
func (r *recordingPromptRepo) Get(_ context.Context, hash string) (*persistence.StepPrompt, error) {
	if b, ok := r.saved[hash]; ok {
		return &persistence.StepPrompt{Hash: hash, Body: b}, nil
	}
	return nil, persistence.ErrNotFound
}
func (r *recordingPromptRepo) PruneUnreferenced(context.Context) (int64, error) { return 0, nil }

func TestRecordingPromptRepo_KeepsTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "StepPromptRepository.Get", (&recordingPromptRepo{}).Get)
}

// A part carrying a credential is stored REDACTED, the hash is of the stored
// bytes, and the finding is recorded — the seam design's rule applied to the
// prompt store (step-prompt persistence design §5).
func TestStepPrompts_RedactsBeforeStoreAndHashesAfter(t *testing.T) {
	inner := &recordingPromptRepo{}
	audit := &recordingAudit{}
	logger := zerolog.Nop()
	s := NewStepPrompts(inner, detector(t), audit, &logger)

	raw := "Use this key: AKIAIOSFODNN7EXAMPLE and carry on."
	hash, err := s.Save(context.Background(), persistence.StepPromptSystem, raw)
	if err != nil {
		t.Fatal(err)
	}
	stored := inner.saved[hash]
	if strings.Contains(stored, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("credential stored in plaintext: %q", stored)
	}
	if !strings.Contains(stored, "[REDACTED:") {
		t.Fatalf("no redaction marker in the stored body: %q", stored)
	}
	if hash != persistence.HashStepPrompt(stored) {
		t.Fatal("the returned hash must be of the STORED (redacted) bytes")
	}
	if hash == persistence.HashStepPrompt(raw) {
		t.Fatal("the hash must not name the raw bytes")
	}
	if len(audit.events) == 0 {
		t.Fatal("the redaction was not recorded in secret_redaction_audit")
	}
	// Clean bodies pass through unchanged, hash of the same bytes.
	clean := "You are the planner."
	h2, err := s.Save(context.Background(), persistence.StepPromptUser, clean)
	if err != nil || h2 != persistence.HashStepPrompt(clean) || inner.saved[h2] != clean {
		t.Fatalf("clean body altered: %q %v", inner.saved[h2], err)
	}
}

// A nil detector is a pass-through — CE paths and tests that never wire
// secret scanning keep persisting prompts.
func TestStepPrompts_NilDetectorPassesThrough(t *testing.T) {
	inner := &recordingPromptRepo{}
	s := NewStepPrompts(inner, nil, nil, nil)
	raw := "key AKIAIOSFODNN7EXAMPLE"
	h, err := s.Save(context.Background(), persistence.StepPromptTools, raw)
	if err != nil || inner.saved[h] != raw {
		t.Fatalf("pass-through altered the body: %q %v", inner.saved[h], err)
	}
}
