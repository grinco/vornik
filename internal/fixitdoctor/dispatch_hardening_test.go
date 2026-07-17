package fixitdoctor

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestDispatch_RecordFailureDoesNotFailApply is the fix for review-20260716-d95b:
// an APPLIED action whose applied-actions record fails to persist must NOT return
// a Dispatch error (the UI renders that as "Apply failed" for a fix that actually
// ran). The action's real result is returned, with a note that rollback-tracking
// may be incomplete.
func TestDispatch_RecordFailureDoesNotFailApply(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindConfigApplyGate, Label: "turn on instinct",
		Params: map[string]string{"key": "instinct.enabled"}}}
	svc, store, sid := newDispatchTestService(t, testRef, actions)
	svc.GatePipeline = &fakeGatePipeline{detail: "instinct.enabled -> true", diff: "- false\n+ true"}
	store.updateErr = context.DeadlineExceeded // make recordAppliedAction fail

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch must not error when only the record write fails, got: %v", err)
	}
	if res == nil || res.Result != ActionResultApplied {
		t.Fatalf("result = %+v, want an applied result", res)
	}
	if !strings.Contains(res.Detail, "recording it for rollback failed") {
		t.Fatalf("detail should note the record failure, got %q", res.Detail)
	}
}

// TestDispatch_ConcurrentAppliesDoNotLostUpdate exercises the dispatchMu guard:
// N concurrent Dispatch calls on the same session must each land a record (no
// read-append-write lost update).
func TestDispatch_ConcurrentAppliesDoNotLostUpdate(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindConfigApplyGate, Label: "on",
		Params: map[string]string{"key": "instinct.enabled"}}}
	svc, store, sid := newDispatchTestService(t, testRef, actions)
	svc.GatePipeline = &fakeGatePipeline{detail: "ok", diff: "d"}

	const n = 6
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := svc.Dispatch(context.Background(), sid, "op-1", 0, ""); err != nil {
				t.Errorf("Dispatch: %v", err)
			}
		}()
	}
	wg.Wait()

	row, _ := store.Get(context.Background(), sid)
	var records []appliedActionRecord
	if err := json.Unmarshal(row.AppliedActions, &records); err != nil {
		t.Fatalf("decode applied_actions: %v", err)
	}
	if len(records) != n {
		t.Fatalf("applied_actions = %d, want %d (lost update)", len(records), n)
	}
}

func TestValidateFailureRef(t *testing.T) {
	ok := []FailureRef{
		{Kind: FailureKindFailedTask, ID: "task_20260717_abc", ProjectID: "janka"},
		{Kind: FailureKindDegradedFeature, ID: "composer.enabled"}, // dotted key, no project
		{Kind: FailureKindRedIntegration, ID: "slack:workspace-1", ProjectID: "p1"},
	}
	for _, ref := range ok {
		if err := validateFailureRef(ref); err != nil {
			t.Errorf("validateFailureRef(%+v) = %v, want nil", ref, err)
		}
	}
	bad := map[string]FailureRef{
		"empty id":       {Kind: FailureKindFailedTask, ID: ""},
		"empty kind":     {Kind: "", ID: "x"},
		"unknown kind":   {Kind: FailureKind("shell_exec"), ID: "x"},
		"newline in id":  {Kind: FailureKindFailedTask, ID: "task\n1"},
		"space in id":    {Kind: FailureKindFailedTask, ID: "task 1"},
		"control in id":  {Kind: FailureKindFailedTask, ID: "task\x00"},
		"oversized id":   {Kind: FailureKindFailedTask, ID: strings.Repeat("a", maxFailureRefFieldLen+1)},
		"bad project id": {Kind: FailureKindFailedTask, ID: "ok", ProjectID: "proj\ttab"},
	}
	for name, ref := range bad {
		if err := validateFailureRef(ref); err == nil {
			t.Errorf("%s: validateFailureRef(%+v) = nil, want error", name, ref)
		}
	}
}

// TestConverse_BudgetBlockedResume_LeavesSessionOpen is the review-20260717-c377
// Finding-1 regression: the relocated budget gate now runs on the RESUME path,
// but a budget-blocked resume must NOT close the (durable, possibly mid-repair)
// session — the orphan-cleanup defer is gated on createdNew, so only a
// freshly-created session is closed on error.
func TestConverse_BudgetBlockedResume_LeavesSessionOpen(t *testing.T) {
	tasks := failedTaskWith(persistence.TaskStatusFailed)
	svc, store, _ := newTestService(t, tasks) // budget blocks before any LLM turn
	svc.BudgetRepo = &fakeBudgetRepo{sum: 999}
	svc.Projects = &fakeProjectLookup{projects: map[string]*registry.Project{
		"proj-1": {ID: "proj-1", Budget: registry.ProjectBudget{DailyHardUSD: 1}},
	}}
	sess := &persistence.FixItSession{
		ID: persistence.GenerateID("fix"), OperatorID: "op1",
		FailureKind: string(FailureKindFailedTask), FailureRefID: "t1", ProjectID: "proj-1",
	}
	if err := store.Insert(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	_, err := svc.Converse(context.Background(), sess.ID, "op1",
		FailureRef{Kind: FailureKindFailedTask, ID: "t1", ProjectID: "proj-1"}, "help")
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("expected budget-exceeded error on resume, got %v", err)
	}
	row, gerr := store.Get(context.Background(), sess.ID)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if row.ClosedAt != nil {
		t.Fatalf("budget-blocked RESUME must leave the session open, got ClosedAt=%v", row.ClosedAt)
	}
}

// FuzzParseEnvelope: the LLM-output parser must never panic on adversarial input
// (deep nesting, invalid UTF-8, duplicate keys, prose) — audit §13.2 / review #14.
func FuzzParseEnvelope(f *testing.F) {
	f.Add(`{"message":"hi","resolved":false}`)
	f.Add("```json\n{\"message\":\"x\"}\n```")
	f.Add("just prose, no json")
	f.Add(`{"message":{"message":{"message":"deep"}}}`)
	f.Add("{\"message\":\"\xff\xfe bad utf8\"}")
	f.Fuzz(func(_ *testing.T, s string) {
		_, _ = ParseEnvelope(s) // must not panic
	})
}

// FuzzValidLinkOutURL: the link_out URL validator must never panic on arbitrary
// input (it gates an operator-facing action param).
func FuzzValidLinkOutURL(f *testing.F) {
	f.Add("https://example.com")
	f.Add("javascript:alert(1)")
	f.Add("://noscheme")
	f.Add("http://\x00")
	f.Fuzz(func(_ *testing.T, s string) {
		_ = validLinkOutURL(s) // must not panic
	})
}
