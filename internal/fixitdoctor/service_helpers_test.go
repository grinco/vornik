package fixitdoctor

import (
	"context"
	"errors"
	"testing"
	"time"
	"vornik.io/vornik/internal/llmspend"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func TestDecodeTranscript_EmptyAndMalformed(t *testing.T) {
	out, err := decodeTranscript(nil)
	if err != nil || out != nil {
		t.Fatalf("expected (nil, nil) for empty input, got (%v, %v)", out, err)
	}
	if _, err := decodeTranscript([]byte("not json")); err == nil {
		t.Fatalf("expected an error decoding malformed transcript bytes")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Fatalf("expected first non-empty to prefer a, got %q", got)
	}
	if got := firstNonEmpty("", "b"); got != "b" {
		t.Fatalf("expected fallback to b, got %q", got)
	}
}

type modelOverridableFake struct {
	fakeSvcChatProvider
	lastModel string
}

func (m *modelOverridableFake) WithModel(model string) chat.Provider {
	m.lastModel = model
	return m
}

func TestPickModel(t *testing.T) {
	base := &fakeSvcChatProvider{}
	if got := pickModel(base, ""); got != base {
		t.Fatalf("empty model must return the client unchanged")
	}
	overridable := &modelOverridableFake{}
	got := pickModel(overridable, "gpt-x")
	if overridable.lastModel != "gpt-x" {
		t.Fatalf("expected WithModel called with gpt-x, got %q", overridable.lastModel)
	}
	if got != overridable {
		t.Fatalf("expected the overridden client back")
	}
}

func TestRecordUsage_NilAndZeroUsageAreNoOps(t *testing.T) {
	usage := &fakeUsageRecorder{}
	svc := &Service{Spend: llmspend.New(usage, nil, SourceFixItDoctor, RoleFixItDoctor)}
	svc.recordUsage(context.Background(), nil, "sess-1")
	if len(usage.rows) != 0 {
		t.Fatalf("expected no usage row for a nil response")
	}
	svc.recordUsage(context.Background(), &chat.ChatResponse{}, "sess-1")
	if len(usage.rows) != 0 {
		t.Fatalf("expected no usage row when prompt/completion tokens are both zero")
	}
}

func TestBudgetBlocked_SkipsWhenUnwiredOrNoProjectID(t *testing.T) {
	svc := &Service{}
	if blocked, _ := svc.budgetBlocked(context.Background(), "proj-1"); blocked {
		t.Fatalf("expected no block with nothing wired")
	}
	svc.BudgetRepo = &fakeBudgetRepo{sum: 999}
	svc.Projects = &fakeProjectLookup{projects: map[string]*registry.Project{}}
	if blocked, _ := svc.budgetBlocked(context.Background(), ""); blocked {
		t.Fatalf("expected no block with empty project id")
	}
	if blocked, _ := svc.budgetBlocked(context.Background(), "unknown-proj"); blocked {
		t.Fatalf("expected no block when the project doesn't resolve")
	}
}

func TestBudgetBlocked_ErrorFromCheckDoesNotBlock(t *testing.T) {
	svc := &Service{
		BudgetRepo: &fakeBudgetRepo{err: errors.New("db down")},
		Projects:   &fakeProjectLookup{projects: map[string]*registry.Project{"p1": {ID: "p1", Budget: registry.ProjectBudget{DailyHardUSD: 1}}}},
	}
	blocked, reason := svc.budgetBlocked(context.Background(), "p1")
	if blocked || reason != "" {
		t.Fatalf("a budget.Check error must fail open (no block), got blocked=%v reason=%q", blocked, reason)
	}
}

func TestObjectGone_NonFailedTaskKindsNeverCascade(t *testing.T) {
	svc := &Service{Assembler: &Assembler{Tasks: &fakeTaskRepo{tasks: map[string]*persistence.Task{}}}}
	if svc.objectGone(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "x"}) {
		t.Fatalf("degraded_feature has no delete concept and must never cascade-close")
	}
	if svc.objectGone(context.Background(), FailureRef{Kind: FailureKindRedIntegration, ID: "x"}) {
		t.Fatalf("red_integration must never cascade-close")
	}
	if svc.objectGone(context.Background(), FailureRef{Kind: FailureKindFailedReload}) {
		t.Fatalf("failed_reload must never cascade-close")
	}
}

func TestObjectGone_NilAssemblerOrTasksIsSafe(t *testing.T) {
	svc := &Service{}
	if svc.objectGone(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "x"}) {
		t.Fatalf("nil Assembler must not report the object gone")
	}
	svc = &Service{Assembler: &Assembler{}}
	if svc.objectGone(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "x"}) {
		t.Fatalf("nil Tasks repo must not report the object gone")
	}
}

func TestConverse_ListByOperatorErrorDoesNotBlockNewSession(t *testing.T) {
	tasks := failedTaskWith(persistence.TaskStatusFailed)
	svc, store, _ := newTestService(t, tasks, svcChatReply{content: envOK})
	// Swap in a store whose ListByOperator errors — the cap check is
	// best-effort (mirrors projectwizard's `if err == nil` guard) so a
	// transient list failure must not block a brand-new session.
	broken := &erroringListStore{fakeFixItSessionStore: store}
	svc.Sessions = broken
	if _, err := svc.Converse(context.Background(), "", "op1", FailureRef{Kind: FailureKindFailedTask, ID: "t1", ProjectID: "proj-1"}, "help"); err != nil {
		t.Fatalf("expected the session cap check to fail open on a list error, got %v", err)
	}
}

type erroringListStore struct {
	*fakeFixItSessionStore
}

func (e *erroringListStore) ListByOperator(context.Context, string, int) ([]*persistence.FixItSession, error) {
	return nil, errors.New("list failed")
}

func TestConverse_GetSessionErrorPropagates(t *testing.T) {
	svc := &Service{Sessions: &erroringGetStore{}, Assembler: &Assembler{}, Chat: &fakeSvcChatProvider{}, Timeout: time.Second}
	if _, err := svc.Converse(context.Background(), "some-id", "op1", FailureRef{}, "hi"); err == nil {
		t.Fatalf("expected the session load error to propagate")
	}
}

type erroringGetStore struct{ fakeFixItSessionStore }

func (e *erroringGetStore) Get(context.Context, string) (*persistence.FixItSession, error) {
	return nil, errors.New("db down")
}
