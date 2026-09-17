package configassist

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
)

// ---- fixtures -------------------------------------------------------------

const fixtureProject = `projectId: assistant
displayName: Assistant
swarmId: s1
defaultWorkflowId: w1
autonomy:
  enabled: true
  goal: keep the operator informed
  maxTasksPerHour: 4
  feeds:
    - slug: news
      cadence: 1h
    - slug: events
      cadence: 720h
`

const fixtureSwarm = `---
swarmId: s1
displayName: S1
roles:
  - name: lead
    model: glm-5.2:cloud
    runtime:
      image: "ghcr.io/grinco/vornik-agent:latest"
    permissions:
      allowedTools:
        - file_read
  - name: reviewer
    model: glm-5.2:cloud
    runtime:
      image: "ghcr.io/grinco/vornik-agent:latest"
    permissions:
      allowedTools:
        - file_read
---
# S1
`

const fixtureWorkflow = `---
workflowId: "w1"
displayName: "W1"
description: "test"
version: "1.0.0"
entrypoint: "plan"
steps:
  plan:
    type: "agent"
    role: "lead"
    on_success: "complete"
    on_fail: "failed"
    timeout: "10m"
    prompt: |
      Plan the work.
terminals:
  complete:
    status: "COMPLETED"
  failed:
    status: "FAILED"
---
# W1
`

type fixture struct {
	root      string // deployed configs dir
	reg       *registry.Registry
	assistant *scriptedProvider
	judge     *scriptedProvider
	store     *memProposals
	applier   *memApplier
	cfg       config.AssistantConfig
	engine    *Engine
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("projects/assistant.yaml", fixtureProject)
	write("swarms/s1.md", fixtureSwarm)
	write("workflows/w1.md", fixtureWorkflow)
	reg := registry.New()
	if err := reg.Load(root); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	if reg.GetProject("assistant") == nil {
		t.Fatal("fixture project did not load")
	}
	f := &fixture{root: root, reg: reg, assistant: &scriptedProvider{model: "glm-5.2:cloud"}, judge: &scriptedProvider{model: "nvidia.nemotron-nano-9b-v2"},
		store: newMemProposals(), applier: &memApplier{}}
	f.cfg = config.AssistantConfig{Enabled: true, Model: "glm-5.2:cloud", JudgeModel: "nvidia.nemotron-nano-9b-v2", MaxToolTurns: 20, MaxOutputBytes: 64 * 1024, ClassEDailyCap: 2, MaxConcurrentPerProject: 1}
	f.engine = &Engine{
		TreeRoot: root, ApplyPrefix: "configs", Registry: reg, Assistant: f.assistant, Judge: f.judge,
		Proposals: f.store, Applier: f.applier, Config: func() config.AssistantConfig { return f.cfg },
		Now: func() time.Time { return time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC) },
	}
	return f
}

// scriptedProvider replays scripted responses in order; the judge script is
// a plain JSON answer. It records every call so a test can assert the
// TRANSPORT was or was not reached (tests 32–35 are transport-level).
type scriptedProvider struct {
	mu       sync.Mutex
	model    string
	script   []*chat.ChatResponse
	err      error
	calls    int
	messages [][]chat.Message
}

func (s *scriptedProvider) next(msgs []chat.Message) (*chat.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.messages = append(s.messages, msgs)
	if s.err != nil {
		return nil, s.err
	}
	if len(s.script) == 0 {
		return &chat.ChatResponse{Model: s.model, Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{{Message: chat.Message{Role: "assistant", Content: "done"}}}}, nil
	}
	r := s.script[0]
	s.script = s.script[1:]
	return r, nil
}
func (s *scriptedProvider) Complete(_ context.Context, m []chat.Message) (*chat.ChatResponse, error) {
	return s.next(m)
}
func (s *scriptedProvider) CompleteWithTools(_ context.Context, m []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return s.next(m)
}
func (s *scriptedProvider) CompleteWithToolsStream(_ context.Context, m []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.next(m)
}
func (s *scriptedProvider) Model() string              { return s.model }
func (s *scriptedProvider) SetMetrics(_ *chat.Metrics) {}
func (s *scriptedProvider) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func textResp(model, text string) *chat.ChatResponse {
	r := &chat.ChatResponse{Model: model}
	r.Choices = append(r.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: text}})
	r.Usage.PromptTokens, r.Usage.CompletionTokens = 100, 20
	return r
}

func toolResp(model, text string, calls ...chat.ToolCall) *chat.ChatResponse {
	r := textResp(model, text)
	r.Choices[0].Message.ToolCalls = calls
	return r
}

func call(id, name string, args map[string]any) chat.ToolCall {
	b, _ := json.Marshal(args)
	return chat.ToolCall{ID: id, Type: "function", Function: chat.FunctionCall{Name: name, Arguments: string(b)}}
}

// scriptEdit scripts the assistant: declare the plan, edit with file_edit,
// then finish with a summary.
//
//nolint:unparam // Keeping the target explicit makes each scripted scenario self-describing.
func (f *fixture) scriptEdit(file, old, replacement string) {
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["`+file+`"]`, call("c1", "file_edit", map[string]any{"path": file, "old_string": old, "new_string": replacement})),
		textResp("glm-5.2:cloud", "Changed "+file+" as asked."),
	}
}

func (f *fixture) scriptJudge(decision, quoted string) {
	f.judge.script = []*chat.ChatResponse{textResp("nvidia.nemotron-nano-9b-v2",
		`{"decision":"`+decision+`","confidence":0.9,"summary":"the diff changes `+quoted+`","quoted_lines":["`+quoted+`"]}`)}
}

// memProposals is an in-memory ProposalStore with idempotency.
type memProposals struct {
	mu   sync.Mutex
	rows map[string]*persistence.ControlPlaneProposal
	byIK map[string]string
}

func newMemProposals() *memProposals {
	return &memProposals{rows: map[string]*persistence.ControlPlaneProposal{}, byIK: map[string]string{}}
}
func (m *memProposals) Create(_ context.Context, p *persistence.ControlPlaneProposal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.IdempotencyKey != "" {
		if _, ok := m.byIK[p.IdempotencyKey]; ok {
			return persistence.ErrDuplicateKey
		}
		m.byIK[p.IdempotencyKey] = p.ID
	}
	cp := *p
	m.rows[p.ID] = &cp
	return nil
}
func (m *memProposals) GetByID(_ context.Context, id string) (*persistence.ControlPlaneProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.rows[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

// List honours Limit the way both real drivers do — newest first, capped.
// A double that ignored the cap hid audit finding CA-17 (2026-09-15): the
// class-E counter read one page of the ledger and called it the whole day.
func (m *memProposals) List(_ context.Context, f persistence.ProposalListFilter) ([]*persistence.ControlPlaneProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*persistence.ControlPlaneProposal
	for _, p := range m.rows {
		if f.ProjectID != "" && p.ProjectID != f.ProjectID {
			continue
		}
		if f.ProposedBy != "" && p.ProposedBy != f.ProposedBy {
			continue
		}
		if f.ActorCredentialID != "" && p.ActorCredentialID != f.ActorCredentialID {
			continue
		}
		if !f.CreatedFrom.IsZero() && p.CreatedAt.Before(f.CreatedFrom) {
			continue
		}
		if !f.CreatedTo.IsZero() && !p.CreatedAt.Before(f.CreatedTo) {
			continue
		}
		cp := *p
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}
func (m *memProposals) SetStatus(_ context.Context, id, status, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	p.Status, p.Approver = status, actor
	return nil
}
func (m *memProposals) GetByIdempotencyKey(_ context.Context, key string) (*persistence.ControlPlaneProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.byIK[key]; ok {
		cp := *m.rows[id]
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

type memApplier struct {
	mu      sync.Mutex
	applied []string
}

func (a *memApplier) Apply(_ context.Context, id, _ string, _ bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applied = append(a.applied, id)
	return nil
}

func operatorReq(intent string) Request {
	return Request{ProjectID: "assistant", Intent: intent, Entrypoint: EntrypointREST, Actor: Actor{Kind: "credential", CredentialID: "key_1", Principal: "api_key_id:key_1"}}
}

// ---- tests ----------------------------------------------------------------

func TestMemProposalsMissContract(t *testing.T) {
	repo := newMemProposals()
	repotest.AssertMissRepo(t, "ProposalRepository.GetByID", repo.GetByID)
	repotest.AssertMissRepo(t, "ProposalRepository.GetByIdempotencyKey", repo.GetByIdempotencyKey)
}

// Test 10/11 shape (the unit half): a well-formed class-A edit produces a
// proposal whose ApplyOps the shipped engine consumes, class A, judged
// pass, base hashes and read set recorded; and auto-applies only with the
// project opt-in.
func TestPropose_ClassA_FilesProposalAndAutoAppliesWithOptIn(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.scriptJudge("pass", "cadence: 2h")
	res, err := f.engine.Propose(context.Background(), operatorReq("make the news feed less frequent"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		t.Fatalf("unexpected refusal: %v", res.Refusal)
	}
	if res.Class != ClassA {
		t.Fatalf("class = %s (%v)", res.Class, res.Touches)
	}
	if res.Proposal == nil || res.Proposal.ProposedBy != ProposedBy || res.Proposal.Status != persistence.ProposalStatusDraft {
		t.Fatalf("proposal = %+v", res.Proposal)
	}
	var ops []Op
	if err := json.Unmarshal([]byte(res.Proposal.ApplyOps), &ops); err != nil || len(ops) != 1 || ops[0].Op != "replace" || ops[0].Path != "configs/projects/assistant.yaml" {
		t.Fatalf("apply ops = %s (%v)", res.Proposal.ApplyOps, err)
	}
	if !strings.Contains(ops[0].Content, "cadence: 2h") || strings.Contains(ops[0].Content, "PLACEHOLDER") {
		t.Fatalf("content: %s", ops[0].Content)
	}
	var ev EvidenceRecord
	if err := json.Unmarshal([]byte(res.Proposal.Evidence), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.BaseHash == "" || ev.ReadSet["configs/projects/assistant.yaml"] != ev.BaseHash || ev.ReadSet["configs/swarms/s1.md"] == "" {
		t.Fatalf("evidence must carry the base hash and the read set: %+v", ev.ReadSet)
	}
	if ev.HasMeasurement || !strings.Contains(res.Proposal.Rationale, "No measurement supports this change") {
		t.Fatalf("no evidence source wired → the proposal must say so (test 28): %s", res.Proposal.Rationale)
	}
	if res.Verdict == nil || !res.Verdict.Pass() {
		t.Fatalf("verdict = %+v", res.Verdict)
	}
	if res.AutoApplied || len(f.applier.applied) != 0 {
		t.Fatal("no opt-in → no auto-apply")
	}
	if res.Proposal.Entrypoint != EntrypointREST || res.Proposal.ActorCredentialID != "key_1" || res.Proposal.RequestID == "" {
		t.Fatalf("ledger identifiers: %+v", res.Proposal)
	}

	// With the project's opt-in, a judged PASS class A auto-applies.
	f2 := newFixture(t)
	f2.cfg.AutoApply = map[string]config.AssistantAutoApply{"assistant": {Classes: []string{"A"}}}
	f2.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f2.scriptJudge("pass", "cadence: 2h")
	res, err = f2.engine.Propose(context.Background(), operatorReq("less frequent"))
	if err != nil || res.Refusal != nil {
		t.Fatalf("%v %v", err, res.Refusal)
	}
	if !res.AutoApplied || len(f2.applier.applied) != 1 {
		t.Fatal("opt-in + pass must auto-apply")
	}
	p, _ := f2.store.GetByID(context.Background(), res.Proposal.ID)
	if p.Status != persistence.ProposalStatusApproved || p.Approver != AutoApplyActor {
		t.Fatalf("auto-approve must be recorded: %+v", p)
	}
}

func TestPropose_WorkspaceProjectContextFilesManagedProposal(t *testing.T) {
	f := newFixture(t)
	ws := t.TempDir()
	ctxPath := filepath.Join(ws, "assistant", ".autonomy", "PROJECT_CONTEXT.md")
	if err := os.MkdirAll(filepath.Dir(ctxPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctxPath, []byte("# Context\n- old failing news source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.engine.WorkspaceRoot = ws
	f.scriptEdit("projects/assistant/PROJECT_CONTEXT.md", "old failing news source", "replacement working news source")
	f.scriptJudge("pass", "replacement working news source")

	res, err := f.engine.Propose(context.Background(), operatorReq("replace the failing news source guidance"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		t.Fatalf("unexpected refusal: %v", res.Refusal)
	}
	if res.Class != ClassB2 {
		t.Fatalf("class = %s (%v), want B2", res.Class, res.Touches)
	}
	if res.Proposal == nil || res.Proposal.Kind != persistence.ProposalKindWorkspaceContext {
		t.Fatalf("proposal kind = %+v", res.Proposal)
	}
	var ops []Op
	if err := json.Unmarshal([]byte(res.Proposal.ApplyOps), &ops); err != nil || len(ops) != 1 {
		t.Fatalf("apply ops = %s (%v)", res.Proposal.ApplyOps, err)
	}
	if ops[0].Path != "workspace/assistant/.autonomy/PROJECT_CONTEXT.md" || !strings.Contains(ops[0].Content, "replacement working news source") {
		t.Fatalf("workspace op = %+v", ops[0])
	}
	var ev EvidenceRecord
	if err := json.Unmarshal([]byte(res.Proposal.Evidence), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ReadSet["workspace/assistant/.autonomy/PROJECT_CONTEXT.md"] == "" {
		t.Fatalf("workspace context must carry stale-base hash in read set: %+v", ev.ReadSet)
	}
	if strings.Contains(res.Diff, ws) {
		t.Fatalf("diff leaked host workspace root: %s", res.Diff)
	}
}

func TestWorkspaceContextPathRejectsTraversal(t *testing.T) {
	if _, err := workspaceContextPath(t.TempDir(), "../outside"); err == nil {
		t.Fatal("workspace context path must reject project-id traversal")
	}
}

// Test 9: the judge sees the intent, diff, class and evidence — never the
// assistant's reasoning or transcript.
func TestPropose_JudgeNeverSeesAuthorReasoning(t *testing.T) {
	f := newFixture(t)
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["projects/assistant.yaml"]`+"\nSECRET-REASONING-MARKER", call("c1", "file_edit", map[string]any{"path": "projects/assistant.yaml", "old_string": "cadence: 1h", "new_string": "cadence: 2h"})),
		textResp("glm-5.2:cloud", "SUMMARY-MARKER changed it"),
	}
	f.scriptJudge("pass", "cadence: 2h")
	if _, err := f.engine.Propose(context.Background(), operatorReq("less often")); err != nil {
		t.Fatal(err)
	}
	if len(f.judge.messages) != 1 {
		t.Fatalf("judge calls = %d", len(f.judge.messages))
	}
	for _, m := range f.judge.messages[0] {
		if strings.Contains(m.Content, "SECRET-REASONING-MARKER") || strings.Contains(m.Content, "SUMMARY-MARKER") {
			t.Fatalf("judge was given the author's reasoning: %s", m.Content)
		}
	}
	if !strings.Contains(f.judge.messages[0][1].Content, "-      cadence: 1h") || !strings.Contains(f.judge.messages[0][1].Content, "less often") {
		t.Fatalf("judge must see the diff and the intent: %s", f.judge.messages[0][1].Content)
	}
}

// Tests 6 and 7: abstain does not apply and the proposal is marked NOT
// JUDGED; an unavailable judge → propose, never apply.
func TestPropose_AbstainAndUnavailableJudgeNeverApply(t *testing.T) {
	for _, mode := range []string{"abstain", "unavailable", "noquote"} {
		f := newFixture(t)
		f.cfg.AutoApply = map[string]config.AssistantAutoApply{"assistant": {Classes: []string{"A"}}}
		f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
		switch mode {
		case "abstain":
			f.scriptJudge("abstain", "cadence: 2h")
		case "unavailable":
			f.judge.err = errors.New("judge down")
		case "noquote":
			f.judge.script = []*chat.ChatResponse{textResp("j", `{"decision":"pass","confidence":0.9,"summary":"looks correct"}`)}
		}
		res, err := f.engine.Propose(context.Background(), operatorReq("less often"))
		if err != nil || res.Refusal != nil {
			t.Fatalf("%s: %v %v", mode, err, res.Refusal)
		}
		if res.AutoApplied || len(f.applier.applied) != 0 {
			t.Fatalf("%s: must not auto-apply", mode)
		}
		if res.Verdict.Pass() {
			t.Fatalf("%s: must not be a pass: %+v", mode, res.Verdict)
		}
		if mode == "unavailable" && (res.Verdict.Judged || !strings.Contains(res.Proposal.Rationale, "NOT JUDGED")) {
			t.Fatalf("unavailable judge must file NOT JUDGED: %+v %s", res.Verdict, res.Proposal.Rationale)
		}
		if mode == "noquote" && res.Verdict.Decision != "abstain" {
			t.Fatalf("a verdict that does not quote the changed lines is abstain (test 18): %+v", res.Verdict)
		}
	}
}

// Test 16: the global pause short-circuits BEFORE any model call; a
// per-subject `false` disables; absent leaves it enabled.
func TestPropose_KillSwitches(t *testing.T) {
	f := newFixture(t)
	f.cfg.Paused = true
	res, err := f.engine.Propose(context.Background(), operatorReq("anything"))
	if err != nil || res.Refusal == nil || res.Refusal.Code != RefusePaused {
		t.Fatalf("%v %v", err, res.Refusal)
	}
	if f.assistant.Calls() != 0 || f.judge.Calls() != 0 {
		t.Fatal("paused must not call any model")
	}

	f = newFixture(t)
	off := false
	f.reg.GetProject("assistant").ConfigAssistantEnabled = &off
	res, _ = f.engine.Propose(context.Background(), operatorReq("anything"))
	if res.Refusal == nil || res.Refusal.Code != RefuseSubjectDisabled || f.assistant.Calls() != 0 {
		t.Fatalf("explicit false must disable before the model: %v", res.Refusal)
	}

	f = newFixture(t)
	f.cfg.DisabledClasses = []string{"A"}
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	res, _ = f.engine.Propose(context.Background(), operatorReq("less often"))
	if res.Refusal == nil || res.Refusal.Code != RefuseClassDisabled {
		t.Fatalf("class kill switch: %v", res.Refusal)
	}
}

// Tests 32/33: a project whose tree trips the hygiene finding is REFUSED
// before any model call — asserted on the assistant transport (zero calls)
// — and the refusal names the finding and the file.
func TestPropose_SecretHygieneRefusesBeforeAnyModelCall(t *testing.T) {
	// Half 1: config.yaml carries a raw secret (the doctor's own finding).
	f := newFixture(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("database:\n  password: sk-live-abcdefghijklmnopqrstuvwxyz0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.engine.ConfigPath = cfgPath
	f.engine.SecretSnapshot = func() map[string]string {
		return map[string]string{"database.password": "sk-live-abcdefghijklmnopqrstuvwxyz0123456789"}
	}
	res, err := f.engine.Propose(context.Background(), operatorReq("less often"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal == nil || res.Refusal.Code != RefuseSecretHygiene {
		t.Fatalf("want hygiene refusal, got %v", res.Refusal)
	}
	if !strings.Contains(res.Refusal.Error(), "database.password") || !strings.Contains(res.Refusal.Error(), cfgPath) {
		t.Fatalf("refusal must name the finding and the file: %v", res.Refusal)
	}
	if f.assistant.Calls() != 0 || f.judge.Calls() != 0 {
		t.Fatal("no outbound model call may happen when the tree carries a raw secret")
	}
	// Freshness (test 33): the same engine, snapshot now clean → runs.
	f.engine.SecretSnapshot = func() map[string]string { return map[string]string{"database.password": "${DB_PASSWORD}"} }
	if err := os.WriteFile(cfgPath, []byte("database:\n  password: ${DB_PASSWORD}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.scriptJudge("pass", "cadence: 2h")
	res, _ = f.engine.Propose(context.Background(), operatorReq("less often"))
	if res.Refusal != nil {
		t.Fatalf("a fresh clean evaluation must run: %v", res.Refusal)
	}

	// Half 2: a project file the project reads carries a pasted secret.
	g := newFixture(t)
	if err := os.MkdirAll(filepath.Join(g.root, "projects", "assistant"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.root, "projects", "assistant", "PROJECT_CONTEXT.md"), []byte("# ctx\napi_key: sk-live-abcdefghijklmnopqrstuvwxyz0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ = g.engine.Propose(context.Background(), operatorReq("less often"))
	if res.Refusal == nil || res.Refusal.Code != RefuseSecretHygiene || !strings.Contains(res.Refusal.Error(), "projects/assistant/PROJECT_CONTEXT.md") {
		t.Fatalf("project-file secret must refuse naming the file: %v", res.Refusal)
	}
	if g.assistant.Calls() != 0 {
		t.Fatal("no model call")
	}
}

// Test 3 at the engine: an unknown key is a skew refusal and NO proposal is
// filed.
func TestPropose_UnknownKeyRefusedNothingFiled(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "maxTasksPerHour: 4", "maxTasksPerHour: 4\n  cadence_default: 2h")
	res, err := f.engine.Propose(context.Background(), operatorReq("add a default cadence"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal == nil || res.Refusal.Code != RefuseSkew || !strings.Contains(res.Refusal.Error(), "cadence_default") {
		t.Fatalf("want skew refusal, got %v", res.Refusal)
	}
	if len(f.store.rows) != 0 || f.judge.Calls() != 0 {
		t.Fatal("nothing may be filed or judged on a gate failure")
	}
}

// Test 4 at the engine: an edit outside the declared plan is refused.
func TestPropose_OutOfScopeEditRefused(t *testing.T) {
	f := newFixture(t)
	f.assistant.script = []*chat.ChatResponse{
		toolResp("m", `PLAN: ["projects/assistant.yaml"]`, call("c1", "file_write", map[string]any{"path": "workflows/w1.md", "content": "x"})),
		textResp("m", "done"),
	}
	res, _ := f.engine.Propose(context.Background(), operatorReq("x"))
	// The loop itself refuses the write (not in plan) so nothing changes.
	if res.Refusal == nil || res.Refusal.Code != RefuseNoChange {
		t.Fatalf("undeclared write must be refused at the tool and leave no change: %v", res.Refusal)
	}
}

// Test 17: a completion past MaxOutputBytes is refused, never truncated.
func TestPropose_OutputCapRefuses(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxOutputBytes = 10
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h — a much longer value")
	res, _ := f.engine.Propose(context.Background(), operatorReq("x"))
	if res.Refusal == nil || res.Refusal.Code != RefuseOutputCap || len(f.store.rows) != 0 {
		t.Fatalf("want output-cap refusal, got %v", res.Refusal)
	}
}

// Tests 5, 19, 20, 24, 36: class E on an operator entrypoint is filed for HUMAN
// approval with the effective-permission diff, never auto-applied even with
// an opt-in (there is no config that enables it), and capped per credential
// per day by the persisted ledger.
func TestPropose_ClassE_HumanApprovedPermissionDiffAndCap(t *testing.T) {
	f := newFixture(t)
	f.cfg.AutoApply = map[string]config.AssistantAutoApply{"assistant": {Classes: []string{"A", "B1", "B2"}}}
	f.cfg.ClassEDailyCap = 1
	// Widen the reviewer by REMOVING its allowedTools block: a role with
	// no allowedTools is UNRESTRICTED, which a text diff would never show.
	f.assistant.script = []*chat.ChatResponse{
		toolResp("m", `PLAN: ["swarms/s1.md"]`, call("c1", "file_edit", map[string]any{"path": "swarms/s1.md", "old_string": "  - name: reviewer\n    model: glm-5.2:cloud\n    runtime:\n      image: \"ghcr.io/grinco/vornik-agent:latest\"\n    permissions:\n      allowedTools:\n        - file_read\n", "new_string": "  - name: reviewer\n    model: glm-5.2:cloud\n    runtime:\n      image: \"ghcr.io/grinco/vornik-agent:latest\"\n"})),
		textResp("m", "widened"),
	}
	f.scriptJudge("pass", "allowedTools:")
	res, err := f.engine.Propose(context.Background(), operatorReq("let the reviewer use more tools"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		t.Fatalf("unexpected refusal: %v", res.Refusal)
	}
	if res.Class != ClassE || res.AutoApplied || len(f.applier.applied) != 0 {
		t.Fatalf("class E must be filed for human approval and never auto-applied: class=%s applied=%v touches=%v", res.Class, res.AutoApplied, res.Touches)
	}
	if len(res.Permissions) == 0 || !strings.Contains(RenderPermissionDiff(res.Permissions), "every tool") {
		t.Fatalf("class E must render the EFFECTIVE permission diff (gains: every tool), got %+v", res.Permissions)
	}
	if !strings.Contains(res.Proposal.Rationale, "EFFECTIVE PERMISSION DIFF") || res.Proposal.Status != persistence.ProposalStatusDraft {
		t.Fatal("proposal must carry the permission diff and await a human")
	}

	// The daily cap is the ledger: seed one class-E row for this credential
	// today (as a restarted daemon or another replica would see it) and the
	// next class-E request is refused loudly, not queued.
	seed := &persistence.ControlPlaneProposal{ID: "cpp_seed", ProposedBy: ProposedBy, ActorCredentialID: "key_1", CreatedAt: f.engine.Now(), Evidence: `{"class":"E"}`}
	_ = f.store.Create(context.Background(), seed)
	g := newFixture(t)
	g.store = f.store
	g.engine.Proposals = f.store
	g.cfg.ClassEDailyCap = 1
	g.assistant.script = []*chat.ChatResponse{
		toolResp("m", `PLAN: ["projects/assistant.yaml"]`, call("c1", "file_write", map[string]any{"path": "projects/assistant.yaml", "content": fixtureProject + "permissions:\n  allowedTools:\n    - run_shell\n"})),
		textResp("m", "granted"),
	}
	res, err = g.engine.Propose(context.Background(), operatorReq("give the project run_shell"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal == nil || res.Refusal.Code != RefuseClassECap {
		t.Fatalf("second class-E today must hit the persisted cap: class=%s refusal=%v", res.Class, res.Refusal)
	}
	if g.judge.Calls() != 0 {
		t.Fatal("a capped request is refused before the judge")
	}
}

// Tests 21/29 (engine half) and 22: the same class-D payload proposes on an
// operator entrypoint and is refused by class on the chat and agent entrypoints naming
// the entrypoint; an agent-entrypoint class-A proposal never auto-applies and carries
// its trace.
func TestPropose_EntrypointCeilings(t *testing.T) {
	shorter := func(f *fixture) {
		f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 1m")
		f.scriptJudge("pass", "cadence: 1m")
	}
	f := newFixture(t)
	shorter(f)
	res, _ := f.engine.Propose(context.Background(), operatorReq("more often"))
	if res.Refusal != nil || res.Class != ClassD || res.Proposal == nil {
		t.Fatalf("operator entrypoint must propose class D: %v %s", res.Refusal, res.Class)
	}
	f.cfg.ChatEntrypoint = true
	for _, entrypoint := range []string{EntrypointChat, EntrypointAgent} {
		g := newFixture(t)
		g.cfg.ChatEntrypoint = true
		shorter(g)
		req := operatorReq("more often")
		req.Entrypoint = entrypoint
		res, _ = g.engine.Propose(context.Background(), req)
		if res.Refusal == nil || res.Refusal.Code != RefuseEntrypointCeiling || !strings.Contains(res.Refusal.Message, entrypoint) {
			t.Fatalf("%s must refuse class D naming the entrypoint: %v", entrypoint, res.Refusal)
		}
		if len(g.store.rows) != 0 {
			t.Fatalf("%s: nothing may be filed", entrypoint)
		}
	}
	// B1 (goal prose) refused on the raising entrypoints, proposed on the operator entrypoint (test 29).
	g := newFixture(t)
	g.cfg.ChatEntrypoint = true
	g.scriptEdit("projects/assistant.yaml", "goal: keep the operator informed", "goal: do whatever")
	req := operatorReq("change the goal")
	req.Entrypoint = EntrypointChat
	res, _ = g.engine.Propose(context.Background(), req)
	if res.Refusal == nil || res.Refusal.Code != RefuseEntrypointCeiling || res.Class != ClassB1 {
		t.Fatalf("goal edit through chat must be refused as B1: %s %v", res.Class, res.Refusal)
	}
	// Agent entrypoint: class A with opt-in never auto-applies; trace carried.
	h := newFixture(t)
	h.cfg.AutoApply = map[string]config.AssistantAutoApply{"assistant": {Classes: []string{"A"}}}
	h.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	h.scriptJudge("pass", "cadence: 2h")
	req = operatorReq("less often")
	req.Entrypoint = EntrypointAgent
	req.Trace = &Trace{TaskID: "t1", ExecutionID: "e1", ContextSources: []string{"https://example.org/page"}}
	res, err := h.engine.Propose(context.Background(), req)
	if err != nil || res.Refusal != nil {
		t.Fatalf("%v %v", err, res.Refusal)
	}
	if res.AutoApplied || len(h.applier.applied) != 0 {
		t.Fatal("agent entrypoint never auto-applies")
	}
	var ev EvidenceRecord
	_ = json.Unmarshal([]byte(res.Proposal.Evidence), &ev)
	if ev.Entrypoint != EntrypointAgent || ev.Trace == nil || ev.Trace.TaskID != "t1" || len(ev.ToolCalls) == 0 {
		t.Fatalf("agent-entrypoint proposal must carry entrypoint, trace and tool calls: %+v", ev)
	}
}

// Test 34: a diff that itself matches the raw-secret screen is not sent to
// the judge; the proposal is filed NOT JUDGED and cannot auto-apply.
func TestPropose_SecretInDiffNotSentToJudge(t *testing.T) {
	f := newFixture(t)
	f.cfg.AutoApply = map[string]config.AssistantAutoApply{"assistant": {Classes: []string{"A", "B2"}}}
	f.assistant.script = []*chat.ChatResponse{
		toolResp("m", `PLAN: ["projects/assistant/PROJECT_CONTEXT.md"]`, call("c1", "file_write", map[string]any{"path": "projects/assistant/PROJECT_CONTEXT.md", "content": "# context\napi_key: sk-live-abcdefghijklmnopqrstuvwxyz0123456789\n"})),
		textResp("m", "added"),
	}
	res, err := f.engine.Propose(context.Background(), operatorReq("add a secret"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		t.Fatalf("unexpected refusal: %v", res.Refusal)
	}
	if f.judge.Calls() != 0 {
		t.Fatal("the diff must not reach the judge transport")
	}
	if res.Verdict.Judged || res.AutoApplied {
		t.Fatalf("must be NOT JUDGED and not applied: %+v", res.Verdict)
	}
}

// R8: an idempotent replay returns the existing proposal.
func TestPropose_IdempotencyKeyReplays(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.scriptJudge("pass", "cadence: 2h")
	req := operatorReq("less often")
	req.IdempotencyKey = "ik-1"
	first, err := f.engine.Propose(context.Background(), req)
	if err != nil || first.Refusal != nil {
		t.Fatalf("%v %v", err, first.Refusal)
	}
	second, err := f.engine.Propose(context.Background(), req)
	if err != nil || !second.Duplicate || second.Proposal.ID != first.Proposal.ID {
		t.Fatalf("replay must return the same proposal: %v %+v", err, second)
	}
	if f.assistant.Calls() != 2 { // the first run's two turns; the replay adds none
		t.Fatalf("replay must not call the model again: calls = %d", f.assistant.Calls())
	}
}

// Test 37 request path: a same-family pair is refused before any call.
func TestPropose_SameFamilyJudgeRefused(t *testing.T) {
	f := newFixture(t)
	f.cfg.JudgeModel = "glm-4.5-air"
	res, _ := f.engine.Propose(context.Background(), operatorReq("x"))
	if res.Refusal == nil || res.Refusal.Code != RefuseModelFamily || f.assistant.Calls() != 0 {
		t.Fatalf("same family must refuse: %v", res.Refusal)
	}
}

// Tests 25/26/28 at the grounding: absent series render NOT MEASURED, a
// project with no feeds renders NOT DECLARED.
func TestGrounding_HonestyRules(t *testing.T) {
	f := newFixture(t)
	g, err := BuildGrounding(context.Background(), f.reg, nil, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	ev := g.RenderEvidence()
	if strings.Contains(ev, ": 0") || !strings.Contains(ev, "autonomy_health: NOT MEASURED") {
		t.Fatalf("absent series must render NOT MEASURED, never zero:\n%s", ev)
	}
	if g.HasMeasurement() {
		t.Fatal("no evidence source → no measurement")
	}
	if !strings.Contains(g.Feeds, "feed news: cadence 1h0m0s") {
		t.Fatalf("feeds: %s", g.Feeds)
	}
	sp := g.SystemPrompt(ClassTable)
	if !strings.Contains(sp, "autonomy.feeds") || !strings.Contains(sp, "step plan") || !strings.Contains(sp, "role reviewer") {
		t.Fatal("system prompt must carry schema keys, step graph and roles")
	}
	// No feeds + autonomy off → NOT DECLARED (test 26).
	p := f.reg.GetProject("assistant")
	p.Autonomy.Feeds = nil
	p.Autonomy.Enabled = false
	g, _ = BuildGrounding(context.Background(), f.reg, stubEvidence{}, "assistant")
	if !strings.Contains(g.RenderEvidence(), "autonomy_health: NOT DECLARED") || !strings.Contains(g.Feeds, "NOT DECLARED") {
		t.Fatalf("not declared must render as such, never OK:\n%s\n%s", g.RenderEvidence(), g.Feeds)
	}
}

type stubEvidence struct{}

func (stubEvidence) AutonomyHealth(context.Context, string) (any, bool, error) {
	return nil, false, nil
}
func (stubEvidence) RatingsRollup(context.Context, string) (any, bool, error) { return nil, false, nil }
func (stubEvidence) QualityPercentiles(context.Context, string) (any, bool, error) {
	return nil, false, nil
}
func (stubEvidence) JudgeVerdicts(context.Context, string) (any, bool, error) { return nil, false, nil }

// last returns the most recently created proposal, for tests that assert on
// the ledger row rather than the returned Result.
func (m *memProposals) last() *persistence.ControlPlaneProposal {
	m.mu.Lock()
	defer m.mu.Unlock()
	var newest *persistence.ControlPlaneProposal
	for _, p := range m.rows {
		if newest == nil || p.CreatedAt.After(newest.CreatedAt) {
			cp := *p
			newest = &cp
		}
	}
	return newest
}

// count returns how many proposals reached the ledger.
func (m *memProposals) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}
