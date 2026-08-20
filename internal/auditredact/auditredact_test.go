package auditredact

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// syntheticKey has the SHAPE of a Google API key (AIza + 35 chars) and is not a
// real credential. Assertions compare markers, never the value.
const syntheticKey = "AIzaSyDUMMYKEYFORTESTINGONLY0123456789A"

// conflictRepo emulates the real INSERT … ON CONFLICT (id) DO NOTHING: the
// first write for an id wins and later writes for that id are dropped. That
// clause is the whole reason this decorator exists, so the fake must model it
// rather than a plain map overwrite.
type conflictRepo struct {
	rows map[string]*persistence.ToolAuditEntry
}

func newConflictRepo() *conflictRepo {
	return &conflictRepo{rows: map[string]*persistence.ToolAuditEntry{}}
}

func (r *conflictRepo) Log(_ context.Context, e *persistence.ToolAuditEntry) error {
	if _, exists := r.rows[e.ID]; exists {
		return nil // ON CONFLICT DO NOTHING
	}
	cp := *e
	r.rows[e.ID] = &cp
	return nil
}

func (r *conflictRepo) List(context.Context, persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return nil, nil
}

func (r *conflictRepo) CountByTool(context.Context, string) (map[string]int64, error) {
	return nil, nil
}

func (r *conflictRepo) ToolLatencyP95ByProjectTool(context.Context, time.Time) ([]persistence.ToolLatencyStat, error) {
	return nil, nil
}

type recordingAudit struct {
	events []persistence.SecretRedactionEvent
}

func (a *recordingAudit) Record(_ context.Context, ev []persistence.SecretRedactionEvent) error {
	a.events = append(a.events, ev...)
	return nil
}

func (a *recordingAudit) CountByTask(context.Context, string) (map[string]int, int, error) {
	return nil, 0, nil
}

func detector(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{Patterns: secrets.DefaultPatterns()})
	if err != nil {
		t.Fatalf("build detector: %v", err)
	}
	return d
}

func entry(id, tool, input, output string) *persistence.ToolAuditEntry {
	return &persistence.ToolAuditEntry{
		ID: id, ProjectID: "p1", TaskID: "t1", ExecutionID: "e1",
		ToolName: tool, ToolInput: input, ToolOutput: output,
	}
}

// THE REGRESSION. Two writers share the agent-supplied audit_id: the realtime
// POST handler (which scanned nothing) and the post-step batch (which scanned).
// With ON CONFLICT DO NOTHING the realtime row landed first and WON, so the
// redacted row was silently discarded and the credential sat in the table at
// rest — while secret_redaction_audit recorded findings, making the control
// look healthy. Redacting at the seam means it no longer matters which writer
// wins the race.
func TestRaceEitherWriterStoresRedacted(t *testing.T) {
	for _, order := range []string{"realtime-first", "batch-first"} {
		t.Run(order, func(t *testing.T) {
			inner := newConflictRepo()
			repo := New(inner, detector(t), nil, &recordingAudit{}, nil, nil)

			realtime := entry("audit-1", "mcp__scraper__web_fetch", "{}", "page footer key="+syntheticKey)
			batch := entry("audit-1", "mcp__scraper__web_fetch", "{}", "page footer key="+syntheticKey)

			first, second := realtime, batch
			if order == "batch-first" {
				first, second = batch, realtime
			}
			if err := repo.Log(context.Background(), first); err != nil {
				t.Fatalf("Log: %v", err)
			}
			if err := repo.Log(context.Background(), second); err != nil {
				t.Fatalf("Log: %v", err)
			}

			stored := inner.rows["audit-1"]
			if stored == nil {
				t.Fatal("no row stored")
			}
			if strings.Contains(stored.ToolOutput, syntheticKey) {
				t.Error("the credential is at rest in tool_audit_log — the row that won the race was unredacted")
			}
			if !strings.Contains(stored.ToolOutput, "[REDACTED:google_api_key]") {
				t.Errorf("stored row must carry the typed marker; got %q", stored.ToolOutput)
			}
		})
	}
}

func TestRedactsInputAndOutput(t *testing.T) {
	inner := newConflictRepo()
	repo := New(inner, detector(t), nil, &recordingAudit{}, nil, nil)
	if err := repo.Log(context.Background(),
		entry("a1", "run_shell", "export K="+syntheticKey, "echoed "+syntheticKey)); err != nil {
		t.Fatalf("Log: %v", err)
	}
	got := inner.rows["a1"]
	if strings.Contains(got.ToolInput, syntheticKey) {
		t.Error("tool_input was stored raw")
	}
	if strings.Contains(got.ToolOutput, syntheticKey) {
		t.Error("tool_output was stored raw")
	}
}

func TestRecordsRedactionEvents(t *testing.T) {
	inner := newConflictRepo()
	audit := &recordingAudit{}
	repo := New(inner, detector(t), nil, audit, nil, nil)
	if err := repo.Log(context.Background(), entry("a1", "run_shell", "", "k="+syntheticKey)); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(audit.events) == 0 {
		t.Fatal("redaction events must be recorded — the task-detail badge and scan-history CLI read them")
	}
	var found bool
	for _, ev := range audit.events {
		if ev.FindingType == "google_api_key" {
			found = true
			if ev.Checkpoint != secrets.CheckpointToolAudit {
				t.Errorf("checkpoint = %q, want %q", ev.Checkpoint, secrets.CheckpointToolAudit)
			}
			if ev.Source != "live" {
				t.Errorf("source = %q, want live (scan is the retro-sweep)", ev.Source)
			}
			if ev.TaskID != "t1" || ev.ProjectID != "p1" {
				t.Errorf("event lost its task context: %+v", ev)
			}
		}
	}
	if !found {
		t.Error("no google_api_key event recorded")
	}
}

// Detect keeps audit fidelity by storing the raw value, and is a deliberate
// operator choice. It must still RECORD, or an operator who chose detect gets
// no signal at all.
func TestDetectStoresRawButStillRecords(t *testing.T) {
	inner := newConflictRepo()
	audit := &recordingAudit{}
	actions := map[string]secrets.Action{secrets.CheckpointToolAudit: secrets.ActionDetect}
	repo := New(inner, detector(t), actions, audit, nil, nil)
	if err := repo.Log(context.Background(), entry("a1", "run_shell", "", "k="+syntheticKey)); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if !strings.Contains(inner.rows["a1"].ToolOutput, syntheticKey) {
		t.Error("detect mode must not rewrite the row")
	}
	if len(audit.events) == 0 {
		t.Error("detect mode must still record findings")
	}
}

// Block degrades to Redact: dropping the audit row would lose more signal than
// redacting it does. This mirrors the behaviour the executor's scanner had.
func TestBlockDegradesToRedact(t *testing.T) {
	inner := newConflictRepo()
	actions := map[string]secrets.Action{secrets.CheckpointToolAudit: secrets.ActionBlock}
	repo := New(inner, detector(t), actions, &recordingAudit{}, nil, nil)
	if err := repo.Log(context.Background(), entry("a1", "run_shell", "", "k="+syntheticKey)); err != nil {
		t.Fatalf("Log must not fail on block — the row is redacted, not dropped: %v", err)
	}
	if inner.rows["a1"] == nil {
		t.Fatal("the row must still be stored")
	}
	if strings.Contains(inner.rows["a1"].ToolOutput, syntheticKey) {
		t.Error("block must degrade to redact, not to pass-through")
	}
}

// The provenance exemption: for a trusted-output tool the OUTPUT is the
// daemon-proxied response the agent cannot forge, so its HEURISTIC findings are
// dropped (e.g. a PageDrop viewing password). Strong patterns are still
// redacted, and the agent-supplied INPUT is never exempt — otherwise the
// exemption is a labelled exfil channel.
func TestTrustedOutputToolExemption(t *testing.T) {
	inner := newConflictRepo()
	repo := New(inner, detector(t), nil, &recordingAudit{}, []string{"mcp__pagedrop__pagedrop_publish"}, nil)

	// A generic_kv-shaped operator value in a trusted tool's output survives.
	if err := repo.Log(context.Background(),
		entry("a1", "mcp__pagedrop__pagedrop_publish_page", "", "password=hunter2-correct-horse")); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if !strings.Contains(inner.rows["a1"].ToolOutput, "hunter2-correct-horse") {
		t.Error("a heuristic finding in a trusted tool's OUTPUT must be exempt")
	}

	// A strong credential in the same trusted output is still redacted.
	if err := repo.Log(context.Background(),
		entry("a2", "mcp__pagedrop__pagedrop_publish_page", "", "key="+syntheticKey)); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if strings.Contains(inner.rows["a2"].ToolOutput, syntheticKey) {
		t.Error("the exemption must never rescue a strong credential pattern")
	}

	// The INPUT is never exempt, even for a trusted tool.
	if err := repo.Log(context.Background(),
		entry("a3", "mcp__pagedrop__pagedrop_publish_page", "password=hunter2-correct-horse", "")); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if strings.Contains(inner.rows["a3"].ToolInput, "hunter2-correct-horse") {
		t.Error("the agent-supplied INPUT must never be exempt — that would be a labelled exfil channel")
	}
}

// An untrusted tool gets no exemption.
func TestUntrustedToolKeepsHeuristicRedaction(t *testing.T) {
	inner := newConflictRepo()
	repo := New(inner, detector(t), nil, &recordingAudit{}, []string{"mcp__pagedrop__pagedrop_publish"}, nil)
	if err := repo.Log(context.Background(),
		entry("a1", "mcp__scraper__web_fetch", "", "password=hunter2-correct-horse")); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if strings.Contains(inner.rows["a1"].ToolOutput, "hunter2-correct-horse") {
		t.Error("an untrusted tool's output must not get the heuristic exemption")
	}
}

// Nil detector is a pass-through, so CE paths and tests that never wire secrets
// keep working rather than losing their audit trail.
func TestNilDetectorPassesThrough(t *testing.T) {
	inner := newConflictRepo()
	repo := New(inner, nil, nil, nil, nil, nil)
	if err := repo.Log(context.Background(), entry("a1", "run_shell", "in", "out")); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if inner.rows["a1"].ToolOutput != "out" {
		t.Error("a nil detector must not alter the entry")
	}
}

// A clean entry must not be rewritten at all — no marker, no copy surprises.
func TestCleanEntryUntouched(t *testing.T) {
	inner := newConflictRepo()
	audit := &recordingAudit{}
	repo := New(inner, detector(t), nil, audit, nil, nil)
	if err := repo.Log(context.Background(), entry("a1", "run_shell", "ls -la", "total 4")); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if inner.rows["a1"].ToolOutput != "total 4" || inner.rows["a1"].ToolInput != "ls -la" {
		t.Error("a clean entry must pass through byte-identical")
	}
	if len(audit.events) != 0 {
		t.Error("a clean entry must record nothing")
	}
}

// The decorator must not mutate the caller's struct — the batch path reuses its
// parsed entries for other bookkeeping.
func TestDoesNotMutateCallerEntry(t *testing.T) {
	inner := newConflictRepo()
	repo := New(inner, detector(t), nil, &recordingAudit{}, nil, nil)
	e := entry("a1", "run_shell", "", "k="+syntheticKey)
	if err := repo.Log(context.Background(), e); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if !strings.Contains(e.ToolOutput, syntheticKey) {
		t.Error("the caller's entry was mutated in place")
	}
}

// The trusted-tool prefix must require a delimiter. Without it a look-alike
// name ("…_publisher_evil" against the "…_publish" prefix) would inherit the
// heuristic exemption, which is a trivially forgeable way to get an exemption
// the operator never granted.
func TestTrustedPrefixRequiresDelimiter(t *testing.T) {
	inner := newConflictRepo()
	repo := New(inner, detector(t), nil, &recordingAudit{}, []string{"mcp__pagedrop__pagedrop_publish"}, nil)
	if err := repo.Log(context.Background(),
		entry("a1", "mcp__pagedrop__pagedrop_publisher_evil", "", "password=hunter2-correct-horse")); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if strings.Contains(inner.rows["a1"].ToolOutput, "hunter2-correct-horse") {
		t.Error("a look-alike tool name must not inherit the trusted-output exemption")
	}
}

// The read methods must pass through untouched. A decorator that quietly
// returned nil here would empty the admin audit views while every write kept
// working — the kind of failure nobody notices until they go looking for a row.
func TestReadMethodsDelegate(t *testing.T) {
	inner := &stubReader{
		rows:    []*persistence.ToolAuditEntry{{ID: "a1", ToolName: "run_shell"}},
		counts:  map[string]int64{"run_shell": 7},
		latency: []persistence.ToolLatencyStat{{ToolName: "run_shell"}},
	}
	repo := New(inner, detector(t), nil, nil, nil, nil)

	rows, err := repo.List(context.Background(), persistence.ToolAuditFilter{})
	if err != nil || len(rows) != 1 || rows[0].ID != "a1" {
		t.Errorf("List did not delegate: rows=%v err=%v", rows, err)
	}
	counts, err := repo.CountByTool(context.Background(), "e1")
	if err != nil || counts["run_shell"] != 7 {
		t.Errorf("CountByTool did not delegate: %v err=%v", counts, err)
	}
	stats, err := repo.ToolLatencyP95ByProjectTool(context.Background(), time.Time{})
	if err != nil || len(stats) != 1 {
		t.Errorf("ToolLatencyP95ByProjectTool did not delegate: %v err=%v", stats, err)
	}
}

type stubReader struct {
	rows    []*persistence.ToolAuditEntry
	counts  map[string]int64
	latency []persistence.ToolLatencyStat
}

func (s *stubReader) Log(context.Context, *persistence.ToolAuditEntry) error { return nil }
func (s *stubReader) List(context.Context, persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return s.rows, nil
}
func (s *stubReader) CountByTool(context.Context, string) (map[string]int64, error) {
	return s.counts, nil
}
func (s *stubReader) ToolLatencyP95ByProjectTool(context.Context, time.Time) ([]persistence.ToolLatencyStat, error) {
	return s.latency, nil
}

// A failing redaction-audit write must not fail the tool-audit write: the audit
// row is a side channel, and losing the whole row to save its bookkeeping would
// trade a large signal for a small one.
func TestAuditRecordErrorDoesNotFailTheWrite(t *testing.T) {
	inner := newConflictRepo()
	repo := New(inner, detector(t), nil, failingAudit{}, nil, nil)
	if err := repo.Log(context.Background(), entry("a1", "run_shell", "", "k="+syntheticKey)); err != nil {
		t.Fatalf("a failing audit recorder must not fail the write: %v", err)
	}
	if inner.rows["a1"] == nil {
		t.Fatal("the row must still be stored")
	}
	if strings.Contains(inner.rows["a1"].ToolOutput, syntheticKey) {
		t.Error("and it must still be redacted")
	}
}

type failingAudit struct{}

func (failingAudit) Record(context.Context, []persistence.SecretRedactionEvent) error {
	return context.DeadlineExceeded
}
func (failingAudit) CountByTask(context.Context, string) (map[string]int, int, error) {
	return nil, 0, nil
}

// With a logger wired, the redaction and the failed-audit paths must log rather
// than panic — and the log line must never carry the secret itself, only its
// type and count.
func TestLoggingPathsDoNotLeakTheSecret(t *testing.T) {
	var buf bytes.Buffer
	lg := zerolog.New(&buf)
	inner := newConflictRepo()
	repo := New(inner, detector(t), nil, failingAudit{}, nil, &lg)
	if err := repo.Log(context.Background(), entry("a1", "run_shell", "", "k="+syntheticKey)); err != nil {
		t.Fatalf("Log: %v", err)
	}
	out := buf.String()
	if out == "" {
		t.Fatal("a redaction must be logged")
	}
	if strings.Contains(out, syntheticKey) {
		t.Error("the log line leaked the credential — it may carry the TYPE and COUNT, never the value")
	}
	if !strings.Contains(out, "google_api_key") {
		t.Errorf("the log must name the finding type for forensics; got %q", out)
	}
}
