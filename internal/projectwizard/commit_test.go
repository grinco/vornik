package projectwizard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type capturingWriter struct {
	calls []captureCall
	err   error
	url   string
}

type captureCall struct {
	projectID string
	yaml      string
}

func (c *capturingWriter) Write(_ context.Context, projectID string, yaml []byte) (string, error) {
	c.calls = append(c.calls, captureCall{projectID: projectID, yaml: string(yaml)})
	if c.err != nil {
		return "", c.err
	}
	url := c.url
	if url == "" {
		url = "/ui/projects/" + projectID
	}
	return url, nil
}

// pinAssistantTurn pre-populates a session so Commit has something
// to work with. Avoids having to thread a full Converse flow
// through every commit test.
func pinReadySession(t *testing.T, store *fakeSessionStore, operatorID string, proposal map[string]any) string {
	t.Helper()
	session := &persistence.ProjectWizardSession{
		ID:            persistence.GenerateID("pw"),
		OperatorID:    operatorID,
		ReadyToCommit: true,
	}
	if proposal != nil {
		proposalBytes, err := json.Marshal(struct {
			Raw map[string]any `json:"raw"`
		}{Raw: proposal})
		if err != nil {
			t.Fatalf("marshal proposal: %v", err)
		}
		session.CurrentProposal = proposalBytes
	}
	if err := store.Insert(context.Background(), session); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return session.ID
}

// minimalValidProposal is the smallest project YAML the Phase B
// RegistryValidator accepts: projectId + swarmId + defaultWorkflowId.
func minimalValidProposal() map[string]any {
	return map[string]any{
		"projectId":         "test-project",
		"displayName":       "Test Project",
		"swarmId":           "test-swarm",
		"defaultWorkflowId": "test-workflow",
	}
}

func TestCommit_HappyPath(t *testing.T) {
	w, store, _ := newWizardForTest()
	writer := &capturingWriter{}
	w.Writer = writer
	w.Validator = RegistryValidator{}

	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	result, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if result.ProjectID != "test-project" {
		t.Errorf("project id wrong: %q", result.ProjectID)
	}
	if result.URL != "/ui/projects/test-project" {
		t.Errorf("url wrong: %q", result.URL)
	}
	if len(writer.calls) != 1 {
		t.Fatalf("expected 1 write call, got %d", len(writer.calls))
	}
	if writer.calls[0].projectID != "test-project" {
		t.Errorf("write project id wrong: %q", writer.calls[0].projectID)
	}
	if !strings.Contains(writer.calls[0].yaml, "projectId: test-project") {
		t.Errorf("yaml missing projectId: %s", writer.calls[0].yaml)
	}
	// Session should be terminal.
	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CommittedProjectID == nil || *stored.CommittedProjectID != "test-project" {
		t.Errorf("session not stamped: %+v", stored.CommittedProjectID)
	}
}

func TestCommit_NotReadyRejected(t *testing.T) {
	w, store, _ := newWizardForTest()
	w.Writer = &capturingWriter{}
	session := &persistence.ProjectWizardSession{
		ID:         persistence.GenerateID("pw"),
		OperatorID: "op_1",
	}
	proposalBytes, _ := json.Marshal(struct {
		Raw map[string]any `json:"raw"`
	}{Raw: minimalValidProposal()})
	session.CurrentProposal = proposalBytes
	session.ReadyToCommit = false
	_ = store.Insert(context.Background(), session)

	_, err := w.Commit(context.Background(), session.ID, "op_1")
	if !errors.Is(err, ErrNotReadyToCommit) {
		t.Fatalf("expected ErrNotReadyToCommit, got %v", err)
	}
}

func TestCommit_NoProposalRejected(t *testing.T) {
	w, store, _ := newWizardForTest()
	w.Writer = &capturingWriter{}
	session := &persistence.ProjectWizardSession{
		ID:            persistence.GenerateID("pw"),
		OperatorID:    "op_1",
		ReadyToCommit: true,
	}
	_ = store.Insert(context.Background(), session)
	_, err := w.Commit(context.Background(), session.ID, "op_1")
	if !errors.Is(err, ErrNoProposal) {
		t.Fatalf("expected ErrNoProposal, got %v", err)
	}
}

func TestCommit_AlreadyCommittedIsIdempotent(t *testing.T) {
	w, store, _ := newWizardForTest()
	w.Writer = &capturingWriter{}
	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	// Pre-commit the session.
	committed := "previously-committed"
	stored, _ := store.Get(context.Background(), sessionID)
	stored.CommittedProjectID = &committed
	_ = store.Update(context.Background(), stored)

	result, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("idempotent commit should succeed: %v", err)
	}
	if result.ProjectID != committed {
		t.Errorf("expected prior project id, got %q", result.ProjectID)
	}
	if result.URL != "/ui/projects/"+committed {
		t.Errorf("expected redirect to existing project, got %q", result.URL)
	}
}

func TestCommit_CrossOperatorReturnsNotFound(t *testing.T) {
	w, store, _ := newWizardForTest()
	w.Writer = &capturingWriter{}
	w.Validator = RegistryValidator{}
	sessionID := pinReadySession(t, store, "op_owner", minimalValidProposal())
	_, err := w.Commit(context.Background(), sessionID, "op_attacker")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("expected ErrNotFound for cross-operator commit, got %v", err)
	}
}

func TestCommit_WriterFailureLeavesSessionOpen(t *testing.T) {
	w, store, _ := newWizardForTest()
	w.Writer = &capturingWriter{err: errors.New("disk full")}
	w.Validator = RegistryValidator{}
	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected writer error to propagate")
	}
	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CommittedProjectID != nil {
		t.Error("session should NOT be stamped when write fails")
	}
}

func TestCommit_ValidationFailurePreventsWrite(t *testing.T) {
	w, store, _ := newWizardForTest()
	writer := &capturingWriter{}
	w.Writer = writer
	w.Validator = RegistryValidator{}
	// Proposal missing swarmId — RegistryValidator refuses.
	bad := map[string]any{
		"projectId":         "test",
		"displayName":       "Test",
		"defaultWorkflowId": "test-workflow",
	}
	sessionID := pinReadySession(t, store, "op_1", bad)
	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected validation error")
	}
	if len(writer.calls) > 0 {
		t.Error("writer should not be called when validation fails")
	}
}

func TestCommit_InvalidProjectIDPreventsWrite(t *testing.T) {
	w, store, _ := newWizardForTest()
	writer := &capturingWriter{}
	w.Writer = writer
	w.Validator = permissiveValidator{}
	bad := minimalValidProposal()
	bad["projectId"] = "../escape"
	sessionID := pinReadySession(t, store, "op_1", bad)
	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil || !strings.Contains(err.Error(), "invalid projectId") {
		t.Fatalf("expected invalid projectId error, got %v", err)
	}
	if len(writer.calls) > 0 {
		t.Error("writer should not be called with unsafe project id")
	}
}

func TestCommit_UnwiredWriterReturnsSentinel(t *testing.T) {
	w, store, _ := newWizardForTest()
	w.Writer = nil
	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if !errors.Is(err, ErrWriterUnwired) {
		t.Errorf("expected ErrWriterUnwired, got %v", err)
	}
}

func TestRegistryValidator_AcceptsMinimal(t *testing.T) {
	v := RegistryValidator{}
	p := &ProjectYAML{Raw: minimalValidProposal()}
	if err := v.Validate(p); err != nil {
		t.Errorf("minimal proposal should validate, got %v", err)
	}
}

func TestRegistryValidator_RejectsMissingFields(t *testing.T) {
	v := RegistryValidator{}
	cases := []struct {
		name string
		raw  map[string]any
	}{
		{"empty", nil},
		{"missing projectId", map[string]any{"swarmId": "s", "defaultWorkflowId": "w"}},
		{"missing swarmId", map[string]any{"projectId": "p", "defaultWorkflowId": "w"}},
		{"missing defaultWorkflowId", map[string]any{"projectId": "p", "swarmId": "s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := v.Validate(&ProjectYAML{Raw: c.raw})
			if err == nil {
				t.Errorf("expected validation failure for %s", c.name)
			}
		})
	}
}

func TestProposalProjectID(t *testing.T) {
	if got := ProposalProjectID(nil); got != "" {
		t.Errorf("nil proposal: got %q", got)
	}
	if got := ProposalProjectID(&ProjectYAML{Raw: map[string]any{}}); got != "" {
		t.Errorf("empty raw: got %q", got)
	}
	if got := ProposalProjectID(&ProjectYAML{Raw: map[string]any{"projectId": "x"}}); got != "x" {
		t.Errorf("expected x, got %q", got)
	}
	if got := ProposalProjectID(&ProjectYAML{Raw: map[string]any{"projectId": 42}}); got != "" {
		t.Errorf("non-string projectId should yield empty, got %q", got)
	}
}
