package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taintlineage"
)

// TestPersistToolAuditFromResult_ClassifiesTaint verifies the third return
// value classifies the step's untrusted-content taint from the tool audit.
func TestPersistToolAuditFromResult_ClassifiesTaint(t *testing.T) {
	repo := &stubAuditRepo{}
	e := &Executor{auditRepo: repo, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	body := []byte(`{"toolAudit":[
		{"tool":"file_read","input":"{}"},
		{"tool":"web_fetch","input":"{\"url\":\"https://a.example/x\"}"}
	]}`)
	_, _, taint := e.persistToolAuditFromResult(context.Background(), task, exec, "step-1", body)
	if taint.MaxSeverity != taintlineage.SeverityHigh {
		t.Fatalf("max severity = %v, want High", taint.MaxSeverity)
	}
	if !taint.Used || !taint.RequiresReview {
		t.Fatalf("used/requires_review not set: %+v", taint)
	}
	if len(taint.Sources) != 1 || taint.Sources[0].Tool != "web_fetch" {
		t.Fatalf("sources = %+v, want one web_fetch entry", taint.Sources)
	}
}

// TestTaintStampFromStep_MarshalsBlob verifies the executor boundary marshals
// StepTaint.Sources into a JSONB blob (like HallucinationSignals) and sets the
// bools; an untainted step yields a zero stamp.
func TestTaintStampFromStep_MarshalsBlob(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}

	st := taintlineage.Classify([]taintlineage.ToolCall{
		{Tool: "web_fetch", Input: `{"url":"https://a.example"}`},
	})
	stamp := e.taintStampFromStep(st)
	if !stamp.UntrustedContentUsed || !stamp.RequiresReview {
		t.Fatalf("stamp bools wrong: %+v", stamp)
	}
	// Blob is the M1 SourcesBlob shape: capped list + full_hash. Round-trip via
	// StepTaintFromBlob (the reader the gate uses).
	rt := taintlineage.StepTaintFromBlob(stamp.UntrustedSources, stamp.RequiresReview)
	if len(rt.Sources) != 1 || rt.Sources[0].Tool != "web_fetch" {
		t.Fatalf("sources blob did not round-trip: %s", stamp.UntrustedSources)
	}
	if rt.FullHash == "" || rt.FullHash != st.FullHash {
		t.Fatalf("full_hash must round-trip (M1): got %q want %q", rt.FullHash, st.FullHash)
	}

	// Untainted step → zero stamp (all false/nil).
	zero := e.taintStampFromStep(taintlineage.Classify([]taintlineage.ToolCall{{Tool: "file_read", Input: "{}"}}))
	if zero.UntrustedContentUsed || zero.RequiresReview || zero.UntrustedSources != nil {
		t.Fatalf("untainted step must yield a zero stamp, got %+v", zero)
	}
}
