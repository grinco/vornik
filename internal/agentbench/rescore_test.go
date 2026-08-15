package agentbench

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type rescoreTraces struct {
	byExec map[string][]Trace
	err    error
}

func (f rescoreTraces) AssembleTraces(_ context.Context, execID string) ([]Trace, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byExec[execID], nil
}

func v2Journal() Journal {
	arm := baseArm()
	arm.HarnessVersion = "2"
	return Journal{
		Manifest: RunManifest{RunID: "r1", Arm: arm, ArmKey: arm.Key()},
		Records: []ExecutionRecord{{
			TaskID: "t1", ExecutionID: "e1", Succeeded: true,
			CostUSD: 1.25, PromptTokens: 900, CompletionTokens: 100,
			Verdicts: []Verdict{{Probe: grantProbeName, CoreMiss: true}},
		}},
	}
}

func goldFor(tools ...string) *GoldManifest {
	return &GoldManifest{Entries: []Gold{{TaskID: "t1", Paths: [][]string{tools}}}}
}

// The point of the whole thing: a v2 core miss that v3 clears, without running
// an agent. sw-02/05/08 all had this exact shape — core wanted git_status, the
// lead granted run_shell, and v2 called it a hard failure.
func TestRescore_AppliesTheCurrentScoringToOldTraces(t *testing.T) {
	traces := rescoreTraces{byExec: map[string][]Trace{"e1": {{
		ExecutionID: "e1", StepID: "s1", Role: "lead",
		Requested: []string{"run_shell"}, Accepted: []string{"run_shell"},
		Invoked: []string{"run_shell"},
	}}}}

	out, err := Rescore(context.Background(), v2Journal(), traces,
		[]Probe{GrantProbe{}}, goldFor("git_status", "run_shell"))
	if err != nil {
		t.Fatalf("rescore: %v", err)
	}
	if out.Manifest.Arm.HarnessVersion != HarnessVersion {
		t.Errorf("harness version = %q, want %q", out.Manifest.Arm.HarnessVersion, HarnessVersion)
	}
	var grant *Verdict
	for i := range out.Records[0].Verdicts {
		if out.Records[0].Verdicts[i].Probe == grantProbeName {
			grant = &out.Records[0].Verdicts[i]
		}
	}
	if grant == nil {
		t.Fatal("no grant verdict after re-scoring")
	}
	if grant.CoreMiss {
		t.Error("v2's core miss survived re-scoring; git_status is covered by the granted shell")
	}
	if grant.CoreSubstitutions["git_status"] != "run_shell" {
		t.Errorf("substitution not recorded: %v", grant.CoreSubstitutions)
	}
}

// Re-scoring changes what a number MEANS, not what happened. Cost, tokens and
// the daemon's success verdict are facts about an execution that already ran.
func TestRescore_PreservesTheFactsOfTheRun(t *testing.T) {
	traces := rescoreTraces{byExec: map[string][]Trace{"e1": {{
		ExecutionID: "e1", StepID: "s1", Requested: []string{"run_shell"},
		Accepted: []string{"run_shell"}, Invoked: []string{"run_shell"},
	}}}}
	in := v2Journal()

	out, err := Rescore(context.Background(), in, traces, []Probe{GrantProbe{}}, goldFor("run_shell"))
	if err != nil {
		t.Fatalf("rescore: %v", err)
	}
	got, want := out.Records[0], in.Records[0]
	if got.CostUSD != want.CostUSD || got.PromptTokens != want.PromptTokens ||
		got.CompletionTokens != want.CompletionTokens || got.Succeeded != want.Succeeded {
		t.Errorf("re-scoring altered the facts of the run: %+v", got)
	}
	// Every arm axis describing the RUN must survive; only scoring moved.
	for _, pair := range [][2]string{
		{"binary", out.Manifest.Arm.BinarySHA256}, {"config", out.Manifest.Arm.ConfigSHA256},
		{"taskset", out.Manifest.Arm.TaskSetSHA256}, {"gold", out.Manifest.Arm.GoldSHA256},
		{"policy", out.Manifest.Arm.ContextPolicy},
	} {
		if pair[1] == "" {
			t.Errorf("re-scoring dropped the arm's %s", pair[0])
		}
	}
	// The denormalised key must move with the version, or a reader believes a
	// v3 journal is comparable with v2 figures.
	if out.Manifest.ArmKey == in.Manifest.ArmKey {
		t.Error("arm key unchanged after a harness bump")
	}
}

// A ledger has a retention window. Quietly dropping expired executions would
// return a smaller, cleaner-looking journal that reads exactly like a complete
// one.
func TestRescore_RefusesWhenTracesAreGone(t *testing.T) {
	t.Run("store error", func(t *testing.T) {
		_, err := Rescore(context.Background(), v2Journal(),
			rescoreTraces{err: errors.New("relation does not exist")},
			[]Probe{GrantProbe{}}, goldFor("run_shell"))
		if err == nil {
			t.Fatal("re-score succeeded with an unreadable ledger")
		}
	})
	t.Run("execution silently absent", func(t *testing.T) {
		_, err := Rescore(context.Background(), v2Journal(),
			rescoreTraces{byExec: map[string][]Trace{}},
			[]Probe{GrantProbe{}}, goldFor("run_shell"))
		if err == nil || !strings.Contains(err.Error(), "silently drop") {
			t.Fatalf("want a refusal naming the silent-drop risk, got: %v", err)
		}
	})
}

// Re-scoring a journal that already used the current contract would change
// nothing and overwrite the original.
func TestRescore_RefusesAJournalAlreadyAtTheCurrentVersion(t *testing.T) {
	j := v2Journal()
	j.Manifest.Arm.HarnessVersion = HarnessVersion

	_, err := Rescore(context.Background(), j, rescoreTraces{}, []Probe{GrantProbe{}}, goldFor("run_shell"))
	if err == nil || !strings.Contains(err.Error(), "already scored") {
		t.Fatalf("want a refusal, got: %v", err)
	}
}

// An excluded task has no execution to re-read; it must pass through rather
// than fail the whole re-score.
func TestRescore_PassesThroughRecordsWithNoExecution(t *testing.T) {
	j := v2Journal()
	j.Records = append(j.Records, ExecutionRecord{TaskID: "t2", Succeeded: false, ErrorText: "excluded from gold: not measured"})
	traces := rescoreTraces{byExec: map[string][]Trace{"e1": {{
		ExecutionID: "e1", StepID: "s1", Requested: []string{"run_shell"},
		Accepted: []string{"run_shell"}, Invoked: []string{"run_shell"},
	}}}}

	out, err := Rescore(context.Background(), j, traces, []Probe{GrantProbe{}}, goldFor("run_shell"))
	if err != nil {
		t.Fatalf("rescore: %v", err)
	}
	if len(out.Records) != 2 {
		t.Fatalf("got %d records, want both kept", len(out.Records))
	}
	if out.Records[1].ErrorText == "" {
		t.Error("the excluded record lost its reason")
	}
}
