package agentbench

import "testing"

// Regression, 2026-08-16. PathCoverage was published as a headline accuracy
// figure with no way to see its sample size. On the qwen-local-fixed arm it
// averaged 16 executions out of 139 — because GrantProbe deliberately refuses
// to score executions that made no grant request, and an entire workflow
// family (dp-*) never makes one.
//
// PathCoverageDefined answered "is there a number"; nothing answered "from how
// many", so 0.548 read as a property of the task set rather than of 11% of it.
func TestRollup_ReportsCoverageSampleSize(t *testing.T) {
	// Two scored verdicts and one execution that produced none — the shape the
	// arm actually has.
	recs := []ExecutionRecord{
		{TaskID: "a", ExecutionID: "e1", Succeeded: true, Verdicts: []Verdict{
			{Probe: grantProbeName, ExecutionID: "e1", TaskID: "a", PathCoverage: 0.4},
		}},
		{TaskID: "b", ExecutionID: "e2", Succeeded: true, Verdicts: []Verdict{
			{Probe: grantProbeName, ExecutionID: "e2", TaskID: "b", PathCoverage: 0.6},
		}},
		{TaskID: "c", ExecutionID: "e3", Succeeded: true},
	}
	r := BuildRollup("arm-test", recs)

	if !r.Accuracy.PathCoverageDefined {
		t.Fatal("coverage should be defined with two scored verdicts")
	}
	if r.Accuracy.PathCoverageN != 2 {
		t.Errorf("PathCoverageN = %d, want 2 — the unscored execution must not inflate the denominator",
			r.Accuracy.PathCoverageN)
	}
	if got := r.Accuracy.PathCoverage; got < 0.49 || got > 0.51 {
		t.Errorf("PathCoverage = %v, want ~0.5 (mean of the SCORED verdicts)", got)
	}

	// No scored verdicts: the count must be zero, not a stale or invented one.
	bare := BuildRollup("arm-test", []ExecutionRecord{{TaskID: "c", ExecutionID: "e3", Succeeded: true}})
	if bare.Accuracy.PathCoverageDefined || bare.Accuracy.PathCoverageN != 0 {
		t.Errorf("no verdicts: defined=%v N=%d, want false/0",
			bare.Accuracy.PathCoverageDefined, bare.Accuracy.PathCoverageN)
	}
}

// Companion to the coverage-denominator regression, 2026-08-16. Conformance is
// a ratio over steps that PRODUCED OUTPUT; steps that crashed, timed out,
// refused or exhausted leave the denominator by design (a crashed container is
// a reliability fact, not a schema fact). The probe kept them apart correctly —
// the rollup published only the ratio, so the reliability half vanished.
//
// On the qwen-local-fixed arm: 825 terminal steps, 479 judged, 346 (41.9%)
// producing nothing. "Conformance 0.912" reads as near-perfect until you know
// it describes 58% of the steps.
func TestRollup_ReportsConformanceHalves(t *testing.T) {
	recs := []ExecutionRecord{
		{TaskID: "a", ExecutionID: "e1", Succeeded: true, Verdicts: []Verdict{{
			Probe: SchemaProbe{}.Name(), ExecutionID: "e1", TaskID: "a",
			Schema: &SchemaVerdict{
				Terminal: 10, Judged: 6, NoOutput: 4,
				SchemaConformance: 1.0, SchemaConformanceDefined: true,
			},
		}}},
		// An execution where EVERY step produced nothing: contributes no ratio,
		// but is precisely the case a reader must see.
		{TaskID: "b", ExecutionID: "e2", Succeeded: false, Verdicts: []Verdict{{
			Probe: SchemaProbe{}.Name(), ExecutionID: "e2", TaskID: "b",
			Schema: &SchemaVerdict{Terminal: 5, Judged: 0, NoOutput: 5},
		}}},
	}
	r := BuildRollup("arm-test", recs)

	if r.Accuracy.SchemaJudged != 6 {
		t.Errorf("SchemaJudged = %d, want 6", r.Accuracy.SchemaJudged)
	}
	if r.Accuracy.SchemaNoOutput != 9 {
		t.Errorf("SchemaNoOutput = %d, want 9 — the all-crashed execution must still be counted",
			r.Accuracy.SchemaNoOutput)
	}
	if !r.Accuracy.SchemaConformanceDefined || r.Accuracy.SchemaConformance != 1.0 {
		t.Errorf("conformance = %v/%v, want a defined 1.0 over the judged steps only",
			r.Accuracy.SchemaConformance, r.Accuracy.SchemaConformanceDefined)
	}
}
