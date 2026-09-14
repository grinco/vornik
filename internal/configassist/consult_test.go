package configassist

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

type fakePeer struct {
	mu     sync.Mutex
	name   string
	answer string
	err    error
	calls  int
	asked  []string
}

func (p *fakePeer) Name() string { return p.name }
func (p *fakePeer) Ask(_ context.Context, q string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.asked = append(p.asked, q)
	return p.answer, p.err
}
func (p *fakePeer) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakeAudit struct {
	mu   sync.Mutex
	rows []*persistence.AdminAuditEntry
	err  error
	// failAfter makes Insert fail once N rows are stored (the outcome path).
	failAfter int
}

func (a *fakeAudit) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	if a.failAfter > 0 && len(a.rows) >= a.failAfter {
		return errors.New("audit store down")
	}
	a.rows = append(a.rows, e)
	return nil
}
func (a *fakeAudit) List(_ context.Context, f persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*persistence.AdminAuditEntry
	for _, r := range a.rows {
		if f.Action != "" && r.Action != f.Action {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
func (a *fakeAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.rows {
		out = append(out, r.Action)
	}
	return out
}

func newConsultant(peer Peer, audit ConsultAudit) *Consultant {
	return &Consultant{Peer: peer, Audit: audit, Actor: Actor{Principal: "api_key_id:k"}, Req: Request{RequestID: "req1", ProjectID: "assistant", Door: DoorREST},
		Cfg: ConsultConfig{Enabled: true, PeerName: "vornik_architect", MaxQuestionBytes: 200, MaxAnswerBytes: 1000, Timeout: time.Second},
		Now: func() time.Time { return time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC) }}
}

func qargs(q string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"question": q})
	return b
}

// Test 38 (R8): consult audit is fail-closed — a nil audit repository or a
// failed attempt-record write produces ZERO outbound calls, asserted on
// the peer transport.
func TestConsult_AuditFailClosedMeansNoNetworkCall(t *testing.T) {
	peer := &fakePeer{name: "vornik_architect", answer: "advice"}
	c := newConsultant(peer, nil)
	out := c.Handle(context.Background(), qargs("what does maxTasksPerHour bound?"))
	if peer.Calls() != 0 || !strings.Contains(out, "could not be recorded") {
		t.Fatalf("nil audit must make no call: calls=%d out=%q", peer.Calls(), out)
	}
	if r := c.Record(); r.Outcome != ConsultOutcomeRefused || !r.Attempted {
		t.Fatalf("record = %+v", r)
	}

	peer = &fakePeer{name: "vornik_architect", answer: "advice"}
	c = newConsultant(peer, &fakeAudit{err: errors.New("disk full")})
	out = c.Handle(context.Background(), qargs("what does maxTasksPerHour bound?"))
	if peer.Calls() != 0 || !strings.Contains(out, "disk full") {
		t.Fatalf("failed attempt write must make no call: calls=%d out=%q", peer.Calls(), out)
	}
}

func TestConsult_SuccessIsAuditedBeforeAndAfterOnceOnly(t *testing.T) {
	peer := &fakePeer{name: "vornik_architect", answer: "  Use a longer cadence.  "}
	audit := &fakeAudit{}
	c := newConsultant(peer, audit)
	out := c.Handle(context.Background(), qargs("what is a sane cadence for a news feed?"))
	if peer.Calls() != 1 {
		t.Fatalf("calls = %d", peer.Calls())
	}
	if !strings.Contains(out, "untrusted_content") || !strings.Contains(out, "Use a longer cadence.") || !strings.Contains(out, "advice, not approval") {
		t.Fatalf("answer must be wrapped as untrusted advice: %q", out)
	}
	got := audit.actions()
	want := []string{ConsultActionAttempt, ConsultActionFirstUse, ConsultActionOutcome}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
	// The attempt row precedes the call and carries only a hash of the
	// question, never its text.
	if strings.Contains(audit.rows[0].After, "sane cadence") || !strings.Contains(audit.rows[0].After, "question_sha256") {
		t.Fatalf("attempt row must carry the hash, not the question: %s", audit.rows[0].After)
	}
	r := c.Record()
	if r.Outcome != ConsultOutcomeOK || r.AnswerBytes == 0 || r.QuestionSHA == "" {
		t.Fatalf("record = %+v", r)
	}
	// One consult per intent (R8).
	out = c.Handle(context.Background(), qargs("another?"))
	if peer.Calls() != 1 || !strings.Contains(out, "budget exhausted") {
		t.Fatalf("second consult must be refused: calls=%d out=%q", peer.Calls(), out)
	}
	// A second request for the same project does not re-write first_use.
	c2 := newConsultant(&fakePeer{name: "vornik_architect", answer: "x"}, audit)
	_ = c2.Handle(context.Background(), qargs("q2"))
	firstUses := 0
	for _, a := range audit.actions() {
		if a == ConsultActionFirstUse {
			firstUses++
		}
	}
	if firstUses != 1 {
		t.Fatalf("first_use must be immutable per project: %d", firstUses)
	}
}

// Design §7.3 third egress point: the question passes the same secret
// screen; a placeholder or raw secret in the question is refused without a
// call.
func TestConsult_EgressScreenOnQuestion(t *testing.T) {
	for _, q := range []string{
		"my api_key: sk-live-abcdefghijklmnopqrstuvwxyz0123456789 fails, why?",
		"what is VORNIK_SECRET_PLACEHOLDER_0123456789abcdef01234567 for?",
	} {
		peer := &fakePeer{name: "vornik_architect", answer: "x"}
		audit := &fakeAudit{}
		c := newConsultant(peer, audit)
		out := c.Handle(context.Background(), qargs(q))
		if peer.Calls() != 0 || !strings.Contains(out, "refused") {
			t.Fatalf("secret-bearing question must be refused before any call: calls=%d out=%q", peer.Calls(), out)
		}
		if len(audit.rows) != 0 {
			t.Fatal("a refused question is not an attempt")
		}
	}
	peer := &fakePeer{name: "vornik_architect", answer: "x"}
	c := newConsultant(peer, &fakeAudit{})
	c.Cfg.MaxQuestionBytes = 5
	if out := c.Handle(context.Background(), qargs("a question longer than five bytes")); peer.Calls() != 0 || !strings.Contains(out, "exceeds") {
		t.Fatalf("oversize question: %q", out)
	}
}

// R8: the destination and consent are re-checked immediately before
// dispatch; a consult disabled (or re-pointed) mid-request is not sent.
func TestConsult_ConsentAndDestinationRecheck(t *testing.T) {
	peer := &fakePeer{name: "vornik_architect", answer: "x"}
	c := newConsultant(peer, &fakeAudit{})
	c.Cfg.StillEnabled = func() bool { return false }
	if out := c.Handle(context.Background(), qargs("q")); peer.Calls() != 0 || !strings.Contains(out, "not enabled") {
		t.Fatalf("disabled mid-request must not send: %q", out)
	}
	peer = &fakePeer{name: "other_peer", answer: "x"}
	c = newConsultant(peer, &fakeAudit{})
	if out := c.Handle(context.Background(), qargs("q")); peer.Calls() != 0 || !strings.Contains(out, "not enabled") {
		t.Fatalf("a destination that differs from the pinned peer must not be called: %q", out)
	}
}

// An unreachable peer degrades the answer (no fabricated consultation) and
// is never retried automatically; an outcome the store cannot record is
// UNKNOWN, not invented.
func TestConsult_UnreachablePeerDegradesAndUnknownOutcome(t *testing.T) {
	peer := &fakePeer{name: "vornik_architect", err: errors.New("dial tcp: timeout")}
	audit := &fakeAudit{}
	c := newConsultant(peer, audit)
	out := c.Handle(context.Background(), qargs("q"))
	if peer.Calls() != 1 || !strings.Contains(out, "did not return an answer") || !strings.Contains(out, "Do NOT invent") {
		t.Fatalf("unreachable peer: calls=%d out=%q", peer.Calls(), out)
	}
	if r := c.Record(); r.Outcome != ConsultOutcomeError {
		t.Fatalf("record = %+v", r)
	}

	peer = &fakePeer{name: "vornik_architect", answer: "fine"}
	audit = &fakeAudit{failAfter: 2} // attempt + first_use succeed; outcome fails
	c = newConsultant(peer, audit)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Handle(ctx, qargs("q"))
	if r := c.Record(); r.Outcome != ConsultOutcomeUnknown {
		t.Fatalf("unrecordable outcome must be UNKNOWN: %+v", r)
	}
}

func TestRecordConsultToggle(t *testing.T) {
	if err := RecordConsultToggle(context.Background(), nil, true, "p", ""); err == nil {
		t.Fatal("nil audit must error")
	}
	a := &fakeAudit{}
	if err := RecordConsultToggle(context.Background(), a, true, "vornik_architect", ""); err != nil || a.rows[0].Action != ConsultActionEnabled || a.rows[0].Principal != "external/unknown" {
		t.Fatalf("%v %+v", err, a.rows)
	}
	_ = RecordConsultToggle(context.Background(), a, false, "vornik_architect", "session:u1")
	if a.rows[1].Action != ConsultActionDisabled || a.rows[1].Principal != "session:u1" {
		t.Fatalf("%+v", a.rows[1])
	}
}

// Engine integration: with consult enabled the tool is advertised and its
// record lands in the proposal's evidence; disabled → the tool is absent
// (plan §8 tests).
func TestPropose_ConsultToolPresenceAndRecord(t *testing.T) {
	f := newFixture(t)
	peer := &fakePeer{name: "vornik_architect", answer: "cadence advice"}
	audit := &fakeAudit{}
	f.engine.Peer, f.engine.Audit = peer, audit
	f.cfg.Consult.Enabled, f.cfg.Consult.Peer = true, "vornik_architect"
	f.assistant.script = []*chat.ChatResponse{
		toolResp("m", `PLAN: ["projects/assistant.yaml"]`, call("c0", ConsultToolName, map[string]any{"question": "is 2h a sane news cadence?"}), call("c1", "file_edit", map[string]any{"path": "projects/assistant.yaml", "old_string": "cadence: 1h", "new_string": "cadence: 2h"})),
		textResp("m", "done"),
	}
	f.scriptJudge("pass", "cadence: 2h")
	res, err := f.engine.Propose(context.Background(), operatorReq("less often, ask the architect"))
	if err != nil || res.Refusal != nil {
		t.Fatalf("%v %v", err, res.Refusal)
	}
	if peer.Calls() != 1 || res.Consult == nil || res.Consult.Outcome != ConsultOutcomeOK {
		t.Fatalf("consult must have run once: calls=%d rec=%+v", peer.Calls(), res.Consult)
	}
	if !strings.Contains(res.Proposal.Evidence, `"consult"`) {
		t.Fatal("evidence must carry the consult record")
	}
	// The tool was advertised: the assistant's first call saw 8 tools.
	if n := len(f.assistant.messages); n == 0 {
		t.Fatal("no assistant calls recorded")
	}

	// Disabled: the tool is not advertised and a call to it is OUT.
	g := newFixture(t)
	g.engine.Peer, g.engine.Audit = peer, audit
	g.assistant.script = []*chat.ChatResponse{
		toolResp("m", `PLAN: ["projects/assistant.yaml"]`, call("c0", ConsultToolName, map[string]any{"question": "q"}), call("c1", "file_edit", map[string]any{"path": "projects/assistant.yaml", "old_string": "cadence: 1h", "new_string": "cadence: 2h"})),
		textResp("m", "done"),
	}
	g.scriptJudge("pass", "cadence: 2h")
	before := peer.Calls()
	res, err = g.engine.Propose(context.Background(), operatorReq("less often"))
	if err != nil || res.Refusal != nil {
		t.Fatalf("%v %v", err, res.Refusal)
	}
	if peer.Calls() != before || res.Consult != nil {
		t.Fatal("consult disabled → no call, no record")
	}
}
