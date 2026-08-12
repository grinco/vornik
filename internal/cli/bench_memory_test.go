package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/membench"
)

// CLI surface for `vornikctl bench memory`. The package-level flag variables are
// shared state, so every test that sets one restores it.

func withFlags(t *testing.T) {
	t.Helper()
	saved := struct {
		dataset, path, corpus, system, profile, answer, judge, db, confirm string
	}{
		benchDataset, benchDatasetPath, benchCorpusDir, benchSystem,
		benchProfile, benchAnswerModel, benchJudgeModel, benchDatabase, benchConfirmWipe,
	}
	t.Cleanup(func() {
		benchDataset, benchDatasetPath, benchCorpusDir, benchSystem = saved.dataset, saved.path, saved.corpus, saved.system
		benchProfile, benchAnswerModel, benchJudgeModel = saved.profile, saved.answer, saved.judge
		benchDatabase, benchConfirmWipe = saved.db, saved.confirm
	})
}

// TestApplyProfile_FillsUnsetModels — a profile is a convenience preset.
func TestApplyProfile_FillsUnsetModels(t *testing.T) {
	withFlags(t)
	for _, name := range []string{"local", "judged", "cloud"} {
		benchProfile, benchAnswerModel, benchJudgeModel = name, "", ""
		if err := applyProfile(); err != nil {
			t.Fatalf("profile %q: %v", name, err)
		}
		if benchAnswerModel == "" || benchJudgeModel == "" {
			t.Errorf("profile %q left a model unset (answer=%q judge=%q)",
				name, benchAnswerModel, benchJudgeModel)
		}
	}
}

// TestApplyProfile_ExplicitFlagWins — a profile must not override a model the
// operator named. Otherwise "--profile local --judge-model X" silently ignores X
// and the manifest records a model that was never used.
func TestApplyProfile_ExplicitFlagWins(t *testing.T) {
	withFlags(t)
	benchProfile, benchAnswerModel, benchJudgeModel = "local", "my-answer-model", ""
	if err := applyProfile(); err != nil {
		t.Fatal(err)
	}
	if benchAnswerModel != "my-answer-model" {
		t.Errorf("answer model = %q, want the explicit flag to win", benchAnswerModel)
	}
	if benchJudgeModel == "" {
		t.Error("judge model was not filled from the profile")
	}
}

// TestApplyProfile_UnknownProfileListsKnownOnes — a typo must be diagnosable, not
// just rejected.
func TestApplyProfile_UnknownProfileListsKnownOnes(t *testing.T) {
	withFlags(t)
	benchProfile = "loocal"
	err := applyProfile()
	if err == nil {
		t.Fatal("an unknown profile was accepted")
	}
	for _, want := range []string{"local", "judged", "cloud"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list the known profile %q", err, want)
		}
	}
}

// TestApplyProfile_NoProfileIsNoOp — with no profile the explicit flags stand
// alone; this is the path the "no default" rule allows.
func TestApplyProfile_NoProfileIsNoOp(t *testing.T) {
	withFlags(t)
	benchProfile, benchAnswerModel, benchJudgeModel = "", "a", "j"
	if err := applyProfile(); err != nil {
		t.Fatal(err)
	}
	if benchAnswerModel != "a" || benchJudgeModel != "j" {
		t.Errorf("models changed with no profile: %q/%q", benchAnswerModel, benchJudgeModel)
	}
}

// TestRunBenchMemory_GuardRunsBeforeAnythingElse — the destructive-run guard must
// refuse before a dataset is read or a model contacted. A run destined to be
// refused should cost nothing, and more importantly the guard must not be
// reachable only after some other validation happens to pass.
func TestRunBenchMemory_GuardRunsBeforeAnythingElse(t *testing.T) {
	withFlags(t)
	// Deliberately ALSO invalid in other ways: no dataset, no profile. If the
	// guard is not first, one of those errors surfaces instead.
	benchDatabase, benchConfirmWipe = "production", "production"
	benchDataset, benchProfile, benchAnswerModel, benchJudgeModel = "", "", "", ""

	err := runBenchMemory(newBenchTestCmd(), nil)
	if err == nil {
		t.Fatal("the production database was accepted")
	}
	if !strings.Contains(err.Error(), "denylist") {
		t.Errorf("error %q is not the denylist refusal, so the guard did not run "+
			"first", err)
	}
}

// TestRunBenchMemory_RefusesWithoutModelSelection — "no default" means refusal.
func TestRunBenchMemory_RefusesWithoutModelSelection(t *testing.T) {
	withFlags(t)
	benchDatabase, benchConfirmWipe = "bench_db", "bench_db"
	benchProfile, benchAnswerModel, benchJudgeModel = "", "", ""

	err := runBenchMemory(newBenchTestCmd(), nil)
	if err == nil {
		t.Fatal("a run with no model selection was accepted")
	}
	if !strings.Contains(err.Error(), "model selection required") {
		t.Errorf("error %q is not the model-selection refusal", err)
	}
}

// TestResolveDataset — the three datasets plus the failure modes.
func TestResolveDataset(t *testing.T) {
	withFlags(t)
	benchDatasetPath = ""
	// native requires a corpus directory; the others carry their own haystacks.
	benchCorpusDir = t.TempDir()

	for name, want := range map[string]string{
		"longmemeval": "longmemeval",
		"locomo":      "locomo",
		"native":      "native",
	} {
		benchDataset = name
		ds, path, err := resolveDataset()
		if err != nil {
			t.Fatalf("dataset %q: %v", name, err)
		}
		if ds.Name() != want {
			t.Errorf("dataset %q resolved to %q", name, ds.Name())
		}
		if path == "" {
			t.Errorf("dataset %q resolved to an empty path", name)
		}
	}

	benchDataset = ""
	if _, _, err := resolveDataset(); err == nil {
		t.Error("an empty dataset name was accepted")
	}
	benchDataset = "nope"
	if _, _, err := resolveDataset(); err == nil {
		t.Error("an unknown dataset name was accepted")
	}
}

// TestDefaultPath_ExplicitPathWins — an operator pointing at a specific file must
// not be silently redirected to the conventional location.
func TestDefaultPath_ExplicitPathWins(t *testing.T) {
	withFlags(t)
	benchDatasetPath = "/tmp/my-dataset.json"
	if got := defaultPath("longmemeval.json"); got != "/tmp/my-dataset.json" {
		t.Errorf("defaultPath = %q, want the explicit --dataset-path", got)
	}
}

// TestFormatAccuracy_UndefinedIsNotZero — 0.000 and "no gradeable answers" mean
// very different things and must not print the same.
func TestFormatAccuracy_UndefinedIsNotZero(t *testing.T) {
	if got := formatAccuracy(membench.OutcomeCounts{Invalid: 3}); got != "n/a" {
		t.Errorf("accuracy with nothing judged = %q, want n/a", got)
	}
	if got := formatAccuracy(membench.OutcomeCounts{Incorrect: 3}); got != "0.000" {
		t.Errorf("accuracy of all-wrong = %q, want 0.000", got)
	}
}

// TestPrintResult_LeadsWithUntrustworthyWarning — the warning must appear BEFORE
// the numbers, so a degraded run's figures are never read without it.
func TestPrintResult_LeadsWithUntrustworthyWarning(t *testing.T) {
	cmd := newBenchTestCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)

	res := membench.Result{
		Counts:  map[string]membench.OutcomeCounts{"cat": {Correct: 1, Error: 9}},
		Metrics: map[string]membench.Metrics{"cat": {}},
		Trust:   membench.Trust{Trustworthy: false, Reason: "degraded rate too high"},
	}
	if err := printResult(cmd, res, "/tmp/run"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	warnAt := strings.Index(out, "UNTRUSTWORTHY")
	if warnAt < 0 {
		t.Fatal("no untrustworthy warning printed")
	}
	if tableAt := strings.Index(out, "CATEGORY"); tableAt >= 0 && tableAt < warnAt {
		t.Error("the numbers were printed before the untrustworthy warning; a " +
			"reader could quote them without seeing it")
	}
}

// TestPrintResult_FlagsPartialComparability — an unverifiable comparability key
// must be surfaced, not silently treated as full comparability.
func TestPrintResult_FlagsPartialComparability(t *testing.T) {
	cmd := newBenchTestCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)

	res := membench.Result{
		Counts:  map[string]membench.OutcomeCounts{"cat": {Correct: 1}},
		Metrics: map[string]membench.Metrics{"cat": {}},
		Trust:   membench.Trust{Trustworthy: true},
		// Not a single-system run and no external config → partial.
		Fields: membench.ComparabilityFields{TheirExtractionModel: "x"},
	}
	if err := printResult(cmd, res, "/tmp/run"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "PARTIAL") {
		t.Error("a partial comparability key was not surfaced in the report")
	}
}

// TestCompare_RefusesIncomparableRuns — the enforcement point, through the CLI.
func TestCompare_RefusesIncomparableRuns(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeRun(t, dirA, membench.ComparabilityFields{JudgeModel: "judge-a", SingleSystem: true})
	writeRun(t, dirB, membench.ComparabilityFields{JudgeModel: "judge-b", SingleSystem: true})

	err := runBenchMemoryCompare(newBenchTestCmd(), []string{dirA, dirB})
	if err == nil {
		t.Fatal("two runs with different judge models were compared")
	}
	if !strings.Contains(err.Error(), "judge_model") {
		t.Errorf("error %q does not name the differing field", err)
	}
}

// TestCompare_AcceptsComparableRuns — and the happy path works.
func TestCompare_AcceptsComparableRuns(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	f := membench.ComparabilityFields{JudgeModel: "same", SingleSystem: true}
	writeRun(t, dirA, f)
	writeRun(t, dirB, f)

	if err := runBenchMemoryCompare(newBenchTestCmd(), []string{dirA, dirB}); err != nil {
		t.Errorf("two identical runs were refused: %v", err)
	}
}

// TestReport_MissingRunDirErrors — a clear failure beats an empty scoreboard.
func TestReport_MissingRunDirErrors(t *testing.T) {
	if err := runBenchMemoryReport(newBenchTestCmd(), []string{t.TempDir()}); err == nil {
		t.Error("reporting on a directory with no results.json succeeded")
	}
}

// TestBenchLLM_Complete — the OpenAI-compatible path, against a fake.
func TestBenchLLM_Complete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("posted to %q, want /chat/completions", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "the answer"}}},
		})
	}))
	defer srv.Close()

	l := &benchLLM{baseURL: srv.URL, model: "m", client: srv.Client()}
	got, err := l.Complete(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "the answer" {
		t.Errorf("Complete = %q", got)
	}
}

// TestBenchLLM_NoChoicesIsError — an empty choices array must not become an empty
// answer, which would be scored as a wrong answer instead of an error.
func TestBenchLLM_NoChoicesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer srv.Close()

	l := &benchLLM{baseURL: srv.URL, model: "m", client: srv.Client()}
	if _, err := l.Complete(context.Background(), "p"); err == nil {
		t.Error("an empty choices array produced a successful empty completion")
	}
}

// TestBenchLLM_HTTPErrorSurfaces — a non-200 must reach the runner as an error.
func TestBenchLLM_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	l := &benchLLM{baseURL: srv.URL, model: "m", client: srv.Client()}
	if _, err := l.Complete(context.Background(), "p"); err == nil {
		t.Error("a 500 produced a successful completion")
	}
}

// TestBenchLLM_WithModelIsACopy — pinning the judge's model must not mutate the
// answerer's client, or both would silently run on the judge model.
func TestBenchLLM_WithModelIsACopy(t *testing.T) {
	base := &benchLLM{baseURL: "http://x", model: "answer-model"}
	judge := base.withModel("judge-model")

	if base.model != "answer-model" {
		t.Errorf("withModel mutated the original: %q", base.model)
	}
	if judge.model != "judge-model" {
		t.Errorf("copy has model %q, want judge-model", judge.model)
	}
}

// TestNewBenchLLM_RequiresAnswerModel — an unset model must fail at construction
// rather than sending a request with an empty model field.
func TestNewBenchLLM_RequiresAnswerModel(t *testing.T) {
	withFlags(t)
	benchAnswerModel = ""
	if _, err := newBenchLLM(); err == nil {
		t.Error("newBenchLLM accepted an empty answer model")
	}
}

// TestNewBenchLLM_EndpointResolutionOrder — explicit beats inferred, so an
// operator can always override.
func TestNewBenchLLM_EndpointResolutionOrder(t *testing.T) {
	withFlags(t)
	benchAnswerModel = "m"

	t.Setenv("VORNIK_BENCH_LLM_URL", "http://explicit:1/v1")
	t.Setenv("OLLAMA_HOST", "http://inferred:2")
	l, err := newBenchLLM()
	if err != nil {
		t.Fatal(err)
	}
	if l.baseURL != "http://explicit:1/v1" {
		t.Errorf("baseURL = %q, want the explicit override", l.baseURL)
	}

	t.Setenv("VORNIK_BENCH_LLM_URL", "")
	l2, err := newBenchLLM()
	if err != nil {
		t.Fatal(err)
	}
	if l2.baseURL != "http://inferred:2/v1" {
		t.Errorf("baseURL = %q, want OLLAMA_HOST with /v1 appended", l2.baseURL)
	}
}

func writeRun(t *testing.T, dir string, f membench.ComparabilityFields) {
	t.Helper()
	res := membench.Result{
		Counts:  map[string]membench.OutcomeCounts{"cat": {Correct: 1}},
		Metrics: map[string]membench.Metrics{"cat": {ContextRecall: 1}},
		Trust:   membench.Trust{Trustworthy: true},
		Fields:  f,
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "results.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// newBenchTestCmd is a bare command with a discardable output buffer. Named
// distinctly from the package's existing newTestCmd, whose signature differs.
func newBenchTestCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd
}

// TestResolveSystem_Vornik — the vornik adapter needs the daemon env; without it
// the failure must name what is missing rather than producing an adapter that
// errors on first use.
func TestResolveSystem_Vornik(t *testing.T) {
	withFlags(t)
	benchSystem = "vornik"

	t.Setenv("VORNIK_URL", "")
	t.Setenv("VORNIK_COMPANION_TOKEN", "")
	_, err := resolveSystem()
	if err == nil {
		t.Fatal("the vornik adapter was built with no daemon URL or token")
	}
	if !strings.Contains(err.Error(), "VORNIK_URL") {
		t.Errorf("error %q does not name the missing environment", err)
	}

	t.Setenv("VORNIK_URL", "http://127.0.0.1:9")
	t.Setenv("VORNIK_COMPANION_TOKEN", "tok")
	sys, err := resolveSystem()
	if err != nil {
		t.Fatalf("resolveSystem: %v", err)
	}
	if sys.Name() != "vornik" {
		t.Errorf("resolved %q, want vornik", sys.Name())
	}
}

// TestResolveSystem_External — the external adapter is reachable, and the missing
// URL is diagnosed rather than defaulted to something local.
func TestResolveSystem_External(t *testing.T) {
	withFlags(t)
	benchSystem = "external"
	benchExternalURL = ""
	if _, err := resolveSystem(); err == nil {
		t.Error("--system external was accepted with no --external-url")
	}

	benchExternalURL = "http://127.0.0.1:8888"
	t.Cleanup(func() { benchExternalURL = "" })
	sys, err := resolveSystem()
	if err != nil {
		t.Fatalf("resolveSystem: %v", err)
	}
	if sys.Name() != "external" {
		t.Errorf("resolved %q, want external", sys.Name())
	}
}

// TestResolveSystem_UnknownRejected — a typo must not silently fall through to a
// default system, which would report a score for something nobody asked to test.
func TestResolveSystem_UnknownRejected(t *testing.T) {
	withFlags(t)
	benchSystem = "vornick"
	if _, err := resolveSystem(); err == nil {
		t.Error("an unknown --system value was accepted")
	}
}

// TestResolveSystem_EmptyDefaultsToVornik — the documented default.
func TestResolveSystem_EmptyDefaultsToVornik(t *testing.T) {
	withFlags(t)
	benchSystem = ""
	t.Setenv("VORNIK_URL", "http://127.0.0.1:9")
	t.Setenv("VORNIK_COMPANION_TOKEN", "tok")
	sys, err := resolveSystem()
	if err != nil {
		t.Fatalf("resolveSystem: %v", err)
	}
	if sys.Name() != "vornik" {
		t.Errorf("empty --system resolved to %q, want vornik", sys.Name())
	}
}

// TestPrintResult_JSONMode — the machine-readable path must emit parseable JSON
// carrying the trust verdict, since a consumer needs it as much as a human does.
func TestPrintResult_JSONMode(t *testing.T) {
	saved := benchJSON
	benchJSON = true
	t.Cleanup(func() { benchJSON = saved })

	cmd := newBenchTestCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)

	res := membench.Result{
		Counts:  map[string]membench.OutcomeCounts{"cat": {Correct: 1}},
		Metrics: map[string]membench.Metrics{"cat": {ContextRecall: 1}},
		Trust:   membench.Trust{Trustworthy: true},
	}
	if err := printResult(cmd, res, "/tmp/run"); err != nil {
		t.Fatal(err)
	}
	var back membench.Result
	if err := json.Unmarshal([]byte(buf.String()), &back); err != nil {
		t.Fatalf("JSON output does not parse: %v", err)
	}
	if !back.Trust.Trustworthy {
		t.Error("the trust verdict did not survive the JSON round trip")
	}
}

// TestReport_ReadsResultsAndPrints — the report path over a written run.
func TestReport_ReadsResultsAndPrints(t *testing.T) {
	dir := t.TempDir()
	writeRun(t, dir, membench.ComparabilityFields{SingleSystem: true})
	if err := runBenchMemoryReport(newBenchTestCmd(), []string{dir}); err != nil {
		t.Errorf("report on a valid run failed: %v", err)
	}
}

// TestCompare_MissingRunErrors — comparing against a directory with no results
// must say which one is missing.
func TestCompare_MissingRunErrors(t *testing.T) {
	good := t.TempDir()
	writeRun(t, good, membench.ComparabilityFields{SingleSystem: true})
	missing := t.TempDir()

	if err := runBenchMemoryCompare(newBenchTestCmd(), []string{missing, good}); err == nil {
		t.Error("compare succeeded with a missing first run")
	}
	if err := runBenchMemoryCompare(newBenchTestCmd(), []string{good, missing}); err == nil {
		t.Error("compare succeeded with a missing second run")
	}
}

// TestResolveDataset_NativeRequiresCorpusDir — the native gold set names documents
// by id and those ids only resolve against a corpus directory, so a missing
// --corpus-dir has to be refused rather than producing an empty haystack.
//
// The default used to be this repository's own design-doc tree. That was removed
// when the harness became a shipped tool: a customer has no such directory, and a
// silently-empty haystack scores every question as a retrieval failure — a
// benchmark reporting zeroes that look like a product defect.
func TestResolveDataset_NativeRequiresCorpusDir(t *testing.T) {
	withFlags(t)
	benchDataset, benchCorpusDir = "native", ""

	_, _, err := resolveDataset()
	if err == nil {
		t.Fatal("native dataset accepted with no corpus directory")
	}
	if !strings.Contains(err.Error(), "corpus-dir") {
		t.Errorf("error %q does not name the missing flag", err)
	}
}

// TestResolveDataset_OtherDatasetsDoNotNeedCorpusDir — longmemeval and locomo carry
// their own haystacks, so requiring the flag for them would be a false barrier.
func TestResolveDataset_OtherDatasetsDoNotNeedCorpusDir(t *testing.T) {
	withFlags(t)
	for _, ds := range []string{"longmemeval", "locomo"} {
		benchDataset, benchCorpusDir = ds, ""
		if _, _, err := resolveDataset(); err != nil {
			t.Errorf("%s rejected without a corpus dir: %v", ds, err)
		}
	}
}

// TestRunBenchMemory_Tier2OnlyNeedsNoModelSelection is the flag's reason for
// existing, asserted at the CLI boundary rather than only in the runner.
//
// A gate that required --profile could not run on a fork PR: the profiles resolve
// to models, models need an endpoint and credentials, and a gate that bills per PR
// gets switched off. So --tier2-only must get PAST the model-selection refusal
// that every other run hits.
//
// It is asserted by the error it does NOT produce. The run still fails here — the
// test has no database or dataset — but it must fail on something later than model
// selection, because reaching a later check is the proof that this one was skipped.
func TestRunBenchMemory_Tier2OnlyNeedsNoModelSelection(t *testing.T) {
	withFlags(t)
	benchDatabase, benchConfirmWipe = "bench_db", "bench_db"
	benchProfile, benchAnswerModel, benchJudgeModel = "", "", ""
	benchTier2Only = true

	err := runBenchMemory(newBenchTestCmd(), nil)
	if err == nil {
		// Would mean the run somehow completed without a dataset; not this test's
		// business, but it certainly means model selection did not block it.
		return
	}
	if strings.Contains(err.Error(), "model selection required") {
		t.Errorf("--tier2-only was still refused for having no model: %v\n"+
			"That refusal is what makes the flag useless for the CI gate it exists to serve.", err)
	}
}
