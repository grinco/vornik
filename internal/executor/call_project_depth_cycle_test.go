package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestWalkCallerLineage_RootTask: a task with no parent walks
// to depth 0 with just its own project in the ancestor set.
func TestWalkCallerLineage_RootTask(t *testing.T) {
	repo := NewMockTaskRepo()
	caller := &persistence.Task{ID: "t-root", ProjectID: "alpha"}
	info, err := walkCallerLineage(context.Background(), repo, caller)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if info.Depth != 0 {
		t.Errorf("Depth = %d, want 0", info.Depth)
	}
	if _, ok := info.AncestorProjects["alpha"]; !ok {
		t.Errorf("ancestor set should include caller project; got %v", info.AncestorProjects)
	}
	if len(info.LineagePath) != 1 || info.LineagePath[0] != "alpha" {
		t.Errorf("LineagePath = %v, want [alpha]", info.LineagePath)
	}
}

// TestWalkCallerLineage_CrossProjectHopsCount: a chain of
// in-project + cross-project parents bumps Depth only on
// project-boundary crossings.
func TestWalkCallerLineage_CrossProjectHopsCount(t *testing.T) {
	repo := NewMockTaskRepo()
	// Build the chain root → mid1 (alpha) → mid2 (alpha) → caller (beta)
	// Two same-project parents shouldn't bump depth; the alpha→beta
	// crossing is one hop.
	root := &persistence.Task{ID: "t-root", ProjectID: "alpha"}
	mid1 := &persistence.Task{ID: "t-mid1", ProjectID: "alpha", ParentTaskID: ptrStr("t-root")}
	mid2 := &persistence.Task{ID: "t-mid2", ProjectID: "alpha", ParentTaskID: ptrStr("t-mid1")}
	caller := &persistence.Task{ID: "t-caller", ProjectID: "beta", ParentTaskID: ptrStr("t-mid2")}
	for _, x := range []*persistence.Task{root, mid1, mid2, caller} {
		_ = repo.Create(context.Background(), x)
	}

	info, _ := walkCallerLineage(context.Background(), repo, caller)
	if info.Depth != 1 {
		t.Errorf("Depth = %d, want 1 (single alpha→beta crossing)", info.Depth)
	}
	if _, ok := info.AncestorProjects["alpha"]; !ok {
		t.Errorf("alpha should be in ancestor set")
	}
	if _, ok := info.AncestorProjects["beta"]; !ok {
		t.Errorf("beta should be in ancestor set")
	}
}

// TestWalkCallerLineage_OrphanParentTerminates: a parent_task_id
// pointing at a deleted row terminates the walk gracefully.
func TestWalkCallerLineage_OrphanParentTerminates(t *testing.T) {
	repo := NewMockTaskRepo()
	caller := &persistence.Task{ID: "t-caller", ProjectID: "alpha", ParentTaskID: ptrStr("t-deleted")}
	info, err := walkCallerLineage(context.Background(), repo, caller)
	if err != nil {
		t.Fatalf("walk should tolerate missing parent: %v", err)
	}
	if info.Depth != 0 {
		t.Errorf("Depth should stay 0 — no parent rows reachable; got %d", info.Depth)
	}
}

// TestCallProject_DepthExceeded_RefusesAndAudits builds a chain
// past the project's MaxCallDepth and confirms the next hop is
// refused with DEPTH_EXCEEDED + an audit row.
func TestCallProject_DepthExceeded_RefusesAndAudits(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	// Caller project explicitly caps at 2 so we don't have to
	// build a chain of 8 to exercise the refusal.
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"alpha": {ID: "alpha", MaxCallDepth: 2},
			"beta":  {ID: "beta", AcceptCallsFrom: []string{"alpha", "gamma"}},
			"gamma": {ID: "gamma", AcceptCallsFrom: []string{"alpha", "beta"}},
		},
	}
	e, tr := newCallProjectExecutor(resolver, cpc)
	audit := &stubAdminAudit{}
	e.adminAuditRepo = audit

	// Build alpha → beta → gamma → caller. Depth = 3 hops. Cap = 2.
	// One more hop would push to 4. Should refuse.
	root := &persistence.Task{ID: "t0", ProjectID: "alpha"}
	hop1 := &persistence.Task{ID: "t1", ProjectID: "beta", ParentTaskID: ptrStr("t0")}
	hop2 := &persistence.Task{ID: "t2", ProjectID: "gamma", ParentTaskID: ptrStr("t1")}
	caller := &persistence.Task{ID: "t3", ProjectID: "alpha", ParentTaskID: ptrStr("t2")}
	for _, x := range []*persistence.Task{root, hop1, hop2, caller} {
		_ = tr.Create(context.Background(), x)
	}

	step := &registry.WorkflowStep{
		Type:           "call_project",
		TargetProject:  "beta",
		TargetWorkflow: "any",
		Payload:        map[string]any{},
	}
	exec := &persistence.Execution{ID: "exec-depth"}

	_, err := e.handleCallProjectStep(context.Background(), caller, exec, "step-x", step, nil)
	if err == nil {
		t.Fatal("expected DEPTH_EXCEEDED error")
	}
	if !strings.Contains(err.Error(), "DEPTH_EXCEEDED") {
		t.Fatalf("error should mention DEPTH_EXCEEDED: %v", err)
	}
	if len(cpc.rows) != 0 {
		t.Errorf("no CPC row should be created on depth-exceeded; got %d", len(cpc.rows))
	}
	if !audit.hasAction(auditActionCPCDepthExceeded) {
		t.Errorf("expected audit row %q; actions=%v", auditActionCPCDepthExceeded, audit.actions())
	}
}

// TestCallProject_DefaultDepthCap_Allows uses the default cap (8)
// and pin that a depth-1 hop from a root task is allowed.
func TestCallProject_DefaultDepthCap_Allows(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"alpha": {ID: "alpha"}, // no override → default 8
			"beta":  {ID: "beta", AcceptCallsFrom: []string{"alpha"}},
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)
	caller := &persistence.Task{ID: "tA", ProjectID: "alpha"}
	step := &registry.WorkflowStep{
		Type: "call_project", TargetProject: "beta", TargetWorkflow: "wf",
	}
	exec := &persistence.Execution{ID: "exec-default"}
	if _, err := e.handleCallProjectStep(context.Background(), caller, exec, "s", step, nil); err != nil {
		t.Fatalf("call should succeed under default cap: %v", err)
	}
	if len(cpc.rows) != 1 {
		t.Errorf("CPC row should be created; got %d", len(cpc.rows))
	}
}

// TestCallProject_CycleDetected_RefusesAndAudits builds a chain
// where the proposed callee is already an ancestor of the
// caller. Refusal with CYCLE_DETECTED + audit row.
func TestCallProject_CycleDetected_RefusesAndAudits(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			// Trust-each-other pair: both accept calls from the
			// other. This is exactly the scenario the cycle
			// guard is designed for — without it the
			// acceptCallsFrom layer would happily let A→B→A
			// loop until per-call timeouts sweep.
			"alpha": {ID: "alpha", AcceptCallsFrom: []string{"beta"}},
			"beta":  {ID: "beta", AcceptCallsFrom: []string{"alpha"}},
		},
	}
	e, tr := newCallProjectExecutor(resolver, cpc)
	audit := &stubAdminAudit{}
	e.adminAuditRepo = audit

	// alpha → beta → caller-in-beta. Now beta tries to call back
	// into alpha — alpha is already an ancestor.
	root := &persistence.Task{ID: "t0", ProjectID: "alpha"}
	hop := &persistence.Task{ID: "t1", ProjectID: "beta", ParentTaskID: ptrStr("t0")}
	caller := &persistence.Task{ID: "t2", ProjectID: "beta", ParentTaskID: ptrStr("t1")}
	for _, x := range []*persistence.Task{root, hop, caller} {
		_ = tr.Create(context.Background(), x)
	}

	step := &registry.WorkflowStep{
		Type:           "call_project",
		TargetProject:  "alpha",
		TargetWorkflow: "any",
	}
	exec := &persistence.Execution{ID: "exec-cycle"}
	_, err := e.handleCallProjectStep(context.Background(), caller, exec, "s", step, nil)
	if err == nil {
		t.Fatal("expected CYCLE_DETECTED")
	}
	if !strings.Contains(err.Error(), "CYCLE_DETECTED") {
		t.Fatalf("error should mention CYCLE_DETECTED: %v", err)
	}
	if len(cpc.rows) != 0 {
		t.Errorf("no CPC row should be created on cycle; got %d", len(cpc.rows))
	}
	if !audit.hasAction(auditActionCPCCycleDetected) {
		t.Errorf("expected audit row %q; actions=%v", auditActionCPCCycleDetected, audit.actions())
	}
}

// TestCallProject_CalleePayloadCarriesDepth pins that the
// callee task's payload carries context.callDepth = depth+1,
// so future hops can read it directly.
func TestCallProject_CalleePayloadCarriesDepth(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"alpha": {ID: "alpha"},
			"beta":  {ID: "beta", AcceptCallsFrom: []string{"alpha"}},
		},
	}
	e, tr := newCallProjectExecutor(resolver, cpc)
	caller := &persistence.Task{ID: "t-caller", ProjectID: "alpha"}
	step := &registry.WorkflowStep{
		Type: "call_project", TargetProject: "beta", TargetWorkflow: "wf",
	}
	exec := &persistence.Execution{ID: "exec-depth-payload"}
	res, err := e.handleCallProjectStep(context.Background(), caller, exec, "s", step, nil)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	callee, _ := tr.Get(context.Background(), res.CalleeTaskID)
	if callee == nil {
		t.Fatal("callee task missing")
	}
	var parsed map[string]any
	if err := json.Unmarshal(callee.Payload, &parsed); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	ctxBlock, _ := parsed["context"].(map[string]any)
	depthVal, _ := ctxBlock["callDepth"].(float64) // json.Unmarshal makes numbers float64
	if int(depthVal) != 1 {
		t.Errorf("callDepth = %v, want 1", depthVal)
	}
}

// TestReadCarriedCallDepth pins the chain-depth-header reader:
// it extracts context.callDepth from a task payload, defaulting
// to 0 for absent / malformed / non-numeric values so a missing
// or corrupt header can never *lower* the effective depth (the
// guard takes the max of walked + carried).
func TestReadCarriedCallDepth(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    int
	}{
		{"nil payload", nil, 0},
		{"empty object", []byte(`{}`), 0},
		{"no context block", []byte(`{"args":{}}`), 0},
		{"context without callDepth", []byte(`{"context":{"prompt":"x"}}`), 0},
		{"explicit depth", buildCalleePayload([]byte(`{}`), 5), 5},
		{"malformed json", []byte(`not json`), 0},
		{"non-numeric depth", []byte(`{"context":{"callDepth":"nope"}}`), 0},
		{"negative depth clamped", []byte(`{"context":{"callDepth":-3}}`), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readCarriedCallDepth(&persistence.Task{Payload: tc.payload}); got != tc.want {
				t.Errorf("readCarriedCallDepth = %d, want %d", got, tc.want)
			}
		})
	}
	if got := readCarriedCallDepth(nil); got != 0 {
		t.Errorf("nil task: readCarriedCallDepth = %d, want 0", got)
	}
}

// TestCallProject_CarriedDepthBackstopsTruncatedLineage pins the
// chain-depth-header backstop. When the stored lineage walk can't
// see the full chain — an ancestor row was deleted (archive sweep,
// manual cleanup) or the walk truncated at lineageWalkHardLimit —
// the depth meter would silently reset to 0 and let a runaway
// chain continue. The guard now also reads the callDepth carried
// on the caller's own payload and takes the max, so a broken
// lineage can't defeat the cap.
func TestCallProject_CarriedDepthBackstopsTruncatedLineage(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"alpha": {ID: "alpha"}, // default cap 8
			"beta":  {ID: "beta", AcceptCallsFrom: []string{"alpha"}},
		},
	}
	e, tr := newCallProjectExecutor(resolver, cpc)
	audit := &stubAdminAudit{}
	e.adminAuditRepo = audit

	// Caller's parent row is gone, so walkCallerLineage sees only
	// Depth 0. But the caller's payload carries callDepth=8 from the
	// hop that created it — the chain is really 8 deep. cap=8, so the
	// next hop must be refused via the carried-depth backstop.
	caller := &persistence.Task{
		ID: "t-caller", ProjectID: "alpha",
		ParentTaskID: ptrStr("t-deleted-ancestor"),
		Payload:      buildCalleePayload([]byte(`{}`), 8),
	}
	_ = tr.Create(context.Background(), caller)

	step := &registry.WorkflowStep{
		Type: "call_project", TargetProject: "beta", TargetWorkflow: "wf",
	}
	exec := &persistence.Execution{ID: "exec-carry"}
	_, err := e.handleCallProjectStep(context.Background(), caller, exec, "s", step, nil)
	if err == nil {
		t.Fatal("expected DEPTH_EXCEEDED via carried-depth backstop")
	}
	if !strings.Contains(err.Error(), "DEPTH_EXCEEDED") {
		t.Fatalf("error should mention DEPTH_EXCEEDED: %v", err)
	}
	if len(cpc.rows) != 0 {
		t.Errorf("no CPC row should be created; got %d", len(cpc.rows))
	}
	if !audit.hasAction(auditActionCPCDepthExceeded) {
		t.Errorf("expected audit row %q; actions=%v", auditActionCPCDepthExceeded, audit.actions())
	}
}

// TestEffectiveMaxCallDepth_DefaultsAndOverrides pin the
// registry helper the call-path reads.
func TestEffectiveMaxCallDepth_DefaultsAndOverrides(t *testing.T) {
	cases := []struct {
		name string
		p    *registry.Project
		want int
	}{
		{"nil", nil, registry.DefaultMaxCallDepth},
		{"zero", &registry.Project{ID: "x"}, registry.DefaultMaxCallDepth},
		{"negative", &registry.Project{ID: "x", MaxCallDepth: -5}, registry.DefaultMaxCallDepth},
		{"explicit", &registry.Project{ID: "x", MaxCallDepth: 3}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.EffectiveMaxCallDepth(); got != tc.want {
				t.Errorf("EffectiveMaxCallDepth = %d, want %d", got, tc.want)
			}
		})
	}
}

// stubAdminAudit captures every Insert for assertion convenience.
// Kept local to this file so the broader test fixtures stay
// untouched.
type stubAdminAudit struct {
	rows []*persistence.AdminAuditEntry
}

func (s *stubAdminAudit) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	cp := *e
	s.rows = append(s.rows, &cp)
	return nil
}

func (s *stubAdminAudit) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

func (s *stubAdminAudit) hasAction(action string) bool {
	for _, r := range s.rows {
		if r.Action == action {
			return true
		}
	}
	return false
}

func (s *stubAdminAudit) actions() []string {
	out := make([]string, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r.Action)
	}
	return out
}

// ptrStr returns a pointer to the given string. Used for the
// ParentTaskID columns which the schema models as *string so the
// loader can distinguish NULL from "".
func ptrStr(s string) *string { return &s }
