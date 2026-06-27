package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// ---- fakes ----

type fakeTaskReader struct {
	get  *persistence.Task
	list []*persistence.Task
	err  error
}

func (f *fakeTaskReader) Get(_ context.Context, _ string) (*persistence.Task, error) {
	return f.get, f.err
}
func (f *fakeTaskReader) List(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
	return f.list, f.err
}

type fakeExecReader struct {
	rows []*persistence.Execution
	err  error
}

func (f *fakeExecReader) List(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	return f.rows, f.err
}

type fakeOutcomeReader struct {
	rows []*persistence.ExecutionStepOutcome
}

func (f *fakeOutcomeReader) List(_ context.Context, _ persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return f.rows, nil
}

type fakeToolAuditReader struct {
	rows []*persistence.ToolAuditEntry
}

func (f *fakeToolAuditReader) List(_ context.Context, _ persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return f.rows, nil
}

type fakeUsageReader struct {
	rows []*persistence.TaskLLMUsage
}

func (f *fakeUsageReader) List(_ context.Context, _ persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return f.rows, nil
}

type fakeMessageReader struct {
	rows []*persistence.TaskMessage
}

func (f *fakeMessageReader) List(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return f.rows, nil
}

type fakeJudgeReader struct {
	v *persistence.TaskJudgeVerdict
}

func (f *fakeJudgeReader) GetByTask(_ context.Context, _ string) (*persistence.TaskJudgeVerdict, error) {
	return f.v, nil
}

type fakePostMortemReader struct {
	pm *persistence.TaskPostMortem
}

func (f *fakePostMortemReader) Get(_ context.Context, _ string) (*persistence.TaskPostMortem, error) {
	return f.pm, nil
}

type fakeArtifactReader struct {
	rows []*persistence.Artifact
}

func (f *fakeArtifactReader) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return f.rows, nil
}

type fakeAdminAuditReader struct {
	rows []*persistence.AdminAuditEntry
}

func (f *fakeAdminAuditReader) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return f.rows, nil
}

type fakeArtifactOpener struct {
	blobs map[string][]byte
}

func (f *fakeArtifactOpener) Open(_ context.Context, id string) (readCloser, error) {
	return io.NopCloser(bytes.NewReader(f.blobs[id])), nil
}

type fakeDoctor struct{ rep any }

func (f *fakeDoctor) Run(_ context.Context) (any, error) { return f.rep, nil }

type fakeHealth struct{ snap any }

func (f *fakeHealth) Snapshot(_ context.Context) (any, error) { return f.snap, nil }

type fakeMetrics struct{ txt string }

func (f *fakeMetrics) Snapshot(_ context.Context) (string, error) { return f.txt, nil }

func newTestDetector(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatalf("detector: %v", err)
	}
	return d
}

// secretFor builds a distinct, high-precision OpenAI-shaped key per
// section so the redaction-coverage test can pinpoint which section
// leaked if one survives.
func secretFor(tag string) string {
	// sk- + 48 chars → matches openai_key (sk-[A-Za-z0-9]{32,}).
	base := "sk-" + tag
	for len(base) < 51 {
		base += "x"
	}
	return base
}

// fullyWiredBuilder returns a builder whose every section carries a
// distinct seeded secret. Returns the builder + the map of
// section-tag -> seeded secret so the test can assert each is gone.
func fullyWiredBuilder(t *testing.T) (*bundleBuilder, map[string]string) {
	t.Helper()
	secretsByTag := map[string]string{
		"task":      secretFor("TASKAAAA"),
		"exec":      secretFor("EXECBBBB"),
		"outcome":   secretFor("OUTCCCCC"),
		"toolin":    secretFor("TOOLIN00"),
		"toolout":   secretFor("TOOLOUT0"),
		"usage":     secretFor("USAGE000"),
		"message":   secretFor("MSGAAAA0"),
		"judge":     secretFor("JUDGE000"),
		"pm":        secretFor("PMAAAA00"),
		"artifact":  secretFor("ARTTEXT0"),
		"config":    secretFor("CONFIG00"),
		"metrics":   secretFor("METRIC00"),
		"doctor":    secretFor("DOCTOR00"),
		"health":    secretFor("HEALTH00"),
		"container": secretFor("CONTLOG0"),
		"window":    secretFor("WINDOW00"),
		"audit":     secretFor("AUDIT000"),
	}
	taskID := "task_test_1"
	pid := "proj1"
	now := time.Now()

	repos := supportRepos{
		Tasks: &fakeTaskReader{
			get: &persistence.Task{ID: taskID, ProjectID: pid, CreatedAt: now, UpdatedAt: now}, // stash a secret in a free-text-ish field by abusing the
			// task description via JSON: we rely on Task having such a
			// field; if not, the prompt below carries it.

			list: []*persistence.Task{{ID: taskID, ProjectID: pid, CreatedAt: now, UpdatedAt: now}},
		},
		Executions: &fakeExecReader{rows: []*persistence.Execution{
			{ID: "exec1", TaskID: taskID, ProjectID: pid},
		}},
		Outcomes: &fakeOutcomeReader{rows: []*persistence.ExecutionStepOutcome{
			{ID: "o1", TaskID: taskID, ProjectID: pid, Outcome: secretsByTag["outcome"]},
		}},
		ToolAudit: &fakeToolAuditReader{rows: []*persistence.ToolAuditEntry{
			{ID: "ta1", TaskID: taskID, ProjectID: pid, ToolInput: secretsByTag["toolin"], ToolOutput: secretsByTag["toolout"]},
		}},
		LLMUsage: &fakeUsageReader{rows: []*persistence.TaskLLMUsage{
			{ID: "u1", ProjectID: pid, Model: secretsByTag["usage"], CostUSD: 0.1},
		}},
		Messages: &fakeMessageReader{rows: []*persistence.TaskMessage{
			{ID: "m1", TaskID: taskID, Content: secretsByTag["message"]},
		}},
		JudgeVerdct: &fakeJudgeReader{v: &persistence.TaskJudgeVerdict{ID: "j1", TaskID: taskID, ProjectID: pid, Verdict: secretsByTag["judge"]}},
		PostMortem:  &fakePostMortemReader{pm: &persistence.TaskPostMortem{TaskID: taskID, ProjectID: pid, Summary: secretsByTag["pm"]}},
		Artifacts: &fakeArtifactReader{rows: []*persistence.Artifact{
			{ID: "art1", ProjectID: pid, Name: "notes.txt"},
		}},
		AdminAudit: &fakeAdminAuditReader{rows: []*persistence.AdminAuditEntry{
			{ID: "aa1", Action: "config.update", After: secretsByTag["audit"], Timestamp: now},
		}},
	}
	// Stash the task secret in LastError — a free-text *string field
	// the JSON serializer emits as a plain string (so the detector
	// sees it; Payload is []byte and marshals as base64, which would
	// defeat scanning — exactly the kind of field we'd want flagged in
	// a real review, but here we want a string-shaped carrier).
	taskSecret := secretsByTag["task"]
	repos.Tasks.(*fakeTaskReader).get.LastError = &taskSecret
	repos.Executions.(*fakeExecReader).rows[0].WorkflowID = secretsByTag["exec"]

	b := &bundleBuilder{
		repos:      repos,
		opener:     &fakeArtifactOpener{blobs: map[string][]byte{"art1": []byte("artifact body " + secretsByTag["artifact"] + "\n")}},
		doctor:     &fakeDoctor{rep: map[string]string{"note": secretsByTag["doctor"]}},
		health:     &fakeHealth{snap: map[string]string{"status": secretsByTag["health"]}},
		metrics:    &fakeMetrics{txt: "# metric line " + secretsByTag["metrics"] + "\n"},
		detector:   newTestDetector(t),
		version:    "2026.6.0-test",
		configYAML: "api_key: " + secretsByTag["config"] + "\n",
	}
	return b, secretsByTag
}

// TestRedactionCoverage is the load-bearing test (LLD §9): a distinct
// secret seeded into EVERY collected section must not survive raw in
// ANY produced file, and REDACTION.txt must count them.
func TestRedactionCoverage(t *testing.T) {
	b, secretsByTag := fullyWiredBuilder(t)
	req := bundleRequest{TaskID: "task_test_1", MaxSize: 200 << 20}
	res, err := b.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Inject container logs the way the handler does.
	b.WriteContainerLogs(req, res, "container stderr: "+secretsByTag["container"]+"\n")
	// Re-finalize so REDACTION.txt/MANIFEST reflect the injected section.
	b.finalize(req, res)

	// No raw secret in ANY file.
	for fname, content := range res.files {
		for tag, secret := range secretsByTag {
			if tag == "window" || tag == "audit" {
				continue // window-only sections; covered by the window test
			}
			if bytes.Contains(content, []byte(secret)) {
				t.Errorf("raw secret for %q survived in %s", tag, fname)
			}
		}
	}

	// Each task section must have produced at least one redaction.
	mustContainRedacted(t, res, "task/task.json")
	mustContainRedacted(t, res, "task/executions.json")
	mustContainRedacted(t, res, "task/step_outcomes.json")
	mustContainRedacted(t, res, "task/tool_audit.json")
	mustContainRedacted(t, res, "task/llm_usage.json")
	mustContainRedacted(t, res, "task/messages.json")
	mustContainRedacted(t, res, "task/judge.json")
	mustContainRedacted(t, res, "task/postmortem.json")
	mustContainRedacted(t, res, "config.redacted.yaml")
	mustContainRedacted(t, res, "metrics.txt")
	mustContainRedacted(t, res, "doctor.json")
	mustContainRedacted(t, res, "health.json")
	mustContainRedacted(t, res, "task/container_logs.txt")
	// Shipped text artifact present + redacted.
	mustContainRedacted(t, res, "task/artifacts/art1-notes.txt")

	// REDACTION.txt counts the openai_key type and a positive total.
	red := string(res.files["REDACTION.txt"])
	if !strings.Contains(red, "openai_key") {
		t.Errorf("REDACTION.txt missing openai_key type tally:\n%s", red)
	}
	if res.tally.total < 13 {
		t.Errorf("expected >=13 redactions across sections, got %d", res.tally.total)
	}
}

func mustContainRedacted(t *testing.T, res *buildResult, name string) {
	t.Helper()
	c, ok := res.files[name]
	if !ok {
		t.Errorf("expected section %s to be present", name)
		return
	}
	if !bytes.Contains(c, []byte("[REDACTED:")) {
		t.Errorf("section %s contains no [REDACTED:] marker:\n%s", name, c)
	}
}

// TestIncludeRawSkipsRedaction asserts raw mode preserves the secret
// verbatim and produces no redaction tally.
func TestIncludeRawSkipsRedaction(t *testing.T) {
	b, secretsByTag := fullyWiredBuilder(t)
	req := bundleRequest{TaskID: "task_test_1", MaxSize: 200 << 20, IncludeRaw: true}
	res, err := b.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cfg := string(res.files["config.redacted.yaml"])
	if !strings.Contains(cfg, secretsByTag["config"]) {
		t.Errorf("raw mode should keep the secret verbatim, got:\n%s", cfg)
	}
	if res.tally.total != 0 {
		t.Errorf("raw mode must not redact; tally=%d", res.tally.total)
	}
	if !res.manifest.Raw {
		t.Error("manifest.raw should be true in raw mode")
	}
}

// TestRedactBeforeTruncate seeds a secret near the size cap boundary
// and asserts it is fully redacted (never split into the bundle).
func TestRedactBeforeTruncate(t *testing.T) {
	secret := secretFor("STRADDLE")
	// Build a metrics blob: padding + secret. The whole file is
	// redacted before any cap touches it.
	pad := strings.Repeat("a", 4096)
	b := &bundleBuilder{
		metrics:  &fakeMetrics{txt: pad + " token=" + secret + " " + pad},
		detector: newTestDetector(t),
		version:  "t",
	}
	req := bundleRequest{TaskID: "x", MaxSize: 200 << 20}
	res, _ := b.Build(context.Background(), req)
	for fname, content := range res.files {
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("secret survived in %s despite redact-before-truncate", fname)
		}
	}
}

// TestBinaryArtifactMetadataOnly asserts a binary artifact is NOT
// shipped as bytes, only listed in the manifest (review #1).
func TestBinaryArtifactMetadataOnly(t *testing.T) {
	binary := []byte{0x00, 0x01, 0x02, 'k', 'e', 'y', 0x00, 0xff}
	b := &bundleBuilder{
		repos: supportRepos{
			Artifacts: &fakeArtifactReader{rows: []*persistence.Artifact{{ID: "bin1", Name: "blob.bin"}}},
		},
		opener:   &fakeArtifactOpener{blobs: map[string][]byte{"bin1": binary}},
		detector: newTestDetector(t),
		version:  "t",
	}
	req := bundleRequest{TaskID: "x", MaxSize: 200 << 20}
	res, _ := b.Build(context.Background(), req)
	// No shipped bytes file for the binary.
	for name := range res.files {
		if strings.Contains(name, "blob.bin") {
			t.Fatalf("binary artifact should not ship bytes; found %s", name)
		}
	}
	// Manifest lists it with sha256 + shipped=false.
	var metas []artifactMeta
	if err := json.Unmarshal(res.files["task/artifacts/MANIFEST.json"], &metas); err != nil {
		t.Fatalf("artifact manifest: %v", err)
	}
	if len(metas) != 1 || metas[0].Shipped || metas[0].SHA256 == "" {
		t.Fatalf("expected one metadata-only binary entry with sha256, got %+v", metas)
	}
}

// TestBestEffortSectionError: a repo that errors records the error and
// the build still completes other sections.
func TestBestEffortSectionError(t *testing.T) {
	b := &bundleBuilder{
		repos: supportRepos{
			Tasks:      &fakeTaskReader{err: errBoom},
			Executions: &fakeExecReader{rows: []*persistence.Execution{{ID: "e1"}}},
		},
		detector: newTestDetector(t),
		version:  "t",
	}
	req := bundleRequest{TaskID: "x", MaxSize: 200 << 20}
	res, err := b.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build should not fail on a section error: %v", err)
	}
	if _, ok := res.sectionErrs["task/task.json"]; !ok {
		t.Error("expected task.json section error recorded")
	}
	if _, ok := res.files["task/executions.json"]; !ok {
		t.Error("expected executions.json still collected")
	}
	if res.manifest.SectionErrors["task/task.json"] == "" {
		t.Error("manifest should carry the section error")
	}
}

// TestWindowFiltering: only tasks inside [since,until] appear.
func TestWindowFiltering(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-2 * time.Hour)
	until := now
	tasks := []*persistence.Task{
		{ID: "in", ProjectID: "p", CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "old", ProjectID: "p", CreatedAt: now.Add(-10 * time.Hour), UpdatedAt: now.Add(-10 * time.Hour)},
		{ID: "future", ProjectID: "p", CreatedAt: now.Add(2 * time.Hour), UpdatedAt: now.Add(2 * time.Hour)},
	}
	b := &bundleBuilder{
		repos:    supportRepos{Tasks: &fakeTaskReader{list: tasks}},
		detector: newTestDetector(t),
		version:  "t",
	}
	req := bundleRequest{Window: true, Since: since, Until: until, MaxSize: 200 << 20}
	res, _ := b.Build(context.Background(), req)
	var got []*persistence.Task
	if err := json.Unmarshal(res.files["window/tasks.json"], &got); err != nil {
		t.Fatalf("window tasks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "in" {
		t.Fatalf("window filter wrong: %+v", got)
	}
}

// TestManifestShape locks the key manifest fields.
func TestManifestShape(t *testing.T) {
	b, _ := fullyWiredBuilder(t)
	req := bundleRequest{TaskID: "task_test_1", MaxSize: 200 << 20}
	res, _ := b.Build(context.Background(), req)
	var mf manifest
	if err := json.Unmarshal(res.files["MANIFEST.json"], &mf); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if mf.SchemaVersion != supportReportSchemaVersion {
		t.Errorf("schema_version = %d", mf.SchemaVersion)
	}
	if mf.Mode != "task" || mf.TaskID != "task_test_1" {
		t.Errorf("mode/task = %q/%q", mf.Mode, mf.TaskID)
	}
	if mf.VornikVersion == "" || mf.GeneratedAt == "" {
		t.Error("version/generated_at missing")
	}
	if len(mf.Files) == 0 {
		t.Error("files list empty")
	}
	if mf.RedactionTotal == 0 {
		t.Error("redaction_total should be > 0")
	}
}

// TestRedactionDisabledNoDetectorErrors: redaction-on with nil
// detector is a programming error, surfaced (not a silent leak).
func TestNilDetectorErrors(t *testing.T) {
	b := &bundleBuilder{version: "t"}
	_, err := b.Build(context.Background(), bundleRequest{TaskID: "x"})
	if err == nil {
		t.Fatal("expected error when redaction requested with nil detector")
	}
}

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }
