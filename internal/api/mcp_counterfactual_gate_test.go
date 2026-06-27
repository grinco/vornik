package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// newStubClassifier builds a stubReplaySafetyClassifier (defined in
// blackbox_handlers_test.go) seeded with the given tool allow-list.
// Pass nil to get the empty (default) list — same semantics as
// newStubClassifier(nil) but without the
// internal/blackbox import.
func newStubClassifier(tools []string) *stubReplaySafetyClassifier {
	if tools == nil {
		tools = []string{}
	}
	return &stubReplaySafetyClassifier{tools: tools}
}

// gateTaskRepo builds a MockTaskRepository whose Get returns the
// supplied task (or ErrNotFound when absent). All other methods
// are left at the mock's zero-value defaults — the gate only
// calls Get.
func gateTaskRepo(tasks ...*persistence.Task) *mocks.MockTaskRepository {
	byID := map[string]*persistence.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	return &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			t, ok := byID[id]
			if !ok {
				return nil, persistence.ErrNotFound
			}
			return t, nil
		},
	}
}

// TestApplyCounterfactualMCPGate_NonCounterfactualPassthrough —
// task has no counterfactual block; gate falls through.
func TestApplyCounterfactualMCPGate_NonCounterfactualPassthrough(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID:      "t-1",
		Payload: json.RawMessage(`{"context":{"prompt":"hi"}}`),
	})
	s := NewServer(WithTaskRepository(repo))
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-1", "mcp__broker__place_order")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gate.HandledLocally {
		t.Errorf("non-counterfactual task should pass through; got %+v", gate)
	}
}

// TestApplyCounterfactualMCPGate_NoTaskIDPassthrough — empty task
// ID skips the lookup entirely.
func TestApplyCounterfactualMCPGate_NoTaskIDPassthrough(t *testing.T) {
	s := NewServer(WithTaskRepository(gateTaskRepo()))
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "", "any_tool")
	if err != nil || gate.HandledLocally {
		t.Errorf("empty task ID should pass through; got gate=%+v err=%v", gate, err)
	}
}

// TestApplyCounterfactualMCPGate_NoRepoPassthrough — no taskRepo
// wired (legacy deployment) → pass through.
func TestApplyCounterfactualMCPGate_NoRepoPassthrough(t *testing.T) {
	s := NewServer()
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-x", "any_tool")
	if err != nil || gate.HandledLocally {
		t.Errorf("missing taskRepo should pass through; got gate=%+v err=%v", gate, err)
	}
}

// TestApplyCounterfactualMCPGate_TaskNotFoundFailsClosed — a supplied
// task provenance header that cannot be resolved must never dispatch.
func TestApplyCounterfactualMCPGate_TaskNotFoundFailsClosed(t *testing.T) {
	s := NewServer(WithTaskRepository(gateTaskRepo()))
	_, err := s.applyCounterfactualMCPGate(context.Background(), "nonexistent", "any_tool")
	if err == nil {
		t.Fatal("missing task provenance must fail closed")
	}
}

// TestApplyCounterfactualMCPGate_RepoErrorBubbles — non-ErrNotFound
// errors bubble up; the handler logs + falls through but the
// gate surfaces the error for visibility.
func TestApplyCounterfactualMCPGate_RepoErrorBubbles(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, errors.New("postgres down")
		},
	}
	s := NewServer(WithTaskRepository(repo))
	_, err := s.applyCounterfactualMCPGate(context.Background(), "t-x", "any_tool")
	if err == nil {
		t.Error("expected non-nil error on repo failure")
	}
}

// TestApplyCounterfactualMCPGate_OperatorStubWins — replay task
// with tool_result override returns the stub even when the tool
// is NOT replay-safe (the operator deliberately chose this stub).
func TestApplyCounterfactualMCPGate_OperatorStubWins(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID: "t-cf",
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"tool_result_override":{"mcp__broker__place_order":"{\"status\":\"stubbed\"}"}
		}}}`),
	})
	classifier := newStubClassifier([]string{"mcp__broker__read_only_query"})
	s := NewServer(
		WithTaskRepository(repo),
		WithBlackBoxReplaySafety(classifier),
	)
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__broker__place_order")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gate.HandledLocally {
		t.Fatal("operator stub should short-circuit the call")
	}
	if gate.Text != `{"status":"stubbed"}` {
		t.Errorf("stub mismatch: got %q", gate.Text)
	}
}

// TestApplyCounterfactualMCPGate_NotReplaySafeBlock — replay task,
// no operator stub, the tool WAS in the original trace but is NOT on
// the replay-safe allow-list → synthesized "not_replay_safe" response
// (the deny-by-default branch).
func TestApplyCounterfactualMCPGate_NotReplaySafeBlock(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID: "t-cf",
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"model_override_all_roles":"opus",
			"original_tools":["mcp__broker__place_order"]
		}}}`),
	})
	// Allow-list deliberately EXCLUDES place_order — it's side-effecting.
	classifier := newStubClassifier([]string{"mcp__broker__read_only_query"})
	s := NewServer(
		WithTaskRepository(repo),
		WithBlackBoxReplaySafety(classifier),
	)
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__broker__place_order")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gate.HandledLocally {
		t.Fatal("non-replay-safe tool should be blocked on replay even when it was in the original")
	}
	if !strings.Contains(gate.Text, `"counterfactual_replay":true`) {
		t.Errorf("synthesized response should signal replay: %q", gate.Text)
	}
	if !strings.Contains(gate.Text, `"skipped":"not_replay_safe"`) {
		t.Errorf("synthesized response should mark not_replay_safe skip reason: %q", gate.Text)
	}
}

// TestApplyCounterfactualMCPGate_DefaultAllowListStubsQualifiedBroker
// is the production-path regression: the classifier is seeded with the
// REAL default allow-list (read-only bare names like broker_get_orders),
// not the qualified names the other gate tests hand-pick. A qualified
// broker ORDER (mcp__broker__place_order) that WAS in the original
// trace is NOT on the allow-list, so deny-by-default must stub it. This
// proves the production default never lets a live order through replay.
func TestApplyCounterfactualMCPGate_DefaultAllowListStubsQualifiedBroker(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID: "t-cf",
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"model_override_all_roles":"opus",
			"original_tools":["mcp__broker__place_order"]
		}}}`),
	})
	classifier := newStubClassifier(nil) // production default
	s := NewServer(
		WithTaskRepository(repo),
		WithBlackBoxReplaySafety(classifier),
	)
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__broker__place_order")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gate.HandledLocally {
		t.Fatal("qualified broker order must be stubbed under the default allow-list during replay")
	}
	if !strings.Contains(gate.Text, `"skipped":"not_replay_safe"`) {
		t.Errorf("expected not_replay_safe skip; got %q", gate.Text)
	}
}

// TestApplyCounterfactualMCPGate_ReplaySafePasses — replay task, tool
// IS on the replay-safe allow-list AND was in the original, no operator
// stub → passes through so the real MCP server fires (the swapped model
// sees real read-only data).
func TestApplyCounterfactualMCPGate_ReplaySafePasses(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID: "t-cf",
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"model_override_all_roles":"opus",
			"original_tools":["mcp__broker__read_only_query"]
		}}}`),
	})
	classifier := newStubClassifier([]string{"mcp__broker__read_only_query"})
	s := NewServer(
		WithTaskRepository(repo),
		WithBlackBoxReplaySafety(classifier),
	)
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__broker__read_only_query")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gate.HandledLocally {
		t.Errorf("replay-safe in-original tool should pass through; got %+v", gate)
	}
}

// TestApplyCounterfactualMCPGate_InOriginalButNotReplaySafeStubbed is
// the deny-by-default core: a tool the original DID call, with known
// provenance, is still stubbed when it's not on the allow-list. This is
// the inversion of the old behaviour (where an in-original
// non-deny-listed tool ran live).
func TestApplyCounterfactualMCPGate_InOriginalButNotReplaySafeStubbed(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID: "t-cf",
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"original_tools":["mcp__unknown__do_thing"]
		}}}`),
	})
	// Empty allow-list → nothing is replay-safe.
	classifier := newStubClassifier([]string{})
	s := NewServer(WithTaskRepository(repo), WithBlackBoxReplaySafety(classifier))
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__unknown__do_thing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gate.HandledLocally || !strings.Contains(gate.Text, `"skipped":"not_replay_safe"`) {
		t.Errorf("in-original-but-not-allow-listed tool must be stubbed not_replay_safe; got %+v", gate)
	}
}

// TestApplyCounterfactualMCPGate_NotInOriginalBlocked — replay
// task whose original_tools set is recorded; a tool the original
// never called is blocked with a synthesized "not_in_original"
// response (the LLD's "we can't fabricate a recorded output" rule).
func TestApplyCounterfactualMCPGate_NotInOriginalBlocked(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID: "t-cf",
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"model_override_all_roles":"opus",
			"original_tools":["mcp__fs__read_file"]
		}}}`),
	})
	classifier := newStubClassifier([]string{"mcp__broker__place_order"})
	s := NewServer(WithTaskRepository(repo), WithBlackBoxReplaySafety(classifier))

	// A brand-new, non-side-effecting tool never seen in the original.
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__net__fetch_url")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gate.HandledLocally {
		t.Fatal("tool not in original should be blocked on replay")
	}
	if !strings.Contains(gate.Text, `"skipped":"not_in_original"`) {
		t.Errorf("synthesized response should mark not_in_original: %q", gate.Text)
	}
}

// TestApplyCounterfactualMCPGate_InOriginalPasses — a tool that
// WAS called in the original passes through normally when it is
// also on the replay-safe allow-list.
func TestApplyCounterfactualMCPGate_InOriginalPasses(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID: "t-cf",
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"original_tools":["mcp__fs__read_file"]
		}}}`),
	})
	// Explicitly include the tool in the allow-list — the production
	// default contains "fs_read_file" which the concrete classifier
	// normalises to match "mcp__fs__read_file". The stub uses exact
	// match, so we pass the fully-qualified wire name directly.
	s := NewServer(WithTaskRepository(repo), WithBlackBoxReplaySafety(newStubClassifier([]string{"mcp__fs__read_file"})))
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__fs__read_file")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gate.HandledLocally {
		t.Errorf("tool present in original should pass through; got %+v", gate)
	}
}

// TestApplyCounterfactualMCPGate_OriginalToolsUnknownFailsClosed —
// without provenance, a newly named side-effecting tool could evade
// the deny-list, so every unstubbed tool is blocked.
func TestApplyCounterfactualMCPGate_OriginalToolsUnknownFailsClosed(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID:      "t-cf",
		Payload: json.RawMessage(`{"context":{"counterfactual":{"model_override_all_roles":"opus"}}}`),
	})
	s := NewServer(WithTaskRepository(repo), WithBlackBoxReplaySafety(newStubClassifier(nil)))
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__net__fetch_url")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gate.HandledLocally || !strings.Contains(gate.Text, `"skipped":"original_tools_unknown"`) {
		t.Errorf("unknown original set must fail closed; got %+v", gate)
	}
}

// TestApplyCounterfactualMCPGate_NoClassifier_AllowsAllTools — CE/Community nil-path:
// when no EE replay-safety classifier is wired, the gate allows all tools
// (replay-safety enforcement is an EE capability). Replaces the prior
// fail-closed behaviour (feat/ce-ee-phase1c-decoupling Task 2).
func TestApplyCounterfactualMCPGate_NoClassifier_AllowsAllTools(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID: "t-cf",
		// The task has original_tools recorded so we get past the
		// original_tools_unknown branch and hit the classifier check.
		Payload: json.RawMessage(`{"context":{"counterfactual":{"model_override_all_roles":"opus","original_tools":["mcp__broker__place_order"]}}}`),
	})
	s := NewServer(WithTaskRepository(repo)) // blackboxReplaySafety is nil
	result, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__broker__place_order")
	if err != nil {
		t.Fatalf("nil classifier must not error; got: %v", err)
	}
	if result.HandledLocally {
		t.Errorf("nil classifier must allow tool (CE nil-path); got HandledLocally=true, text=%q", result.Text)
	}
}

// TestApplyCounterfactualMCPGate_ZeroToolOriginalRecorded is the FIX 2
// regression on the gate side: a replay whose original called zero
// tools records original_tools as an empty array. The gate must treat
// it as KNOWN provenance (not original_tools_unknown) and apply the
// normal not-in-original / side-effect rules.
//
// Regression: review of a799e3f2 (2026-06-07) — "recorded-but-empty"
// was unrepresentable (OriginalToolsKnown was len(set)>0), so zero-tool
// replays blanket-blocked every tool as original_tools_unknown.
func TestApplyCounterfactualMCPGate_ZeroToolOriginalRecorded(t *testing.T) {
	repo := gateTaskRepo(&persistence.Task{
		ID: "t-cf",
		// original_tools present but EMPTY → recorded, zero tools.
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"model_override_all_roles":"opus",
			"original_tools":[]
		}}}`),
	})
	classifier := newStubClassifier([]string{"mcp__broker__place_order"})
	s := NewServer(WithTaskRepository(repo), WithBlackBoxReplaySafety(classifier))

	// A non-side-effecting tool the original never called must be
	// blocked as not_in_original — NOT original_tools_unknown.
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__net__fetch_url")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gate.HandledLocally {
		t.Fatal("zero-tool original should still block a tool it never called")
	}
	if strings.Contains(gate.Text, `"skipped":"original_tools_unknown"`) {
		t.Errorf("zero-tool original is KNOWN provenance, must not be original_tools_unknown; got %q", gate.Text)
	}
	if !strings.Contains(gate.Text, `"skipped":"not_in_original"`) {
		t.Errorf("expected not_in_original for a tool absent from the (empty) recorded set; got %q", gate.Text)
	}
}

// flakyTaskRepo serves task rows until flip() is called, after which
// every Get returns the supplied error — simulating a DB blip / a row
// archived out from under a still-running agent.
type flakyTaskRepo struct {
	*mocks.MockTaskRepository
}

func newFlakyTaskRepo(failAfter *bool, failErr error, tasks ...*persistence.Task) *flakyTaskRepo {
	byID := map[string]*persistence.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	return &flakyTaskRepo{
		MockTaskRepository: &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
				if *failAfter {
					return nil, failErr
				}
				t, ok := byID[id]
				if !ok {
					return nil, persistence.ErrNotFound
				}
				return t, nil
			},
		},
	}
}

// TestApplyCounterfactualMCPGate_NonReplayCachedSurvivesDBBlip is the
// FIX 1 blast-radius regression.
//
// Regression: review of a799e3f2 (2026-06-07) — the gate ran
// taskRepo.Get before the IsReplay short-circuit and the handler 503'd
// on any gate error, so a transient DB error blocked ALL agent MCP
// dispatch (every call carries a task ID), not just replays. After the
// fix a non-replay task resolved once is served from cache on a later
// Get error and passes through.
func TestApplyCounterfactualMCPGate_NonReplayCachedSurvivesDBBlip(t *testing.T) {
	fail := false
	repo := newFlakyTaskRepo(&fail, errors.New("postgres down"), &persistence.Task{
		ID:      "t-plain",
		Payload: json.RawMessage(`{"context":{"prompt":"hi"}}`),
	})
	s := NewServer(WithTaskRepository(repo), WithBlackBoxReplaySafety(newStubClassifier(nil)))

	// First call resolves + caches the (non-replay) snapshot.
	if gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-plain", "mcp__broker__place_order"); err != nil || gate.HandledLocally {
		t.Fatalf("first call should pass through cleanly; gate=%+v err=%v", gate, err)
	}
	// DB goes down; the cached non-replay snapshot must let the call through.
	fail = true
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-plain", "mcp__broker__place_order")
	if err != nil {
		t.Fatalf("cached non-replay task must not error during a DB blip; got %v", err)
	}
	if gate.HandledLocally {
		t.Errorf("cached non-replay task must pass through during a DB blip; got %+v", gate)
	}
}

// TestApplyCounterfactualMCPGate_UncachedDBErrorFailsClosed — an
// uncached task we have never resolved still fails closed on a Get
// error (the caller 503s).
//
// Regression: review of a799e3f2 (2026-06-07) — fail-closed posture
// for replays must survive the FIX 1 cache fallback when no snapshot
// exists.
func TestApplyCounterfactualMCPGate_UncachedDBErrorFailsClosed(t *testing.T) {
	fail := true
	repo := newFlakyTaskRepo(&fail, errors.New("postgres down"))
	s := NewServer(WithTaskRepository(repo), WithBlackBoxReplaySafety(newStubClassifier(nil)))
	if _, err := s.applyCounterfactualMCPGate(context.Background(), "never-seen", "mcp__broker__place_order"); err == nil {
		t.Fatal("uncached task with a Get error must fail closed")
	}
}

// TestApplyCounterfactualMCPGate_CachedReplayStillEnforcedOnDBError —
// a replay task resolved once must keep enforcing from the cached
// snapshot when a later Get fails (DB blip or archived row).
//
// Regression: review of a799e3f2 (2026-06-07) — the cache fallback
// must NOT weaken replay enforcement; cached replays still stub
// non-replay-safe tools during a DB outage.
func TestApplyCounterfactualMCPGate_CachedReplayStillEnforcedOnDBError(t *testing.T) {
	fail := false
	repo := newFlakyTaskRepo(&fail, errors.New("postgres down"), &persistence.Task{
		ID: "t-cf",
		// Original called both a read-only query and a live order.
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"original_tools":["mcp__broker__read_only_query","mcp__broker__place_order"]
		}}}`),
	})
	// Allow-list contains only the read-only query — place_order is not
	// replay-safe.
	classifier := newStubClassifier([]string{"mcp__broker__read_only_query"})
	s := NewServer(WithTaskRepository(repo), WithBlackBoxReplaySafety(classifier))

	// Prime the cache with a successful resolution — the replay-safe
	// read query passes through cleanly, proving the snapshot cached.
	if gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__broker__read_only_query"); err != nil || gate.HandledLocally {
		t.Fatalf("priming call should pass through cleanly; gate=%+v err=%v", gate, err)
	}
	// DB blip: a non-replay-safe tool on the cached replay must STILL
	// be short-circuited, not allowed through.
	fail = true
	gate, err := s.applyCounterfactualMCPGate(context.Background(), "t-cf", "mcp__broker__place_order")
	if err != nil {
		t.Fatalf("cached replay enforcement should not error: %v", err)
	}
	if !gate.HandledLocally || !strings.Contains(gate.Text, `"skipped":"not_replay_safe"`) {
		t.Errorf("cached replay must still stub non-replay-safe tools during a DB blip; got %+v", gate)
	}
}
