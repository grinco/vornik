package api

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestWindowFullSections exercises the window admin-audit + cost-rollup
// collectors, including redaction of an audit secret and the cost
// aggregation, plus REDACTION coverage for the window-only sections.
func TestWindowFullSections(t *testing.T) {
	now := time.Now().UTC()
	auditSecret := secretFor("WINAUDIT")
	b := &bundleBuilder{
		repos: supportRepos{
			Tasks: &fakeTaskReader{list: []*persistence.Task{
				{ID: "w1", ProjectID: "p", CreatedAt: now, UpdatedAt: now},
			}},
			AdminAudit: &fakeAdminAuditReader{rows: []*persistence.AdminAuditEntry{
				{ID: "a1", Action: "config.update", After: auditSecret, Timestamp: now},
			}},
			LLMUsage: &fakeUsageReader{rows: []*persistence.TaskLLMUsage{
				{ID: "u1", ProjectID: "p", Model: "gpt", CostUSD: 1.5},
				{ID: "u2", ProjectID: "p", Model: "claude", CostUSD: 2.5},
			}},
		},
		detector: newTestDetector(t),
		version:  "t",
	}
	req := bundleRequest{Window: true, Since: now.Add(-time.Hour), Until: now.Add(time.Hour), MaxSize: 200 << 20}
	res, err := b.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Admin audit secret redacted.
	aa := res.files["window/admin_audit.json"]
	if bytes.Contains(aa, []byte(auditSecret)) {
		t.Errorf("admin audit secret leaked:\n%s", aa)
	}
	if !bytes.Contains(aa, []byte("[REDACTED:")) {
		t.Errorf("admin audit not redacted:\n%s", aa)
	}

	// Cost rollup aggregates by project + model + total.
	var roll struct {
		ByProject map[string]float64 `json:"cost_usd_by_project"`
		ByModel   map[string]float64 `json:"cost_usd_by_model"`
		TotalUSD  float64            `json:"total_usd"`
		Rows      int                `json:"rows"`
	}
	if err := json.Unmarshal(res.files["window/cost_rollup.json"], &roll); err != nil {
		t.Fatalf("cost rollup: %v", err)
	}
	if roll.TotalUSD != 4.0 || roll.Rows != 2 || roll.ByProject["p"] != 4.0 {
		t.Errorf("cost rollup wrong: %+v", roll)
	}
}

// TestEnforceTotalCap drops the largest non-essential section to honour
// --max-size and notes it in the manifest; essentials survive.
func TestEnforceTotalCap(t *testing.T) {
	b := &bundleBuilder{
		repos: supportRepos{
			Tasks: &fakeTaskReader{get: &persistence.Task{ID: "t1", ProjectID: "p"}},
			Messages: &fakeMessageReader{rows: []*persistence.TaskMessage{
				{ID: "m1", TaskID: "t1", Content: makeLargeString(50_000)},
			}},
		},
		detector: newTestDetector(t),
		version:  "t",
	}
	// Tiny cap forces a drop of the big messages section.
	req := bundleRequest{TaskID: "t1", MaxSize: 4096}
	res, err := b.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := res.files["task/messages.json"]; ok {
		t.Error("oversized messages.json should have been dropped under the cap")
	}
	if res.truncations["task/messages.json"] == "" {
		t.Error("drop must be noted in truncations (never silent)")
	}
	// Essentials kept.
	if _, ok := res.files["MANIFEST.json"]; !ok {
		t.Error("MANIFEST.json must always survive")
	}
	if _, ok := res.files["version.txt"]; !ok {
		t.Error("version.txt is essential and must survive")
	}
}

func TestIsEssentialFile(t *testing.T) {
	for _, n := range []string{"MANIFEST.json", "REDACTION.txt", "version.txt", "doctor.json"} {
		if !isEssentialFile(n) {
			t.Errorf("%s should be essential", n)
		}
	}
	if isEssentialFile("task/messages.json") {
		t.Error("messages.json is not essential")
	}
}

// TestWindowSectionErrors: window repos that error record section errors
// without failing the build.
func TestWindowSectionErrors(t *testing.T) {
	b := &bundleBuilder{
		repos: supportRepos{
			Tasks:      &fakeTaskReader{err: errBoom},
			AdminAudit: &fakeAdminAuditReaderErr{},
			LLMUsage:   &fakeUsageReaderErr{},
		},
		detector: newTestDetector(t),
		version:  "t",
	}
	now := time.Now()
	req := bundleRequest{Window: true, Since: now.Add(-time.Hour), Until: now, MaxSize: 200 << 20}
	res, err := b.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, k := range []string{"window/tasks.json", "window/admin_audit.json", "window/cost_rollup.json"} {
		if res.sectionErrs[k] == "" {
			t.Errorf("expected section error recorded for %s", k)
		}
	}
}

func makeLargeString(n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = 'x'
	}
	return string(buf)
}

type fakeAdminAuditReaderErr struct{}

func (fakeAdminAuditReaderErr) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, errBoom
}

type fakeUsageReaderErr struct{}

func (fakeUsageReaderErr) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return nil, errBoom
}

// erroring fakes for every repo, to exercise each collector's
// best-effort error branch in a single task-mode build.
type errExecReader struct{}

func (errExecReader) List(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	return nil, errBoom
}

type errOutcomeReader struct{}

func (errOutcomeReader) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return nil, errBoom
}

type errToolAuditReader struct{}

func (errToolAuditReader) List(context.Context, persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return nil, errBoom
}

type errMessageReader struct{}

func (errMessageReader) List(context.Context, persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, errBoom
}

type errJudgeReader struct{}

func (errJudgeReader) GetByTask(context.Context, string) (*persistence.TaskJudgeVerdict, error) {
	return nil, errBoom
}

type errPostMortemReader struct{}

func (errPostMortemReader) Get(context.Context, string) (*persistence.TaskPostMortem, error) {
	return nil, errBoom
}

type errArtifactReader struct{}

func (errArtifactReader) List(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return nil, errBoom
}

type errDoctor struct{}

func (errDoctor) Run(context.Context) (any, error) { return nil, errBoom }

type errHealth struct{}

func (errHealth) Snapshot(context.Context) (any, error) { return nil, errBoom }

type errMetrics struct{}

func (errMetrics) Snapshot(context.Context) (string, error) { return "", errBoom }

// TestTaskAllSectionsError drives every collector's error branch.
func TestTaskAllSectionsError(t *testing.T) {
	b := &bundleBuilder{
		repos: supportRepos{
			Tasks:       &fakeTaskReader{err: errBoom},
			Executions:  errExecReader{},
			Outcomes:    errOutcomeReader{},
			ToolAudit:   errToolAuditReader{},
			LLMUsage:    fakeUsageReaderErr{},
			Messages:    errMessageReader{},
			JudgeVerdct: errJudgeReader{},
			PostMortem:  errPostMortemReader{},
			Artifacts:   errArtifactReader{},
		},
		doctor:   errDoctor{},
		health:   errHealth{},
		metrics:  errMetrics{},
		detector: newTestDetector(t),
		version:  "t",
	}
	req := bundleRequest{TaskID: "x", MaxSize: 200 << 20}
	res, err := b.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build must not fail on section errors: %v", err)
	}
	for _, k := range []string{
		"task/task.json", "task/executions.json", "task/step_outcomes.json",
		"task/tool_audit.json", "task/llm_usage.json", "task/messages.json",
		"task/judge.json", "task/postmortem.json", "doctor.json", "health.json", "metrics.txt",
	} {
		if res.sectionErrs[k] == "" {
			t.Errorf("expected section error recorded for %s", k)
		}
	}
}

// TestNilReposDegrade: a builder with no repos still produces a valid
// bundle (always-on sections only).
func TestNilReposDegrade(t *testing.T) {
	b := &bundleBuilder{detector: newTestDetector(t), version: "t"}
	res, err := b.Build(context.Background(), bundleRequest{TaskID: "x", MaxSize: 200 << 20})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := res.files["MANIFEST.json"]; !ok {
		t.Error("MANIFEST.json should still be produced with no repos")
	}
	if _, ok := res.files["doctor.json"]; !ok {
		t.Error("doctor.json placeholder should be produced when doctor nil")
	}
}

func TestCapOutcomesTruncates(t *testing.T) {
	res := &buildResult{truncations: map[string]string{}}
	rows := make([]*persistence.ExecutionStepOutcome, 5)
	out := capOutcomes(rows, 3, "x", res)
	if len(out) != 3 || res.truncations["x"] == "" {
		t.Fatalf("cap: len=%d trunc=%q", len(out), res.truncations["x"])
	}
}

func TestTaskInWindowNil(t *testing.T) {
	if taskInWindow(nil, time.Now(), time.Now()) {
		t.Error("nil task should not be in window")
	}
}

func TestSafeArtifactFilename(t *testing.T) {
	if got := safeArtifactFilename("idididididididid", "a/b/../c.txt"); !strings.Contains(got, "c.txt") {
		t.Errorf("got %q", got)
	}
	if got := safeArtifactFilename("x", ""); got == "" {
		t.Error("empty name should fall back")
	}
}

// TestPerSectionCaps exceeds each per-section row cap so the truncation
// branches fire + are noted in the manifest.
func TestPerSectionCaps(t *testing.T) {
	mkAudit := func(n int) []*persistence.ToolAuditEntry {
		out := make([]*persistence.ToolAuditEntry, n)
		for i := range out {
			out[i] = &persistence.ToolAuditEntry{ID: "a"}
		}
		return out
	}
	mkUsage := func(n int) []*persistence.TaskLLMUsage {
		out := make([]*persistence.TaskLLMUsage, n)
		for i := range out {
			out[i] = &persistence.TaskLLMUsage{ID: "u"}
		}
		return out
	}
	mkMsg := func(n int) []*persistence.TaskMessage {
		out := make([]*persistence.TaskMessage, n)
		for i := range out {
			out[i] = &persistence.TaskMessage{ID: "m"}
		}
		return out
	}
	mkOut := func(n int) []*persistence.ExecutionStepOutcome {
		out := make([]*persistence.ExecutionStepOutcome, n)
		for i := range out {
			out[i] = &persistence.ExecutionStepOutcome{ID: "o"}
		}
		return out
	}
	mkArt := func(n int) []*persistence.Artifact {
		out := make([]*persistence.Artifact, n)
		for i := range out {
			out[i] = &persistence.Artifact{ID: "art"}
		}
		return out
	}
	b := &bundleBuilder{
		repos: supportRepos{
			Tasks:     &fakeTaskReader{get: &persistence.Task{ID: "t1"}},
			ToolAudit: &fakeToolAuditReader{rows: mkAudit(defaultToolAuditCap + 5)},
			LLMUsage:  &fakeUsageReader{rows: mkUsage(defaultUsageCap + 5)},
			Messages:  &fakeMessageReader{rows: mkMsg(defaultMessageCap + 5)},
			Outcomes:  &fakeOutcomeReader{rows: mkOut(defaultOutcomeCap + 5)},
			Artifacts: &fakeArtifactReader{rows: mkArt(defaultArtifactCap + 5)},
		},
		detector: newTestDetector(t),
		version:  "t",
	}
	req := bundleRequest{TaskID: "t1", MaxSize: 200 << 20}
	res, err := b.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, k := range []string{
		"task/tool_audit.json", "task/llm_usage.json", "task/messages.json",
		"task/step_outcomes.json", "task/artifacts/MANIFEST.json",
	} {
		if res.truncations[k] == "" {
			t.Errorf("expected truncation note for %s", k)
		}
	}
}

// TestEmptyVersionAndContainerLogs covers the empty-version fallback and
// the empty-container-log early return.
func TestEmptyVersionAndContainerLogs(t *testing.T) {
	b := &bundleBuilder{detector: newTestDetector(t)} // empty version
	req := bundleRequest{TaskID: "t1", MaxSize: 200 << 20}
	res, err := b.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(string(res.files["version.txt"]), "unknown") {
		t.Errorf("empty version should fall back to 'unknown': %q", res.files["version.txt"])
	}
	// Empty container logs: no file written.
	b.WriteContainerLogs(req, res, "   ")
	if _, ok := res.files["task/container_logs.txt"]; ok {
		t.Error("blank container logs should not produce a file")
	}
}
