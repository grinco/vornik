package dispatcher

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// TestBuildOperatorProfileBlock_ReturnsKeysAndNotesFlag pins the
// audit-aware return shape. Tests of the prompt content itself
// live in operator_profile_test.go; here we cover the second
// + third return values that drive the Phase-B audit row.
func TestBuildOperatorProfileBlock_ReturnsKeysAndNotesFlag(t *testing.T) {
	prompt, keys, notes := buildOperatorProfileBlock("base", &persistence.OperatorProfile{
		Structured: []byte(`{"tone":"terse","verbosity":"low"}`),
		Notes:      "prefers metric units",
	})
	if !strings.Contains(prompt, "<operator_profile>") {
		t.Errorf("expected block in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "from your profile") {
		t.Errorf("expected citation footer instruction in prompt:\n%s", prompt)
	}
	if !notes {
		t.Errorf("expected notes flag to be true")
	}
	want := map[string]bool{"tone": true, "verbosity": true}
	got := map[string]bool{}
	for _, k := range keys {
		got[k] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("expected key %q in usedKeys, got %v", k, keys)
		}
	}
}

func TestBuildOperatorProfileBlock_NilReturnsZero(t *testing.T) {
	prompt, keys, notes := buildOperatorProfileBlock("base", nil)
	if prompt != "base" {
		t.Errorf("nil profile changed prompt")
	}
	if len(keys) != 0 || notes {
		t.Errorf("nil profile should not report usage")
	}
}

func TestBuildOperatorProfileBlock_EmptyProfileReturnsZero(t *testing.T) {
	prompt, keys, notes := buildOperatorProfileBlock("base", &persistence.OperatorProfile{
		Structured: []byte("{}"),
	})
	if prompt != "base" {
		t.Errorf("empty profile changed prompt")
	}
	if len(keys) != 0 || notes {
		t.Errorf("empty profile should not report usage")
	}
}

func TestBuildOperatorProfileBlock_NotesOnlyStillFlagsNotes(t *testing.T) {
	prompt, keys, notes := buildOperatorProfileBlock("base", &persistence.OperatorProfile{
		Notes: "prefers Czech UI",
	})
	if !strings.Contains(prompt, "<operator_profile>") {
		t.Errorf("notes-only block should still render")
	}
	if len(keys) != 0 {
		t.Errorf("notes-only profile must produce no structured-key usage: %v", keys)
	}
	if !notes {
		t.Errorf("notes flag should be true for notes-only profile")
	}
}

func TestOperatorProfileCitationFooter_PresentWhenBlockEmitted(t *testing.T) {
	prompt, _, _ := buildOperatorProfileBlock("sys", &persistence.OperatorProfile{
		Structured: []byte(`{"tone":"terse"}`),
	})
	if !strings.Contains(prompt, "[from your profile:") {
		t.Errorf("citation footer should include the marker syntax:\n%s", prompt)
	}
	if !strings.Contains(prompt, "[overriding profile:") {
		t.Errorf("override pattern should be present:\n%s", prompt)
	}
}

// auditCapture is a tiny stub implementing
// persistence.ProfileUseAuditRepository so the audit-write
// integration test doesn't need a DB.
type auditCapture struct {
	mu   sync.Mutex
	rows []*persistence.ProfileUseAudit
	err  error
}

func (c *auditCapture) Insert(_ context.Context, row *persistence.ProfileUseAudit) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.rows = append(c.rows, row)
	return nil
}
func (c *auditCapture) ListForOperator(_ context.Context, _ string, _ persistence.ProfileUseAuditQuery) ([]*persistence.ProfileUseAudit, error) {
	return nil, nil
}
func (c *auditCapture) DeleteAllForOperator(_ context.Context, _ string) error { return nil }

// stubProfileRepo returns a fixed profile.
type stubProfileRepo struct {
	profile *persistence.OperatorProfile
	err     error
}

func (s *stubProfileRepo) Get(_ context.Context, id string) (*persistence.OperatorProfile, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.profile == nil {
		return nil, persistence.ErrNotFound
	}
	p := *s.profile
	p.OperatorID = id
	return &p, nil
}
func (s *stubProfileRepo) Upsert(_ context.Context, _ *persistence.OperatorProfile) error { return nil }
func (s *stubProfileRepo) Delete(_ context.Context, _ string) error                       { return nil }
func (s *stubProfileRepo) List(_ context.Context, _ int) ([]*persistence.OperatorProfile, error) {
	return nil, nil
}

func TestMaybeInjectOperatorProfile_WritesAuditRow(t *testing.T) {
	audit := &auditCapture{}
	a := &Agent{
		logger: zerolog.Nop(),
		operatorProfiles: &stubProfileRepo{profile: &persistence.OperatorProfile{
			Structured: []byte(`{"tone":"terse"}`),
			Notes:      "hi",
		}},
		profileUseAudit: audit,
	}
	out := a.maybeInjectOperatorProfile(context.Background(), "sys", "tg:42")
	if !strings.Contains(out, "<operator_profile>") {
		t.Fatalf("expected block in output:\n%s", out)
	}
	// Audit write runs in a goroutine; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		audit.mu.Lock()
		n := len(audit.rows)
		audit.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.rows))
	}
	row := audit.rows[0]
	if row.OperatorID != "tg:42" {
		t.Errorf("operator id: %q", row.OperatorID)
	}
	if len(row.UsedKeys) == 0 || row.UsedKeys[0] != "tone" {
		t.Errorf("used keys: %v", row.UsedKeys)
	}
	if !row.UsedNotes {
		t.Errorf("notes flag should be true")
	}
}

func TestMaybeInjectOperatorProfile_NoAuditWhenBlockSuppressed(t *testing.T) {
	audit := &auditCapture{}
	a := &Agent{
		logger:           zerolog.Nop(),
		operatorProfiles: &stubProfileRepo{profile: &persistence.OperatorProfile{Structured: []byte("{}")}},
		profileUseAudit:  audit,
	}
	a.maybeInjectOperatorProfile(context.Background(), "sys", "tg:42")
	// Give the (would-be) goroutine a moment to run + not write.
	time.Sleep(50 * time.Millisecond)
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.rows) != 0 {
		t.Errorf("empty profile must not write audit rows; got %d", len(audit.rows))
	}
}

func TestMaybeInjectOperatorProfile_AuditFailureDoesNotBreakTurn(t *testing.T) {
	audit := &auditCapture{err: context.Canceled}
	a := &Agent{
		logger:           zerolog.Nop(),
		operatorProfiles: &stubProfileRepo{profile: &persistence.OperatorProfile{Structured: []byte(`{"tone":"terse"}`)}},
		profileUseAudit:  audit,
	}
	// Should NOT panic + must still produce the block.
	out := a.maybeInjectOperatorProfile(context.Background(), "sys", "tg:42")
	if !strings.Contains(out, "<operator_profile>") {
		t.Errorf("block missing even when audit fails:\n%s", out)
	}
}
