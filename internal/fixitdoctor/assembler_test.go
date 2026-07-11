package fixitdoctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/persistence"
)

// --- fakes -----------------------------------------------------------

type fakeTaskRepo struct {
	persistence.TaskRepository
	tasks map[string]*persistence.Task
}

func (f *fakeTaskRepo) Get(_ context.Context, id string) (*persistence.Task, error) {
	t, ok := f.tasks[id]
	if !ok {
		// persistence.ErrNotFound (not a bare error) so callers that
		// need to distinguish "gone" from "some other failure" — e.g.
		// fixitdoctor.Service's cascade-close check — can errors.Is
		// against it, matching how the real repositories behave.
		return nil, persistence.ErrNotFound
	}
	return t, nil
}

type fakeExecutionRepo struct {
	persistence.ExecutionRepository
	byTask map[string]*persistence.Execution
}

func (f *fakeExecutionRepo) GetByTaskID(_ context.Context, taskID string) (*persistence.Execution, error) {
	e, ok := f.byTask[taskID]
	if !ok {
		return nil, errors.New("not found")
	}
	return e, nil
}

type fakeStepOutcomeRepo struct {
	persistence.ExecutionStepOutcomeRepository
	byExecution map[string][]*persistence.ExecutionStepOutcome
	err         error
}

func (f *fakeStepOutcomeRepo) List(_ context.Context, filter persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	if f.err != nil {
		return nil, f.err
	}
	if filter.ExecutionID == nil {
		return nil, nil
	}
	rows := f.byExecution[*filter.ExecutionID]
	if filter.PageSize > 0 && len(rows) > filter.PageSize {
		rows = rows[:filter.PageSize]
	}
	return rows, nil
}

type fakeNarrationRepo struct {
	persistence.ExecutionNarrationRepository
	byExecution map[string][]*persistence.ExecutionNarration
	err         error
}

func (f *fakeNarrationRepo) ListByExecution(_ context.Context, executionID string) ([]*persistence.ExecutionNarration, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byExecution[executionID], nil
}

type fakeIntegrationProbes struct {
	result integrations.ProbeResult
	docURL string
	ok     bool
	err    error
}

func (f *fakeIntegrationProbes) LatestProbe(_ context.Context, _ FailureRef) (integrations.ProbeResult, string, bool, error) {
	return f.result, f.docURL, f.ok, f.err
}

type fakeReloadStatus struct {
	rv  ReloadValidationError
	ok  bool
	err error
}

func (f *fakeReloadStatus) LatestReloadError(_ context.Context, _ FailureRef) (ReloadValidationError, bool, error) {
	return f.rv, f.ok, f.err
}

func strPtr(s string) *string { return &s }

type fakeInstinctRepo struct {
	rows []*persistence.Instinct
	err  error
}

// List returns rows matching filter.Status, mirroring the real
// repository closely enough for LearnedRemediations' per-status merge
// (it queries active then promoted) to not double-count a fixture row.
func (f *fakeInstinctRepo) List(_ context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*persistence.Instinct
	for _, r := range f.rows {
		if filter.Status != nil && r.Status != *filter.Status {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// --- failed_task -------------------------------------------------------

func TestAssemble_FailedTask_BundleShape(t *testing.T) {
	task := &persistence.Task{
		ID:             "task-1",
		ProjectID:      "proj-1",
		LastErrorClass: strPtr(persistence.TaskFailureClassToolError),
	}
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1"}
	outcomes := []*persistence.ExecutionStepOutcome{
		{StepID: "step-a", Role: "coder", Outcome: "failed", ErrorClass: persistence.TaskFailureClassToolError, ErrorDetail: "curl exit 7"},
	}

	a := &Assembler{
		Tasks:        &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Executions:   &fakeExecutionRepo{byTask: map[string]*persistence.Execution{"task-1": exec}},
		StepOutcomes: &fakeStepOutcomeRepo{byExecution: map[string][]*persistence.ExecutionStepOutcome{"exec-1": outcomes}},
	}

	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1", ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if bundle.Kind != FailureKindFailedTask || bundle.FailedTask == nil {
		t.Fatalf("expected a populated FailedTask bundle, got %+v", bundle)
	}
	ft := bundle.FailedTask
	if ft.ErrorClass.Value != persistence.TaskFailureClassToolError {
		t.Errorf("expected error class %q, got %q", persistence.TaskFailureClassToolError, ft.ErrorClass.Value)
	}
	if ft.ErrorClass.Untrusted {
		t.Errorf("error class is a controlled classifier vocabulary, expected trusted")
	}
	if ft.Cause.Value == "" || ft.HumanMessage.Value == "" {
		t.Errorf("expected non-empty playbook cause/human message, got %+v", ft)
	}
	if len(ft.Suggestions) == 0 {
		t.Errorf("expected at least one suggestion from the playbook corpus")
	}
	if len(ft.StepOutcomes) != 1 {
		t.Fatalf("expected 1 step outcome row, got %d", len(ft.StepOutcomes))
	}
	row := ft.StepOutcomes[0]
	if row.StepID.Value != "step-a" || !row.StepID.Untrusted {
		t.Errorf("expected step id to be present and marked untrusted (workflow-authored), got %+v", row.StepID)
	}
	if row.ErrorDetail.Value != "curl exit 7" || !row.ErrorDetail.Untrusted {
		t.Errorf("expected raw error detail marked untrusted, got %+v", row.ErrorDetail)
	}
	if ft.NarrationTail != nil {
		t.Errorf("expected nil narration tail when no narration repo is wired, got %+v", ft.NarrationTail)
	}
}

// TestAssemble_FailedTask_NarrationTailPresent covers the Phase-2
// narration seam: when a narration repo is wired and has rows for the
// task's execution, the tail is populated, ordered, capped, and every
// line is marked Untrusted (LLM-authored prose).
func TestAssemble_FailedTask_NarrationTailPresent(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassTimeout)}
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1"}
	lines := []*persistence.ExecutionNarration{
		{Seq: 2, Kind: persistence.ExecutionNarrationKindStep, Text: "second"},
		{Seq: 1, Kind: persistence.ExecutionNarrationKindStep, Text: "first"},
		{Seq: 3, Kind: persistence.ExecutionNarrationKindCompletion, Text: "third"},
	}

	a := &Assembler{
		Tasks:      &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Executions: &fakeExecutionRepo{byTask: map[string]*persistence.Execution{"task-1": exec}},
		Narration:  &fakeNarrationRepo{byExecution: map[string][]*persistence.ExecutionNarration{"exec-1": lines}},
	}

	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1", ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	tail := bundle.FailedTask.NarrationTail
	if len(tail) != 3 {
		t.Fatalf("expected 3 narration lines, got %d: %+v", len(tail), tail)
	}
	if tail[0].Text.Value != "first" || tail[1].Text.Value != "second" || tail[2].Text.Value != "third" {
		t.Fatalf("expected seq-ascending order, got %+v", tail)
	}
	for _, l := range tail {
		if !l.Text.Untrusted {
			t.Errorf("expected narration text marked untrusted (agent/tool-origin), got %+v", l)
		}
	}
}

// TestAssemble_FailedTask_NarrationTailCapped pins the last-N-lines
// contract: with more rows than the limit, only the most recent N
// survive.
func TestAssemble_FailedTask_NarrationTailCapped(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassTimeout)}
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1"}
	var lines []*persistence.ExecutionNarration
	for i := int64(1); i <= 5; i++ {
		lines = append(lines, &persistence.ExecutionNarration{Seq: i, Text: "line"})
	}

	a := &Assembler{
		Tasks:          &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Executions:     &fakeExecutionRepo{byTask: map[string]*persistence.Execution{"task-1": exec}},
		Narration:      &fakeNarrationRepo{byExecution: map[string][]*persistence.ExecutionNarration{"exec-1": lines}},
		NarrationLimit: 2,
	}

	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	tail := bundle.FailedTask.NarrationTail
	if len(tail) != 2 {
		t.Fatalf("expected tail capped to 2, got %d", len(tail))
	}
}

// TestAssemble_FailedTask_NarrationAbsent_StillFunctional pins §5.1's
// "narration_disabled degradation": no narration repo wired at all
// still yields a fully populated bundle on error class + playbook +
// step outcomes alone, with no error.
func TestAssemble_FailedTask_NarrationAbsent_StillFunctional(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassBudgetBlocked)}

	a := &Assembler{
		Tasks: &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
	}

	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err != nil {
		t.Fatalf("expected no error when narration/executions/step-outcomes are all unwired, got %v", err)
	}
	if bundle.FailedTask.NarrationTail != nil {
		t.Errorf("expected nil narration tail, got %+v", bundle.FailedTask.NarrationTail)
	}
	if bundle.FailedTask.Cause.Value == "" {
		t.Errorf("expected the bundle to still carry playbook cause text")
	}
}

func TestAssemble_FailedTask_UnknownTaskErrors(t *testing.T) {
	a := &Assembler{Tasks: &fakeTaskRepo{tasks: map[string]*persistence.Task{}}}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "missing"})
	if err == nil {
		t.Fatal("expected an error for an unknown task ID")
	}
}

func TestAssemble_FailedTask_NoTaskRepositoryErrors(t *testing.T) {
	a := &Assembler{}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err == nil {
		t.Fatal("expected an error when no TaskRepository is wired")
	}
}

// TestAssemble_FailedTask_LearnedRemediationsPresent covers the
// learned-overlay path (playbook.LearnedRemediations) and pins that
// its Action text — worker-mined, ultimately agent/tool-derived — is
// marked Untrusted.
func TestAssemble_FailedTask_LearnedRemediationsPresent(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassRateLimited)}
	repo := &fakeInstinctRepo{
		rows: []*persistence.Instinct{
			{
				ID:         "inst-1",
				Status:     persistence.InstinctStatusActive,
				Domain:     persistence.InstinctDomainRecovery,
				ProjectID:  "proj-1",
				Action:     "backing off and retrying resolved the rate-limit failure",
				Confidence: 0.9,
				Trigger:    []byte(`{"error_class":"` + persistence.TaskFailureClassRateLimited + `"}`),
			},
		},
	}
	a := &Assembler{
		Tasks:   &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Learned: repo,
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	lr := bundle.FailedTask.LearnedRemediations
	if len(lr) != 1 {
		t.Fatalf("expected 1 learned remediation, got %d: %+v", len(lr), lr)
	}
	if !lr[0].Action.Untrusted {
		t.Errorf("expected learned Action marked untrusted, got %+v", lr[0].Action)
	}
}

func TestAssemble_FailedTask_LearnedRemediationsErrorPropagates(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassRateLimited)}
	a := &Assembler{
		Tasks:   &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Learned: &fakeInstinctRepo{err: errors.New("db down")},
	}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err == nil {
		t.Fatal("expected the learned-remediations error to propagate")
	}
}

func TestAssemble_FailedTask_CustomStepOutcomeAndLearnedLimits(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassToolError)}
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1"}
	var outcomes []*persistence.ExecutionStepOutcome
	for i := 0; i < 5; i++ {
		outcomes = append(outcomes, &persistence.ExecutionStepOutcome{StepID: "s", Outcome: "failed"})
	}
	a := &Assembler{
		Tasks:            &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Executions:       &fakeExecutionRepo{byTask: map[string]*persistence.Execution{"task-1": exec}},
		StepOutcomes:     &fakeStepOutcomeRepo{byExecution: map[string][]*persistence.ExecutionStepOutcome{"exec-1": outcomes}},
		StepOutcomeLimit: 2,
		LearnedLimit:     1,
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(bundle.FailedTask.StepOutcomes) != 2 {
		t.Fatalf("expected step outcomes capped to 2, got %d", len(bundle.FailedTask.StepOutcomes))
	}
}

func TestAssemble_FailedTask_StepOutcomesErrorPropagates(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassToolError)}
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1"}
	a := &Assembler{
		Tasks:        &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Executions:   &fakeExecutionRepo{byTask: map[string]*persistence.Execution{"task-1": exec}},
		StepOutcomes: &fakeStepOutcomeRepo{err: errors.New("db down")},
	}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err == nil {
		t.Fatal("expected the step-outcomes error to propagate")
	}
}

func TestAssemble_FailedTask_NarrationErrorPropagates(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassToolError)}
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1"}
	a := &Assembler{
		Tasks:      &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Executions: &fakeExecutionRepo{byTask: map[string]*persistence.Execution{"task-1": exec}},
		Narration:  &fakeNarrationRepo{err: errors.New("db down")},
	}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err == nil {
		t.Fatal("expected the narration error to propagate")
	}
}

// TestAssemble_FailedTask_NarrationEmptyRows covers narrationTail's
// explicit empty-rows path (as opposed to a nil narration repo).
func TestAssemble_FailedTask_NarrationEmptyRows(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassToolError)}
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1"}
	a := &Assembler{
		Tasks:      &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Executions: &fakeExecutionRepo{byTask: map[string]*persistence.Execution{"task-1": exec}},
		Narration:  &fakeNarrationRepo{byExecution: map[string][]*persistence.ExecutionNarration{}},
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if bundle.FailedTask.NarrationTail != nil {
		t.Errorf("expected nil narration tail for zero rows, got %+v", bundle.FailedTask.NarrationTail)
	}
}

// TestAssemble_FailedTask_NarrationSkipsNilRows pins narrationTail's
// defensive nil-row skip (a defensive guard against a malformed repo
// implementation).
func TestAssemble_FailedTask_NarrationSkipsNilRows(t *testing.T) {
	rows := []*persistence.ExecutionNarration{nil, {Seq: 1, Text: "ok"}}
	out := narrationTail(rows, 0)
	if len(out) != 1 || out[0].Text.Value != "ok" {
		t.Fatalf("expected nil rows skipped, got %+v", out)
	}
}

// --- degraded_feature ----------------------------------------------------

type fakeConfigReader struct {
	values map[string]any
}

func (f *fakeConfigReader) GateValue(key string) (any, bool) {
	v, ok := f.values[key]
	return v, ok
}

func TestAssemble_DegradedFeature_BundleShape(t *testing.T) {
	feature := featuredoctor.Feature{
		ID:     "test-feature",
		Title:  "Test Feature",
		DocRef: "docs/public/features/test-feature.md",
		Gates:  []featuredoctor.Gate{{Key: "test.enabled", EnableTo: true}},
		Prereqs: []featuredoctor.Prereq{
			{Name: "model-reachable", Check: func(_ context.Context, _ featuredoctor.Deps) featuredoctor.PrereqResult {
				return featuredoctor.PrereqResult{OK: false, Detail: "model unreachable at http://10.0.0.5:11434", Remediation: "start the model server"}
			}},
		},
	}
	a := &Assembler{
		Features:    []featuredoctor.Feature{feature},
		FeatureDeps: featuredoctor.Deps{Config: &fakeConfigReader{values: map[string]any{"test.enabled": true}}},
	}

	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "test-feature"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	df := bundle.DegradedFeature
	if df == nil {
		t.Fatal("expected a populated DegradedFeature bundle")
	}
	if df.Status.Value != string(featuredoctor.StatusDegraded) {
		t.Errorf("expected status degraded, got %q", df.Status.Value)
	}
	if len(df.FailingPrereqs) != 1 {
		t.Fatalf("expected 1 failing prereq, got %d", len(df.FailingPrereqs))
	}
	fp := df.FailingPrereqs[0]
	if !fp.Detail.Untrusted {
		t.Errorf("expected live-system-state Detail marked untrusted, got %+v", fp.Detail)
	}
	if fp.Remediation.Untrusted {
		t.Errorf("expected code-authored Remediation marked trusted, got %+v", fp.Remediation)
	}
	if df.DocRef.Value != feature.DocRef {
		t.Errorf("expected doc ref %q, got %q", feature.DocRef, df.DocRef.Value)
	}
}

func TestAssemble_DegradedFeature_UnknownFeatureErrors(t *testing.T) {
	a := &Assembler{Features: []featuredoctor.Feature{}}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "nope"})
	if err == nil {
		t.Fatal("expected an error for an unknown feature ID")
	}
}

// TestAssemble_DegradedFeature_DefaultsToRealRegistry covers the
// features() nil -> featuredoctor.Registry() default path.
func TestAssemble_DegradedFeature_DefaultsToRealRegistry(t *testing.T) {
	a := &Assembler{FeatureDeps: featuredoctor.Deps{Config: &fakeConfigReader{}}}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "auth"})
	if err != nil {
		t.Fatalf("Assemble against the real registry: %v", err)
	}
	if bundle.DegradedFeature == nil {
		t.Fatal("expected a populated DegradedFeature bundle from the real registry")
	}
}

// TestAssemble_DegradedFeature_PassingPrereqOmitted pins that only
// FAILING prereqs are surfaced (an OK prereq is noise for a repair
// conversation, not signal).
func TestAssemble_DegradedFeature_PassingPrereqOmitted(t *testing.T) {
	feature := featuredoctor.Feature{
		ID: "ok-feature",
		Prereqs: []featuredoctor.Prereq{
			{Name: "fine", Check: func(_ context.Context, _ featuredoctor.Deps) featuredoctor.PrereqResult {
				return featuredoctor.PrereqResult{OK: true}
			}},
		},
	}
	a := &Assembler{
		Features:    []featuredoctor.Feature{feature},
		FeatureDeps: featuredoctor.Deps{Config: &fakeConfigReader{}},
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "ok-feature"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(bundle.DegradedFeature.FailingPrereqs) != 0 {
		t.Errorf("expected the passing prereq to be omitted, got %+v", bundle.DegradedFeature.FailingPrereqs)
	}
}

// TestAssemble_DegradedFeature_FailingVerify covers the Verify-failed
// branch (gates on, prereqs met, but Verify itself fails).
func TestAssemble_DegradedFeature_FailingVerify(t *testing.T) {
	feature := featuredoctor.Feature{
		ID:    "verify-feature",
		Gates: []featuredoctor.Gate{{Key: "vf.enabled", EnableTo: true}},
		Verify: func(_ context.Context, _ featuredoctor.Deps) featuredoctor.PrereqResult {
			return featuredoctor.PrereqResult{OK: false, Detail: "verify probe failed: connection refused", Remediation: "check the service is running"}
		},
	}
	a := &Assembler{
		Features:    []featuredoctor.Feature{feature},
		FeatureDeps: featuredoctor.Deps{Config: &fakeConfigReader{values: map[string]any{"vf.enabled": true}}},
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "verify-feature"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	fv := bundle.DegradedFeature.FailingVerify
	if fv == nil {
		t.Fatal("expected FailingVerify to be populated")
	}
	if !fv.Detail.Untrusted {
		t.Errorf("expected verify Detail marked untrusted, got %+v", fv.Detail)
	}
}

// --- red_integration ------------------------------------------------------

func TestAssemble_RedIntegration_BundleShape(t *testing.T) {
	probe := integrations.ProbeResult{
		OK:      false,
		Outcome: integrations.OutcomeFail,
		Kind:    "telegram",
		Summary: "bot token rejected",
		Detail:  "Telegram API returned 401 Unauthorized",
		Failures: []integrations.CheckFailure{
			{Field: "bot_token", Reason: "token rejected by Telegram"},
		},
	}
	a := &Assembler{
		IntegrationProbes: &fakeIntegrationProbes{result: probe, docURL: "https://docs.vornik.io/integrations/telegram", ok: true},
	}

	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindRedIntegration, ID: "telegram", ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	ri := bundle.RedIntegration
	if ri == nil {
		t.Fatal("expected a populated RedIntegration bundle")
	}
	if ri.Outcome.Value != string(integrations.OutcomeFail) {
		t.Errorf("expected outcome fail, got %q", ri.Outcome.Value)
	}
	if !ri.Summary.Untrusted || !ri.Detail.Untrusted {
		t.Errorf("expected Summary/Detail marked untrusted, got %+v / %+v", ri.Summary, ri.Detail)
	}
	if ri.DocURL.Value != "https://docs.vornik.io/integrations/telegram" {
		t.Errorf("unexpected doc url %q", ri.DocURL.Value)
	}
	if ri.FailedField.Value != "bot_token" {
		t.Errorf("expected failed field bot_token, got %q", ri.FailedField.Value)
	}
	if len(ri.Failures) != 1 || !ri.Failures[0].Reason.Untrusted {
		t.Errorf("expected 1 failure with untrusted reason, got %+v", ri.Failures)
	}
}

func TestAssemble_RedIntegration_NoProbeResultErrors(t *testing.T) {
	a := &Assembler{IntegrationProbes: &fakeIntegrationProbes{ok: false}}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindRedIntegration, ID: "telegram"})
	if err == nil {
		t.Fatal("expected an error when no probe result is known")
	}
}

func TestAssemble_RedIntegration_NoProviderErrors(t *testing.T) {
	a := &Assembler{}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindRedIntegration, ID: "telegram"})
	if err == nil {
		t.Fatal("expected an error when no IntegrationProbeProvider is wired")
	}
}

func TestAssemble_RedIntegration_ProviderErrorPropagates(t *testing.T) {
	a := &Assembler{IntegrationProbes: &fakeIntegrationProbes{err: errors.New("probe cache unavailable")}}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindRedIntegration, ID: "telegram"})
	if err == nil {
		t.Fatal("expected the provider error to propagate")
	}
}

// --- failed_reload --------------------------------------------------------

func TestAssemble_FailedReload_BundleShape(t *testing.T) {
	a := &Assembler{
		ReloadStatus: &fakeReloadStatus{
			ok: true,
			rv: ReloadValidationError{
				Message:          "invalid duration for llm.timeout: \"abc\"",
				OffendingKeyPath: "llm.timeout",
				OffendingValue:   "abc",
			},
		},
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedReload})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	fr := bundle.FailedReload
	if fr == nil {
		t.Fatal("expected a populated FailedReload bundle")
	}
	if !fr.Message.Untrusted || !fr.OffendingKeyPath.Untrusted {
		t.Errorf("expected Message/OffendingKeyPath marked untrusted, got %+v / %+v", fr.Message, fr.OffendingKeyPath)
	}
	if fr.OffendingValue == nil || fr.OffendingValue.Value != "abc" {
		t.Errorf("expected non-secret-shaped offending value to survive, got %+v", fr.OffendingValue)
	}
}

// TestAssemble_FailedReload_SecretMasking is the load-bearing
// secret-masking test (§5.1/§8): a secret-shaped offending config
// value must never reach the assembled bundle, and the lifted shared
// redactor is what enforces that.
func TestAssemble_FailedReload_SecretMasking(t *testing.T) {
	const rawSecret = "sk-super-secret-value-12345"
	a := &Assembler{
		ReloadStatus: &fakeReloadStatus{
			ok: true,
			rv: ReloadValidationError{
				Message:          "invalid telegram.bot_token",
				OffendingKeyPath: "telegram.bot_token",
				OffendingValue:   rawSecret,
			},
		},
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedReload})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	fr := bundle.FailedReload
	if fr.OffendingValue == nil {
		t.Fatal("expected an OffendingValue field")
	}
	if fr.OffendingValue.Value == rawSecret {
		t.Fatalf("secret leaked into the assembled bundle: %q", fr.OffendingValue.Value)
	}
	if fr.OffendingValue.Value != "<redacted>" {
		t.Fatalf("expected the shared redactor's placeholder, got %q", fr.OffendingValue.Value)
	}
}

func TestAssemble_FailedReload_NoErrorKnownErrors(t *testing.T) {
	a := &Assembler{ReloadStatus: &fakeReloadStatus{ok: false}}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedReload})
	if err == nil {
		t.Fatal("expected an error when no reload error is known")
	}
}

func TestAssemble_FailedReload_NoProviderErrors(t *testing.T) {
	a := &Assembler{}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedReload})
	if err == nil {
		t.Fatal("expected an error when no ReloadStatusProvider is wired")
	}
}

func TestAssemble_FailedReload_ProviderErrorPropagates(t *testing.T) {
	a := &Assembler{ReloadStatus: &fakeReloadStatus{err: errors.New("flag store unavailable")}}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedReload})
	if err == nil {
		t.Fatal("expected the provider error to propagate")
	}
}

// TestAssemble_FailedReload_NoOffendingValue covers the branch where
// the caller doesn't know the raw offending value (OffendingValue
// stays nil rather than a masked-empty placeholder).
func TestAssemble_FailedReload_NoOffendingValue(t *testing.T) {
	a := &Assembler{
		ReloadStatus: &fakeReloadStatus{ok: true, rv: ReloadValidationError{Message: "bad config", OffendingKeyPath: "some.path"}},
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedReload})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if bundle.FailedReload.OffendingValue != nil {
		t.Errorf("expected nil OffendingValue when none is known, got %+v", bundle.FailedReload.OffendingValue)
	}
}

// --- unknown kind ---------------------------------------------------------

func TestAssemble_UnknownKindErrors(t *testing.T) {
	a := &Assembler{}
	_, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKind("bogus")})
	if err == nil {
		t.Fatal("expected an error for an unknown failure kind")
	}
}

// --- defense-in-depth free-text secret redaction (companion review
// 2026-07-10, review-20260710-e0b1.md) ------------------------------------
//
// These tests pin that the assembler does NOT rely on upstream
// contracts (Phase-5 probes are "secret-free by contract", Phase-2
// narration is "redacted at source") for secret safety: every
// Untrusted free-text string the assembler emits gets its own
// defense-in-depth scan+redact pass. Each test embeds a secret-shaped
// substring (matching secrets.DefaultPatterns' openai_key pattern,
// chosen because it needs no special setup) inside otherwise-ordinary
// free text and asserts the raw secret substring never survives into
// the assembled bundle.

// adversarialSecret matches secrets' openai_key pattern
// (`\bsk-[A-Za-z0-9]{32,}\b`) — 36 chars after "sk-", well past the
// 32-char floor.
const adversarialSecret = "sk-1234567890abcdefghijklmnopqrstuvwxyz"

func TestAssemble_RedIntegration_SecretInSummaryAndDetailRedacted(t *testing.T) {
	probe := integrations.ProbeResult{
		OK:      false,
		Outcome: integrations.OutcomeFail,
		Kind:    "telegram",
		Summary: "bot token rejected: " + adversarialSecret,
		Detail:  "Telegram API returned 401 Unauthorized for token " + adversarialSecret,
		Failures: []integrations.CheckFailure{
			{Field: "bot_token", Reason: "provider said: " + adversarialSecret + " is invalid"},
		},
	}
	a := &Assembler{
		IntegrationProbes: &fakeIntegrationProbes{result: probe, docURL: "https://docs.vornik.io/integrations/telegram", ok: true},
	}

	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindRedIntegration, ID: "telegram"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	ri := bundle.RedIntegration
	if strings.Contains(ri.Summary.Value, adversarialSecret) {
		t.Fatalf("secret leaked into RedIntegration.Summary: %q", ri.Summary.Value)
	}
	if strings.Contains(ri.Detail.Value, adversarialSecret) {
		t.Fatalf("secret leaked into RedIntegration.Detail: %q", ri.Detail.Value)
	}
	if len(ri.Failures) != 1 || strings.Contains(ri.Failures[0].Reason.Value, adversarialSecret) {
		t.Fatalf("secret leaked into ProbeFailureField.Reason: %+v", ri.Failures)
	}
}

func TestAssemble_FailedTask_SecretInNarrationTextRedacted(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassTimeout)}
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1"}
	lines := []*persistence.ExecutionNarration{
		{Seq: 1, Kind: persistence.ExecutionNarrationKindStep, Text: "used credential " + adversarialSecret + " to call the API"},
	}
	a := &Assembler{
		Tasks:      &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Executions: &fakeExecutionRepo{byTask: map[string]*persistence.Execution{"task-1": exec}},
		Narration:  &fakeNarrationRepo{byExecution: map[string][]*persistence.ExecutionNarration{"exec-1": lines}},
	}

	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	tail := bundle.FailedTask.NarrationTail
	if len(tail) != 1 || strings.Contains(tail[0].Text.Value, adversarialSecret) {
		t.Fatalf("secret leaked into narration tail Text: %+v", tail)
	}
}

func TestAssemble_FailedTask_SecretInLearnedActionRedacted(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassRateLimited)}
	repo := &fakeInstinctRepo{
		rows: []*persistence.Instinct{
			{
				ID:         "inst-1",
				Status:     persistence.InstinctStatusActive,
				Domain:     persistence.InstinctDomainRecovery,
				ProjectID:  "proj-1",
				Action:     "retry using api key " + adversarialSecret + " resolved the failure",
				Confidence: 0.9,
				Trigger:    []byte(`{"error_class":"` + persistence.TaskFailureClassRateLimited + `"}`),
			},
		},
	}
	a := &Assembler{
		Tasks:   &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Learned: repo,
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	lr := bundle.FailedTask.LearnedRemediations
	if len(lr) != 1 || strings.Contains(lr[0].Action.Value, adversarialSecret) {
		t.Fatalf("secret leaked into learned remediation Action: %+v", lr)
	}
}

// TestAssemble_FailedTask_SecretInStepOutcomeErrorDetailRedacted covers
// the fourth Untrusted free-text field the review's generalization
// (not just the three named examples) requires: raw executor error
// text can echo a secret (e.g. a curl command with an embedded
// bearer token in its failure output).
func TestAssemble_FailedTask_SecretInStepOutcomeErrorDetailRedacted(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "proj-1", LastErrorClass: strPtr(persistence.TaskFailureClassToolError)}
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1"}
	outcomes := []*persistence.ExecutionStepOutcome{
		{StepID: "step-a", Role: "coder", Outcome: "failed", ErrorClass: persistence.TaskFailureClassToolError, ErrorDetail: "curl -H 'Authorization: Bearer " + adversarialSecret + "' failed: 401"},
	}
	a := &Assembler{
		Tasks:        &fakeTaskRepo{tasks: map[string]*persistence.Task{"task-1": task}},
		Executions:   &fakeExecutionRepo{byTask: map[string]*persistence.Execution{"task-1": exec}},
		StepOutcomes: &fakeStepOutcomeRepo{byExecution: map[string][]*persistence.ExecutionStepOutcome{"exec-1": outcomes}},
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "task-1"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	rows := bundle.FailedTask.StepOutcomes
	if len(rows) != 1 || strings.Contains(rows[0].ErrorDetail.Value, adversarialSecret) {
		t.Fatalf("secret leaked into StepOutcomeRow.ErrorDetail: %+v", rows)
	}
}

func TestAssemble_DegradedFeature_SecretInPrereqAndVerifyDetailRedacted(t *testing.T) {
	feature := featuredoctor.Feature{
		ID:    "test-feature",
		Title: "Test Feature",
		Gates: []featuredoctor.Gate{{Key: "test.enabled", EnableTo: true}},
		Prereqs: []featuredoctor.Prereq{
			{Name: "model-reachable", Check: func(_ context.Context, _ featuredoctor.Deps) featuredoctor.PrereqResult {
				return featuredoctor.PrereqResult{OK: false, Detail: "model unreachable, key " + adversarialSecret + " rejected", Remediation: "start the model server"}
			}},
		},
		Verify: func(_ context.Context, _ featuredoctor.Deps) featuredoctor.PrereqResult {
			return featuredoctor.PrereqResult{OK: false, Detail: "verify failed with credential " + adversarialSecret, Remediation: "check the service is running"}
		},
	}
	a := &Assembler{
		Features:    []featuredoctor.Feature{feature},
		FeatureDeps: featuredoctor.Deps{Config: &fakeConfigReader{values: map[string]any{"test.enabled": true}}},
	}

	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "test-feature"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	df := bundle.DegradedFeature
	if len(df.FailingPrereqs) != 1 || strings.Contains(df.FailingPrereqs[0].Detail.Value, adversarialSecret) {
		t.Fatalf("secret leaked into PrereqField.Detail: %+v", df.FailingPrereqs)
	}
	if df.FailingVerify == nil || strings.Contains(df.FailingVerify.Detail.Value, adversarialSecret) {
		t.Fatalf("secret leaked into FailingVerify.Detail: %+v", df.FailingVerify)
	}
}

func TestAssemble_FailedReload_SecretInMessageRedacted(t *testing.T) {
	a := &Assembler{
		ReloadStatus: &fakeReloadStatus{
			ok: true,
			rv: ReloadValidationError{
				Message:          "invalid telegram.bot_token: " + adversarialSecret + " is not a recognized token shape",
				OffendingKeyPath: "telegram.bot_token",
				OffendingValue:   adversarialSecret,
			},
		},
	}
	bundle, err := a.Assemble(context.Background(), FailureRef{Kind: FailureKindFailedReload})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	fr := bundle.FailedReload
	if strings.Contains(fr.Message.Value, adversarialSecret) {
		t.Fatalf("secret leaked into FailedReload.Message: %q", fr.Message.Value)
	}
}
