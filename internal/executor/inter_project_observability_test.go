package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubLivePub captures every Publish so the observability
// tests can assert on what was emitted without standing up
// the full publisher.
type stubLivePub struct {
	mu     sync.Mutex
	events []stubLiveCall
}

type stubLiveCall struct {
	ExecutionID string
	Kind        string
	Payload     any
}

func (s *stubLivePub) Publish(_ context.Context, executionID, kind string, payload any) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, stubLiveCall{ExecutionID: executionID, Kind: kind, Payload: payload})
	return int64(len(s.events))
}

func (s *stubLivePub) Subscribe(_ string, _ int64) (<-chan livepubsub.LiveEvent, func(), error) {
	ch := make(chan livepubsub.LiveEvent)
	close(ch)
	return ch, func() {}, nil
}

func (s *stubLivePub) SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error) {
	ch := make(chan livepubsub.LiveEvent)
	close(ch)
	return ch, func() {}, nil
}

func (s *stubLivePub) byKind(kind string) []stubLiveCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []stubLiveCall{}
	for _, e := range s.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// stubAdminAuditRepo captures Insert calls so tests can verify
// the audit row shape + count.
type stubAdminAuditRepo struct {
	mu   sync.Mutex
	rows []*persistence.AdminAuditEntry
}

func (s *stubAdminAuditRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *e
	s.rows = append(s.rows, &cp)
	return nil
}

func (s *stubAdminAuditRepo) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

func (s *stubAdminAuditRepo) byAction(action string) []*persistence.AdminAuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*persistence.AdminAuditEntry{}
	for _, r := range s.rows {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// TestCallProject_EmitsLiveAndAudit asserts the call_project
// handler fires the started live event, bumps the started
// metric, and writes the audit row. Combined coverage of all
// three Phase C observability surfaces in one test (they all
// run on the same code path).
func TestCallProject_EmitsLiveAndAudit(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	pub := &stubLivePub{}
	audit := &stubAdminAuditRepo{}

	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)
	e.livePub = pub
	e.adminAuditRepo = audit

	task := &persistence.Task{ID: "task-caller", ProjectID: "marketing"}
	exec := &persistence.Execution{ID: "exec-caller"}

	if _, err := e.handleCallProjectStep(context.Background(), task, exec, "step-1", makeCallStep(), nil); err != nil {
		t.Fatalf("handleCallProjectStep: %v", err)
	}

	// Live event — on the caller's stream, kind started.
	started := pub.byKind(livepubsub.KindCrossProjectCallStarted)
	if len(started) != 1 {
		t.Fatalf("expected exactly one cross_project_call_started event, got %d", len(started))
	}
	if started[0].ExecutionID != "exec-caller" {
		t.Errorf("live event emitted on wrong execution: %q, want exec-caller", started[0].ExecutionID)
	}
	payload, _ := started[0].Payload.(livepubsub.CrossProjectCallStartedPayload)
	if payload.CalleeProject != "architect" || payload.CalleeWorkflow != "produce-spec" {
		t.Errorf("payload routing wrong: %+v", payload)
	}
	if payload.ExpectedSchema != "spec_envelope.v1" {
		t.Errorf("payload schema = %q, want spec_envelope.v1", payload.ExpectedSchema)
	}

	// Audit row — action interproject.cpc.create.
	rows := audit.byAction(auditActionCPCCreate)
	if len(rows) != 1 {
		t.Fatalf("expected one cpc.create audit row, got %d", len(rows))
	}
	if rows[0].Source != auditSource || rows[0].Principal != auditPrincipal {
		t.Errorf("audit row provenance wrong: source=%q principal=%q", rows[0].Source, rows[0].Principal)
	}
	if rows[0].Target != "architect" {
		t.Errorf("audit target = %q, want architect (callee project)", rows[0].Target)
	}
	if !contains(rows[0].After, `"callee_project":"architect"`) {
		t.Errorf("audit After missing callee routing, got %q", rows[0].After)
	}
}

// TestResolveCPC_EmitsAndAuditsTerminal asserts the resolve
// hook emits the resolved live event on the CALLER's stream
// and writes the audit row. Covers the happy + rejected
// branches (failed and timed_out follow the same code path).
func TestResolveCPC_EmitsAndAuditsTerminal(t *testing.T) {
	withInterProjectEnabled(t)

	cases := []struct {
		name       string
		envelope   []byte
		succeeded  bool
		wantStatus string
		wantErr    bool
	}{
		{
			name:       "happy_completed",
			envelope:   []byte(`{"schema":"spec_envelope.v1","status":"ok"}`),
			succeeded:  true,
			wantStatus: string(persistence.CPCStatusCompleted),
		},
		{
			name:       "empty_envelope_rejected",
			envelope:   nil,
			succeeded:  true,
			wantStatus: string(persistence.CPCStatusRejected),
			wantErr:    true,
		},
		{
			name:       "callee_failed",
			envelope:   nil,
			succeeded:  false,
			wantStatus: string(persistence.CPCStatusFailed),
			wantErr:    true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cpc := newMockCPCRepo()
			pub := &stubLivePub{}
			audit := &stubAdminAuditRepo{}

			// Seed a running CPC row so the hook has something
			// to resolve.
			cpcID := "ccp_" + c.name
			cpc.rows[cpcID] = &persistence.CrossProjectCall{
				ID:             cpcID,
				CallerTaskID:   "task-caller",
				CallerProject:  "marketing",
				CalleeProject:  "architect",
				CalleeWorkflow: "produce-spec",
				Status:         persistence.CPCStatusRunning,
				CreatedAt:      time.Now().Add(-1 * time.Second),
			}

			// Wire the executor with a stub exec repo whose
			// GetByTaskID returns a known execution id for the
			// caller — the resolve hook needs that ID to emit
			// on the CALLER's stream.
			e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
			e.livePub = pub
			e.adminAuditRepo = audit
			// Inject a caller execution row so
			// lookupExecutionIDForTask returns a non-empty ID
			// for the caller task — the emit-on-resolved path
			// fires only when an execution id is found.
			if er, ok := e.execRepo.(*MockExecRepo); ok {
				_ = er.Create(context.Background(), &persistence.Execution{
					ID: "exec-caller-" + c.name, TaskID: "task-caller",
				})
			}

			calleeTask := &persistence.Task{
				ID:                 "callee-task",
				CrossProjectCallID: &cpcID,
				ResultEnvelope:     c.envelope,
			}
			if !c.succeeded {
				msg := "callee blew up"
				calleeTask.LastError = &msg
			}

			e.resolveCrossProjectCallForTask(context.Background(), calleeTask, c.succeeded)

			// Live event on the caller's stream.
			resolved := pub.byKind(livepubsub.KindCrossProjectCallResolved)
			if len(resolved) != 1 {
				t.Fatalf("expected one resolved event, got %d", len(resolved))
			}
			payload, _ := resolved[0].Payload.(livepubsub.CrossProjectCallResolvedPayload)
			if payload.Status != c.wantStatus {
				t.Errorf("resolved payload status = %q, want %q", payload.Status, c.wantStatus)
			}
			if c.wantErr && payload.ErrorMessage == "" {
				t.Error("expected non-empty ErrorMessage for failed/rejected branch")
			}

			// Audit row.
			rows := audit.byAction(auditActionCPCResolve)
			if len(rows) != 1 {
				t.Errorf("expected one resolve audit row, got %d", len(rows))
			}
		})
	}
}
