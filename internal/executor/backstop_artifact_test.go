package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/verifier"
)

func discardLogger() zerolog.Logger { return zerolog.Nop() }

func TestStubFilenameFromPattern(t *testing.T) {
	cases := map[string]string{
		"scan-*.md":     "scan-backstop-abcd1234.md",
		"*.patch":       "backstop-abcd1234.patch",
		"report-*.json": "report-backstop-abcd1234.json",
		"scan-*":        "", // no extension
		"a-*-b-*.md":    "", // multi-star
		"":              "",
		"   ":           "",
	}
	for pattern, want := range cases {
		got := stubFilenameFromPattern(pattern, "abcd1234ef")
		if got != want {
			t.Errorf("stubFilenameFromPattern(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestStubFilenameFromPattern_ShortExecID(t *testing.T) {
	// Exec ID shorter than 8 chars is used as-is.
	if got := stubFilenameFromPattern("scan-*.md", "abc"); got != "scan-backstop-abc.md" {
		t.Fatalf("short execID: %q", got)
	}
}

func TestAlreadyHasArtifact(t *testing.T) {
	arts := []*persistence.Artifact{
		nil,
		{Name: "scan-foo-2026-05-14.md"},
		{Name: "other.md"},
	}
	if !alreadyHasArtifact(arts, "scan-foo-2026-05-14.md") {
		t.Fatal("should find existing")
	}
	if alreadyHasArtifact(arts, "missing.md") {
		t.Fatal("should not match")
	}
	if alreadyHasArtifact(nil, "any") {
		t.Fatal("nil slice should not match")
	}
}

func TestSummariseAuditForBackstop(t *testing.T) {
	if got := summariseAuditForBackstop(nil); !strings.Contains(got, "no tool audit") {
		t.Fatalf("nil: %q", got)
	}

	entries := []*persistence.ToolAuditEntry{
		nil,
		{ToolName: "fetch", ToolInput: `{"url":"https://example.com"}`, ToolOutput: `{"status":429}`},
		{ToolName: "search", ToolInput: "q", ToolOutput: "ok"},
	}
	got := summariseAuditForBackstop(entries)
	if !strings.Contains(got, "fetch") || !strings.Contains(got, "429") {
		t.Fatalf("missing details: %q", got)
	}
}

func TestSummariseAuditForBackstop_TruncatesLongFields(t *testing.T) {
	bigInput := strings.Repeat("a", 1000)
	entries := []*persistence.ToolAuditEntry{
		{ToolName: "fetch", ToolInput: bigInput, ToolOutput: bigInput},
	}
	got := summariseAuditForBackstop(entries)
	if !strings.Contains(got, "(truncated)") {
		t.Fatalf("expected truncation marker: %q", got[:200])
	}
}

func TestSummariseAuditForBackstop_KeepsLastFive(t *testing.T) {
	entries := make([]*persistence.ToolAuditEntry, 8)
	for i := range entries {
		entries[i] = &persistence.ToolAuditEntry{
			ToolName:  "t",
			ToolInput: strings.Repeat("x", 10) + string(rune('A'+i)),
		}
	}
	got := summariseAuditForBackstop(entries)
	// Should see at most 5 bullets.
	if got == "" || strings.Count(got, "- `t`") > 5 {
		t.Fatalf("bullet count: %q", got)
	}
}

func TestTruncForStub(t *testing.T) {
	if got := truncForStub("short", 100); got != "short" {
		t.Fatalf("under: %q", got)
	}
	if got := truncForStub("alphabet soup", 5); got != "alpha…(truncated)" {
		t.Fatalf("over: %q", got)
	}
}

func TestCollectMissingPatterns(t *testing.T) {
	cfgs := []verifier.Config{
		{
			Name: "scan_min_entries",
			Type: "artifact_min_entries",
			Params: map[string]any{
				"artifact_pattern": "scan-*.md",
				"min":              1,
			},
		},
		{
			// Different verifier — not a missing-artifact one.
			Name: "no_rate_limit_blocks",
			Type: "no_status_429_in_audit",
		},
	}
	violations := []verifier.Violation{
		{
			VerifierName: "scan_min_entries",
			Type:         "artifact_min_entries",
			Severity:     verifier.SeverityFail,
			Detail:       `no artifact matched pattern "scan-*.md"; expected at least one with ≥1 items`,
		},
		{
			// Warn-tier — must be skipped.
			VerifierName: "scan_min_entries",
			Type:         "artifact_min_entries",
			Severity:     verifier.SeverityWarn,
			Detail:       `no artifact matched pattern "scan-*.md"`,
		},
		{
			// Wrong verifier type — must be skipped.
			VerifierName: "no_rate_limit_blocks",
			Type:         "no_status_429_in_audit",
			Severity:     verifier.SeverityFail,
			Detail:       "rate-limit detected",
		},
		{
			// Right type but different detail — must be skipped (not "no artifact matched").
			VerifierName: "scan_min_entries",
			Type:         "artifact_min_entries",
			Severity:     verifier.SeverityFail,
			Detail:       `artifact "scan-foo.md" has 0 list-item line(s); requires ≥1`,
		},
	}
	got := collectMissingPatterns(cfgs, violations)
	if len(got) != 1 {
		t.Fatalf("expected 1 hit, got %d: %+v", len(got), got)
	}
	if got[0].pattern != "scan-*.md" {
		t.Fatalf("pattern: %q", got[0].pattern)
	}
	if got[0].verifierName != "scan_min_entries" {
		t.Fatalf("name: %q", got[0].verifierName)
	}
}

func TestCollectMissingPatterns_NoMatchingConfig(t *testing.T) {
	// Violation references a verifier name that isn't in cfgs.
	cfgs := []verifier.Config{}
	violations := []verifier.Violation{
		{
			VerifierName: "ghost",
			Type:         "artifact_min_entries",
			Severity:     verifier.SeverityFail,
			Detail:       `no artifact matched pattern "x-*.md"`,
		},
	}
	if got := collectMissingPatterns(cfgs, violations); len(got) != 0 {
		t.Fatalf("expected no hits: %+v", got)
	}
}

func TestBuildStubBody(t *testing.T) {
	body := buildStubBody(
		"scan_min_entries",
		`no artifact matched pattern "scan-*.md"`,
		"- `fetch`: input=u output=ok\n",
		"task-1", "exec-1",
	)
	for _, want := range []string{
		"# Backstop scan record",
		"scan_min_entries",
		"no artifact matched",
		"- `fetch`",
		"task_id: task-1",
		"execution_id: exec-1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
	// Body must have at least one markdown list item ("- ") so the
	// next iteration's artifact_min_entries with min:1 passes.
	if !strings.Contains(body, "\n- ") {
		t.Fatal("body must contain a list-item bullet for the next iteration's verifier")
	}
}

func TestBuildStubBody_EmptyAuditSummary(t *testing.T) {
	body := buildStubBody("v", "detail", "", "t", "e")
	if !strings.Contains(body, "no tool audit recorded") {
		t.Fatalf("empty audit fallback missing: %s", body)
	}
}

// ---- Integration with persistStubArtifact ----

type capturingArtifactRepo struct {
	created []*persistence.Artifact
}

func (c *capturingArtifactRepo) Create(_ context.Context, a *persistence.Artifact) error {
	c.created = append(c.created, a)
	return nil
}
func (c *capturingArtifactRepo) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return nil, nil
}
func (c *capturingArtifactRepo) GetByHash(_ context.Context, _ string) (*persistence.Artifact, error) {
	return nil, nil
}

func TestPersistStubArtifact_WritesFileAndCreatesRow(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	repo := &capturingArtifactRepo{}
	e := &Executor{artifactRepo: repo}
	task := &persistence.Task{ID: "task-x", ProjectID: "p-x"}
	execution := &persistence.Execution{ID: "exec-x"}

	err := e.persistStubArtifact(context.Background(), task, execution, "scan", "scan-backstop-foo.md", "BODY")
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("expected 1 created artifact, got %d", len(repo.created))
	}
	got := repo.created[0]
	if got.Name != "scan-backstop-foo.md" {
		t.Fatalf("name: %q", got.Name)
	}
	if got.TaskID == nil || *got.TaskID != "task-x" {
		t.Fatalf("task id: %+v", got.TaskID)
	}
	// File written to disk under TMPDIR/vornik-backstop/exec-x/.
	on := got.StoragePath
	if !strings.Contains(on, "vornik-backstop") || !strings.Contains(on, "exec-x") {
		t.Fatalf("path layout: %s", on)
	}
	body, rerr := os.ReadFile(on)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(body) != "BODY" {
		t.Fatalf("body: %q", body)
	}
}

// fakeArtifactStore captures Store() calls so we can assert that the
// artifactStore path is taken when it's wired (vs the artifactRepo
// fallback).
type fakeArtifactStore struct {
	storeCalls int
	storeErr   error
}

func (f *fakeArtifactStore) Store(_ context.Context, projectID, executionID, taskID, name, sourcePath string) (*persistence.Artifact, error) {
	f.storeCalls++
	if f.storeErr != nil {
		return nil, f.storeErr
	}
	return &persistence.Artifact{
		ID:          "stored_" + name,
		ProjectID:   projectID,
		ExecutionID: &executionID,
		TaskID:      &taskID,
		Name:        name,
		StoragePath: sourcePath,
	}, nil
}

// Retrieve satisfies the ArtifactStore interface for tests that
// don't need to read; backstop tests verify the Store side only.
func (f *fakeArtifactStore) Retrieve(_ context.Context, _ string) ([]byte, error) {
	return nil, errors.New("fakeArtifactStore: Retrieve not implemented in this test")
}

func TestPersistStubArtifact_UsesArtifactStoreWhenWired(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	store := &fakeArtifactStore{}
	e := &Executor{artifactStore: store}
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	execution := &persistence.Execution{ID: "e"}
	if err := e.persistStubArtifact(context.Background(), task, execution, "s", "stub.md", "B"); err != nil {
		t.Fatal(err)
	}
	if store.storeCalls != 1 {
		t.Fatalf("artifactStore.Store not called: %d", store.storeCalls)
	}
}

func TestPersistStubArtifact_StoreErrorPropagates(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	store := &fakeArtifactStore{storeErr: assertErr("disk full")}
	e := &Executor{artifactStore: store}
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	execution := &persistence.Execution{ID: "e"}
	if err := e.persistStubArtifact(context.Background(), task, execution, "s", "stub.md", "B"); err == nil {
		t.Fatal("want error from store failure")
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

// TestWriteBackstopArtifacts_FullFlow exercises the orchestrator with
// a missing-pattern violation in the slice; it must write one stub
// and record it via the artifact repo.
func TestWriteBackstopArtifacts_FullFlow(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	repo := &capturingArtifactRepo{}
	e := &Executor{artifactRepo: repo, logger: discardLogger()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	execution := &persistence.Execution{ID: "execAAAA1234"}

	cfgs := []verifier.Config{
		{
			Name: "scan_min_entries",
			Type: "artifact_min_entries",
			Params: map[string]any{
				"artifact_pattern": "scan-*.md",
				"min":              1,
			},
		},
	}
	in := verifier.Input{
		Artifacts: nil,
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "fetch", ToolOutput: `{"status_code":429}`},
		},
	}
	violations := []verifier.Violation{
		{
			VerifierName: "scan_min_entries",
			Type:         "artifact_min_entries",
			Severity:     verifier.SeverityFail,
			Detail:       `no artifact matched pattern "scan-*.md"; expected at least one with ≥1 items`,
		},
	}
	e.writeBackstopArtifacts(context.Background(), task, execution, "scan", cfgs, in, violations)
	if len(repo.created) != 1 {
		t.Fatalf("expected 1 backstop artifact, got %d", len(repo.created))
	}
	if !strings.HasPrefix(repo.created[0].Name, "scan-backstop-") {
		t.Fatalf("name: %q", repo.created[0].Name)
	}
}

func TestWriteBackstopArtifacts_NilGuards(t *testing.T) {
	// nil receiver / task / execution exit early without panic.
	var nilE *Executor
	nilE.writeBackstopArtifacts(context.Background(), nil, nil, "", nil, verifier.Input{}, nil)

	e := &Executor{logger: discardLogger()}
	e.writeBackstopArtifacts(context.Background(), nil, nil, "", nil, verifier.Input{}, nil)
}

func TestWriteBackstopArtifacts_NoMissingPatternsIsNoop(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	repo := &capturingArtifactRepo{}
	e := &Executor{artifactRepo: repo, logger: discardLogger()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	execution := &persistence.Execution{ID: "execAAAA"}
	// Violation without "no artifact matched" → no stub.
	violations := []verifier.Violation{
		{
			VerifierName: "scan_min_entries",
			Type:         "artifact_min_entries",
			Severity:     verifier.SeverityFail,
			Detail:       `artifact "scan-foo.md" has 0 list-item line(s); requires ≥1`,
		},
	}
	e.writeBackstopArtifacts(context.Background(), task, execution, "scan", nil, verifier.Input{}, violations)
	if len(repo.created) != 0 {
		t.Fatalf("expected no stubs, got %d", len(repo.created))
	}
}

func TestWriteBackstopArtifacts_SkipsWhenStubAlreadyExists(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	repo := &capturingArtifactRepo{}
	e := &Executor{artifactRepo: repo, logger: discardLogger()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	execution := &persistence.Execution{ID: "execAAAA"}
	cfgs := []verifier.Config{
		{
			Name: "scan_min_entries",
			Type: "artifact_min_entries",
			Params: map[string]any{
				"artifact_pattern": "scan-*.md",
				"min":              1,
			},
		},
	}
	expectedName := stubFilenameFromPattern("scan-*.md", execution.ID)
	in := verifier.Input{
		Artifacts: []*persistence.Artifact{{Name: expectedName}},
	}
	violations := []verifier.Violation{
		{
			VerifierName: "scan_min_entries",
			Type:         "artifact_min_entries",
			Severity:     verifier.SeverityFail,
			Detail:       `no artifact matched pattern "scan-*.md"`,
		},
	}
	e.writeBackstopArtifacts(context.Background(), task, execution, "scan", cfgs, in, violations)
	if len(repo.created) != 0 {
		t.Fatalf("idempotency: expected no new stub, got %d", len(repo.created))
	}
}

func TestPersistStubArtifact_NoRepo_NoRowButFileStillWritten(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	e := &Executor{} // no artifactRepo, no artifactStore
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	execution := &persistence.Execution{ID: "e1"}

	err := e.persistStubArtifact(context.Background(), task, execution, "s", "n.md", "B")
	if err != nil {
		t.Fatal(err)
	}
	// File exists even without a repo.
	matches, _ := filepath.Glob(filepath.Join(tmp, "vornik-backstop", "e1", "n.md"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 file, got %v", matches)
	}
}
