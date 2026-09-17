package configassist

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// scriptCreate scripts a file_write that CREATES a path absent from the
// snapshot — the shape the new-file classification path handles.
func (f *fixture) scriptCreate(file, content string) {
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["`+file+`"]`, call("c1", "file_write", map[string]any{"path": file, "content": content})),
		textResp("glm-5.2:cloud", "Created "+file+" as asked."),
	}
}

// Regression: audit 2026-09-15 CA-18 — "New Files Are Classified Using Their
// Human Explanation". For a created file the engine synthesised a Change
// whose After was ClassifyFileEvent(...).Reason — an English sentence — and
// then classified THAT as if it were a config value. "new project prose
// (wrapped as data by the daemon)" looks like prose, so the synthetic touch
// came back B1 and outranked the real B2 event appended moments later. A
// legitimate request to create descriptive context was refused as steering
// prose at the chat/agent ceiling.
func TestPropose_NewProseFileClassifiedByLocationNotByItsExplanation(t *testing.T) {
	f := newFixture(t)
	f.scriptCreate("projects/assistant/README.md", "# Assistant\n\nThis project runs the news digest.\n")
	f.scriptJudge("pass", "# Assistant")
	res, err := f.engine.Propose(context.Background(), operatorReq("write a README describing this project"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		t.Fatalf("unexpected refusal: %+v", res.Refusal)
	}
	if res.Class != ClassB2 {
		t.Fatalf("a new project .md is descriptive prose (B2), got %s: %+v", res.Class, res.Touches)
	}
	for _, tch := range res.Touches {
		if tch.Key == "<new file>" {
			t.Fatalf("the synthetic <new file> change must not reach the classifier: %+v", tch)
		}
		if strings.Contains(tch.Reason, "unclassified prose is B1") {
			t.Fatalf("a file EVENT was classified as a config value: %+v", tch)
		}
	}
}

// Regression: audit 2026-09-15 CA-16 — "Idempotency Is Global and Does Not
// Bind the Request". A matching key short-circuited before the project,
// actor and intent were even looked at, so the same client key reused for a
// different change returned an unrelated stored proposal as a successful
// duplicate.
func TestPropose_IdempotencyKeyIsBoundToItsRequest(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.scriptJudge("pass", "cadence: 2h")
	first := operatorReq("make the news feed less frequent")
	first.IdempotencyKey = "shared-client-key"
	res, err := f.engine.Propose(context.Background(), first)
	if err != nil || res.Refusal != nil {
		t.Fatalf("%v %+v", err, res.Refusal)
	}
	original := res.Proposal.ID

	// Same key, DIFFERENT intent: not a retry of the first request.
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 30m")
	f.scriptJudge("pass", "cadence: 30m")
	second := operatorReq("make the news feed MORE frequent")
	second.IdempotencyKey = "shared-client-key"
	res2, err := f.engine.Propose(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Duplicate && res2.Proposal != nil && res2.Proposal.ID == original {
		t.Fatal("a different intent under a reused key must not replay the first proposal")
	}
	if res2.Refusal == nil && res2.Proposal != nil && res2.Proposal.ID == original {
		t.Fatal("the second request silently returned the first proposal")
	}

	// Same key, DIFFERENT project: must never cross the project boundary.
	third := operatorReq("make the news feed less frequent")
	third.IdempotencyKey = "shared-client-key"
	third.ProjectID = "other-project"
	res3, err := f.engine.Propose(context.Background(), third)
	if err == nil && res3 != nil && res3.Proposal != nil && res3.Proposal.ProjectID == "assistant" {
		t.Fatal("a reused key returned another project's proposal")
	}

	// A GENUINE retry — same project, same actor, same intent — still replays.
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.scriptJudge("pass", "cadence: 2h")
	retry := operatorReq("make the news feed less frequent")
	retry.IdempotencyKey = "shared-client-key"
	res4, err := f.engine.Propose(context.Background(), retry)
	if err != nil {
		t.Fatal(err)
	}
	if !res4.Duplicate || res4.Proposal == nil || res4.Proposal.ID != original {
		t.Fatalf("a true retry must still replay the original proposal, got %+v", res4.Proposal)
	}
}

// Regression: audit 2026-09-15 CA-14 — "Refused or Failed Loops Lose Their
// Usage Record". RunLoop accumulates tokens, then returns a nil result on
// the tool-turn cap, on cancellation and on provider errors; the engine
// returned before reaching e.record, so an expensive failed attempt was
// absent from the project's assistant usage ledger — exactly when a caller
// is most likely to retry.
func TestPropose_FailedLoopStillRecordsUsage(t *testing.T) {
	f := newFixture(t)
	var recorded []usageCall
	f.engine.Usage = func(_ context.Context, project, component, model string, prompt, completion int) {
		recorded = append(recorded, usageCall{project, component, model, prompt, completion})
	}
	// Two tool turns that consume tokens, against a cap of one: the loop
	// burns the tokens and then refuses.
	f.cfg.MaxToolTurns = 1
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["projects/assistant.yaml"]`, call("c1", "file_read", map[string]any{"path": "projects/assistant.yaml"})),
		toolResp("glm-5.2:cloud", "still working", call("c2", "file_read", map[string]any{"path": "swarms/s1.md"})),
		textResp("glm-5.2:cloud", "done"),
	}
	res, err := f.engine.Propose(context.Background(), operatorReq("look around"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal == nil {
		t.Fatal("the tool-turn cap must refuse")
	}
	if len(recorded) == 0 {
		t.Fatal("a refused loop that consumed tokens must still be recorded in usage")
	}
	if recorded[0].prompt == 0 && recorded[0].completion == 0 {
		t.Fatalf("usage recorded with no tokens: %+v", recorded[0])
	}
}

type usageCall struct {
	project, component, model string
	prompt, completion        int
}

// Regression: audit 2026-09-15 CA-07 — "Evidence Truncation Fails Open on the
// Config Apply Path". The engine truncated the serialized Evidence JSON as
// prose, so a long intent produced a record that was under the byte limit and
// no longer valid JSON. Downstream that reads as an EMPTY read set (stale-base
// protection silently gone), an unreadable class (the class-E counter skips
// the row) or an unapplyable proposal, depending on the kind.
func TestPropose_EvidenceIsAlwaysValidJSON(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.scriptJudge("pass", "cadence: 2h")
	req := operatorReq(strings.Repeat("please change the cadence because ", 4000))
	res, err := f.engine.Propose(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		// Refusing the oversized request is an acceptable outcome; filing an
		// invalid record is not.
		return
	}
	if res.Proposal == nil {
		t.Fatal("no proposal and no refusal")
	}
	var ev EvidenceRecord
	if err := json.Unmarshal([]byte(res.Proposal.Evidence), &ev); err != nil {
		t.Fatalf("filed Evidence is not valid JSON (%d bytes): %v", len(res.Proposal.Evidence), err)
	}
	if ev.Class == "" {
		t.Fatal("Evidence must keep its class: the class-E counter reads it")
	}
	if len(ev.ReadSet) == 0 || ev.BaseHash == "" {
		t.Fatalf("Evidence must keep its read set and base hash: %+v", ev)
	}
}

// Regression: audit 2026-09-15 CA-17 — "Class-E Throttling Is a Partial, Racy
// Count". The counter listed at most 1000 proposals GLOBALLY and filtered by
// credential and day afterwards, so on a busy deployment the relevant
// same-day rows fall out of the window and the cap stops binding.
func TestClassECap_CountsBeyondOnePage(t *testing.T) {
	f := newFixture(t)
	f.cfg.ClassEDailyCap = 2
	now := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// Two class-E proposals for our credential, then enough unrelated rows
	// to push them past any fixed page window.
	ev, _ := json.Marshal(EvidenceRecord{Class: ClassE})
	for i := 0; i < 2; i++ {
		if err := f.store.Create(ctx, &persistence.ControlPlaneProposal{
			ID: persistence.GenerateID("cpp"), ProjectID: "assistant", ProposedBy: ProposedBy,
			ActorCredentialID: "key_1", Evidence: string(ev), CreatedAt: now, Status: persistence.ProposalStatusDraft,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 1200; i++ {
		if err := f.store.Create(ctx, &persistence.ControlPlaneProposal{
			ID: persistence.GenerateID("cpp"), ProjectID: "other", ProposedBy: ProposedBy,
			ActorCredentialID: "key_other", Evidence: string(ev), CreatedAt: now.Add(time.Duration(i) * time.Second),
			Status: persistence.ProposalStatusDraft,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if r := f.engine.classECap(ctx, operatorReq("another authority change"), f.cfg, now); r == nil {
		t.Fatal("the credential is at its class-E daily cap: the count must not be limited to one page of the ledger")
	}
}

// Regression: re-audit 2026-09-15 CA-14 (REOPENED) — "cancelled assistant
// requests still cannot persist paid usage".
//
// The first CA-14 fix made RunLoop return partial usage on the ceiling paths,
// which closed the tool-turn case. It did not close the CANCELLATION case:
// e.record still passes the REQUEST's context, so when a browser disconnects
// after a paid turn the recorder is handed a context that is already
// cancelled. Production forwards that to llmspend.Recorder.Record and on to
// the SQL repository, which refuses the write — so the spend that actually
// happened is never persisted, in exactly the situation (an abandoned slow
// request) where it is most likely to happen.
//
// THE SEAM: "the callback was CALLED" and "the callback could WRITE" are
// different claims. A test whose fake recorder ignores ctx proves the first
// and says nothing about the second, which is why this asserts on the
// context the callback receives, not on the call count.
func TestPropose_CancelledRequestStillGetsAUsableAccountingContext(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())

	var recordedErr error
	var recorded int
	f.engine.Usage = func(uctx context.Context, _, _, _ string, _, _ int) {
		recorded++
		recordedErr = uctx.Err()
	}
	// A paid turn, then the client goes away before the loop finishes.
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["projects/assistant.yaml"]`, call("c1", "file_read", map[string]any{"path": "projects/assistant.yaml"})),
		textResp("glm-5.2:cloud", "done"),
	}
	f.engine.Assistant = &cancellingProvider{inner: f.assistant, cancel: cancel}

	_, _ = f.engine.Propose(ctx, operatorReq("look around"))

	if recorded == 0 {
		t.Fatal("a paid turn must still be recorded after cancellation")
	}
	if recordedErr != nil {
		t.Fatalf("the usage recorder was handed an unusable context (%v): a real repository rejects the write and the spend is lost", recordedErr)
	}
}

// cancellingProvider cancels the caller's context after the first response,
// making the client-disconnect interleaving deterministic rather than timed.
type cancellingProvider struct {
	inner  *scriptedProvider
	cancel context.CancelFunc
	fired  bool
}

func (p *cancellingProvider) Complete(ctx context.Context, m []chat.Message) (*chat.ChatResponse, error) {
	return p.CompleteWithTools(ctx, m, nil)
}
func (p *cancellingProvider) CompleteWithTools(ctx context.Context, m []chat.Message, tools []chat.Tool) (*chat.ChatResponse, error) {
	resp, err := p.inner.CompleteWithTools(ctx, m, tools)
	if !p.fired {
		p.fired = true
		p.cancel()
	}
	return resp, err
}
func (p *cancellingProvider) CompleteWithToolsStream(ctx context.Context, m []chat.Message, tools []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return p.CompleteWithTools(ctx, m, tools)
}
func (p *cancellingProvider) Model() string              { return p.inner.Model() }
func (p *cancellingProvider) SetMetrics(_ *chat.Metrics) {}

// Regression: re-audit 2026-09-15 CA-13 — "Replay also restores only
// Proposal/Duplicate, leaving class, judge, diff, permissions and measurement
// indicators empty."
//
// The console's whole retry story rests on a replay being SAFE and USEFUL:
// the operator refreshes after an uncertain response and should see the
// result they missed. They saw a blank shell instead — the proposal existed
// and everything describing it was dropped on the floor.
func TestPropose_ReplayReturnsTheFullResult(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.scriptJudge("pass", "cadence: 2h")
	req := operatorReq("make the news feed less frequent")
	req.IdempotencyKey = "console-token"
	first, err := f.engine.Propose(context.Background(), req)
	if err != nil || first.Refusal != nil {
		t.Fatalf("%v %+v", err, first.Refusal)
	}

	replay, err := f.engine.Propose(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Duplicate {
		t.Fatal("the retry must be reported as a duplicate")
	}
	if replay.Class != first.Class {
		t.Errorf("class = %q, want %q: the operator cannot see what the change was", replay.Class, first.Class)
	}
	if replay.Diff == "" {
		t.Error("replay has no diff")
	}
	if replay.Verdict == nil || !replay.Verdict.Judged {
		t.Errorf("replay lost the judge verdict: %+v", replay.Verdict)
	}
	if len(replay.Touches) == 0 {
		t.Error("replay lost the per-key touches")
	}
}
