package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/fixitdoctor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

type fakeFixItSessions struct {
	rows map[string]*persistence.FixItSession
}

func newFakeFixItSessions() *fakeFixItSessions {
	return &fakeFixItSessions{rows: map[string]*persistence.FixItSession{}}
}

func (f *fakeFixItSessions) Insert(_ context.Context, s *persistence.FixItSession) error {
	f.rows[s.ID] = s
	return nil
}
func (f *fakeFixItSessions) Get(_ context.Context, id string) (*persistence.FixItSession, error) {
	r, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return r, nil
}
func (f *fakeFixItSessions) Update(_ context.Context, s *persistence.FixItSession) error {
	f.rows[s.ID] = s
	return nil
}
func (f *fakeFixItSessions) Close(_ context.Context, _, _ string) error { return nil }
func (f *fakeFixItSessions) ListByOperator(_ context.Context, _ string, _ int) ([]*persistence.FixItSession, error) {
	return nil, nil
}
func (f *fakeFixItSessions) CascadeCloseByFailureRef(_ context.Context, _, _ string) (int, error) {
	return 0, nil
}

func TestBuildFixItDoctorOrNil_NilWhenUnwired(t *testing.T) {
	if got := buildFixItDoctorOrNil(nil, nil); got != nil {
		t.Fatalf("expected nil for a nil container")
	}
	if got := buildFixItDoctorOrNil(&Container{}, nil); got != nil {
		t.Fatalf("expected nil with no repos/chat client wired")
	}
	c := &Container{repos: &storage.Repositories{FixItSessions: newFakeFixItSessions()}}
	if got := buildFixItDoctorOrNil(c, nil); got != nil {
		t.Fatalf("expected nil with no chat client wired")
	}
}

func TestBuildFixItDoctorOrNil_BuildsWhenWired(t *testing.T) {
	c := &Container{
		Logger:     zerolog.Nop(),
		Config:     &config.Config{},
		ChatClient: &fakeModelListingProvider{},
		repos:      &storage.Repositories{FixItSessions: newFakeFixItSessions()},
	}
	got := buildFixItDoctorOrNil(c, nil)
	if got == nil {
		t.Fatal("expected a non-nil adapter")
	}
	adapter, ok := got.(*fixItDoctorAdapter)
	if !ok {
		t.Fatalf("expected *fixItDoctorAdapter, got %T", got)
	}
	if adapter.svc.Sessions == nil || adapter.svc.Assembler == nil || adapter.svc.Chat == nil {
		t.Fatalf("expected core deps wired, got %+v", adapter.svc)
	}
	if adapter.svc.MaxActiveSessions != 5 || adapter.svc.MaxTurns != 20 {
		t.Fatalf("expected default caps, got %+v", adapter.svc)
	}
	// Task 3.3: the gate pipeline is unconditionally wired (fails closed
	// per-key via findGateFeature when a key isn't registered), even
	// with no projectDoctor/proposals/tasks/audit repo present.
	if adapter.svc.GatePipeline == nil {
		t.Fatalf("expected GatePipeline to be wired even with a minimal container")
	}
	if adapter.svc.SecretSetter != nil {
		t.Fatalf("expected SecretSetter nil when projectDoctor is nil")
	}
	// Task 3.4: IntegrationProbes/ReloadStatus are unconditionally wired
	// (against *Container.uiServer, read lazily) — task 3.1 left them
	// nil; a nil c.uiServer now degrades the adapters themselves to
	// fail-closed at call time rather than leaving the Assembler fields
	// nil at construction time.
	if adapter.svc.Assembler.IntegrationProbes == nil {
		t.Fatal("expected Assembler.IntegrationProbes wired (task 3.4)")
	}
	if adapter.svc.Assembler.ReloadStatus == nil {
		t.Fatal("expected Assembler.ReloadStatus wired (task 3.4)")
	}
}

func TestResolveFixItDoctorModel(t *testing.T) {
	if got := resolveFixItDoctorModel(nil); got != "" {
		t.Fatalf("expected empty for nil config, got %q", got)
	}
	cfg := &config.Config{}
	cfg.Chat.FixItModel = "  gpt-mid  "
	if got := resolveFixItDoctorModel(cfg); got != "gpt-mid" {
		t.Fatalf("expected trimmed model, got %q", got)
	}
}

func TestFixItDoctorAdapter_SessionScope(t *testing.T) {
	sessions := newFakeFixItSessions()
	sessions.rows["fix-1"] = &persistence.FixItSession{ID: "fix-1", OperatorID: "op1", ProjectID: "proj-a"}
	svc := &fixitdoctor.Service{Sessions: sessions, Assembler: &fixitdoctor.Assembler{}, Chat: &fakeModelListingProvider{}}
	adapter := newFixItDoctorAdapter(svc).(*fixItDoctorAdapter)

	proj, ok, err := adapter.SessionScope(context.Background(), "fix-1", "op1")
	if err != nil || !ok || proj != "proj-a" {
		t.Fatalf("expected (proj-a, true, nil), got (%q, %v, %v)", proj, ok, err)
	}

	_, ok, err = adapter.SessionScope(context.Background(), "fix-1", "op-other")
	if err != nil || ok {
		t.Fatalf("expected not-ok for a foreign operator, got ok=%v err=%v", ok, err)
	}

	_, ok, err = adapter.SessionScope(context.Background(), "ghost", "op1")
	if err != nil || ok {
		t.Fatalf("expected not-ok for a missing session, got ok=%v err=%v", ok, err)
	}
}

func TestFixItDoctorAdapter_Converse_PropagatesRequiredRefError(t *testing.T) {
	svc := &fixitdoctor.Service{Sessions: newFakeFixItSessions(), Assembler: &fixitdoctor.Assembler{}, Chat: &fakeModelListingProvider{}}
	adapter := newFixItDoctorAdapter(svc)
	_, err := adapter.Converse(context.Background(), "", "op1", "", "", "", "help")
	if !errors.Is(err, fixitdoctor.ErrFailureRefRequired) {
		t.Fatalf("expected ErrFailureRefRequired, got %v", err)
	}
}

func TestNewFixItDoctorAdapter_NilServiceReturnsNilInterface(t *testing.T) {
	if got := newFixItDoctorAdapter(nil); got != nil {
		t.Fatalf("expected a nil interface for a nil service")
	}
}

func TestFixItDoctorAdapter_Apply_PropagatesResult(t *testing.T) {
	sessions := newFakeFixItSessions()
	env := fixitdoctor.FixItEnvelope{Actions: []fixitdoctor.ProposedAction{
		{Kind: fixitdoctor.ActionKindRetryTask, Params: map[string]string{"task_id": "t1"}},
	}}
	envJSON, _ := json.Marshal(env)
	sessions.rows["fix-1"] = &persistence.FixItSession{ID: "fix-1", OperatorID: "op1", LastEnvelope: envJSON}
	svc := &fixitdoctor.Service{
		Sessions:          sessions,
		Metrics:           fixitdoctor.NewMetrics(prometheus.NewRegistry()),
		ActionTaskRetrier: fakeTaskRetrierAdapter{detail: "requeued"},
	}
	adapter := newFixItDoctorAdapter(svc).(*fixItDoctorAdapter)

	res, err := adapter.Apply(context.Background(), "fix-1", "op1", 0, "")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Result != fixitdoctor.ActionResultApplied || res.Detail != "requeued" {
		t.Fatalf("unexpected result: %+v", res)
	}

	if _, err := adapter.Apply(context.Background(), "ghost", "op1", 0, ""); err == nil {
		t.Fatal("expected an error for an unknown session")
	}
}

func TestFixItDoctorAdapter_Rollback_PropagatesResult(t *testing.T) {
	sessions := newFakeFixItSessions()
	// Seed an applied config_apply for cpp_1 so it passes the rollback-ownership
	// gate (fixitdoctor: rollback must target a proposal THIS session applied) —
	// this test exercises result PROPAGATION, not the gate itself.
	sessions.rows["fix-1"] = &persistence.FixItSession{
		ID: "fix-1", OperatorID: "op1",
		AppliedActions: []byte(`[{"kind":"config_apply","result":"applied","rollback_id":"cpp_1"}]`),
	}
	svc := &fixitdoctor.Service{
		Sessions:        sessions,
		Metrics:         fixitdoctor.NewMetrics(prometheus.NewRegistry()),
		ConfigProposals: fakeConfigProposalPipelineAdapter{},
	}
	adapter := newFixItDoctorAdapter(svc).(*fixItDoctorAdapter)

	res, err := adapter.Rollback(context.Background(), "fix-1", "op1", "cpp_1")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res.Result != fixitdoctor.ActionResultApplied || res.RollbackID != "cpp_1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// fakeTaskRetrierAdapter / fakeConfigProposalPipelineAdapter are minimal
// fixitdoctor.TaskRetrier / ConfigProposalPipeline fakes local to this
// file — the dispatcher's own routing is covered exhaustively in
// internal/fixitdoctor/dispatch_test.go; these only prove the adapter
// wiring/translation layer end to end.
type fakeTaskRetrierAdapter struct{ detail string }

func (f fakeTaskRetrierAdapter) Retry(context.Context, string, string) (string, error) {
	return f.detail, nil
}

type fakeConfigProposalPipelineAdapter struct{}

func (fakeConfigProposalPipelineAdapter) File(context.Context, string, string, string) (string, string, error) {
	return "cpp_1", "diff", nil
}
func (fakeConfigProposalPipelineAdapter) Apply(context.Context, string, string) error { return nil }
func (fakeConfigProposalPipelineAdapter) Rollback(context.Context, string) error      { return nil }

func TestToAPIFixItResult(t *testing.T) {
	if got := toAPIFixItResult(nil); got != nil {
		t.Fatalf("expected nil for a nil result")
	}
	res := &fixitdoctor.Result{
		SessionID: "fix-1",
		Envelope: &fixitdoctor.FixItEnvelope{
			Message:  "here's a plan",
			Resolved: true,
			Actions: []fixitdoctor.ProposedAction{
				{Kind: fixitdoctor.ActionKindLinkOut, Label: "open it", Params: map[string]string{"url": "/ui/x"}},
			},
		},
		StatusPoll: &fixitdoctor.StatusPollResult{Summary: "task status: COMPLETED", Healthy: true},
	}
	got := toAPIFixItResult(res)
	if got.SessionID != "fix-1" || got.Envelope.Message != "here's a plan" || !got.Envelope.Resolved {
		t.Fatalf("unexpected mapping: %+v", got)
	}
	if len(got.Envelope.Actions) != 1 || got.Envelope.Actions[0].Kind != "link_out" || got.Envelope.Actions[0].Params["url"] != "/ui/x" {
		t.Fatalf("unexpected action mapping: %+v", got.Envelope.Actions)
	}
	if got.StatusPoll == nil || !got.StatusPoll.Healthy || got.StatusPoll.Summary != "task status: COMPLETED" {
		t.Fatalf("unexpected status poll mapping: %+v", got.StatusPoll)
	}
}
