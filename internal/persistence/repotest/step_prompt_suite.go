package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunStepPromptSuite pins the contract of the content-addressed step-prompt
// store on both backends (step-prompt persistence design §4, §6): Save is
// idempotent, Get round-trips bytes exactly, and PruneUnreferenced keeps a
// part referenced by ANY of an outcome row's five hash columns and removes
// the rest — the prompt horizon is the outcome horizon, with no second knob.
func RunStepPromptSuite(t *testing.T, prompts persistence.StepPromptRepository, outcomes persistence.ExecutionStepOutcomeRepository) {
	t.Helper()
	t.Run("MissContract", func(t *testing.T) {
		AssertMissRepo(t, "StepPromptRepository.Get", prompts.Get)
	})
	t.Run("SaveIsIdempotent_GetRoundTrips", func(t *testing.T) { stepPromptSaveGet(t, prompts) })
	t.Run("PruneKeepsAnyReferencedColumn", func(t *testing.T) { stepPromptPrune(t, prompts, outcomes) })
	t.Run("OutcomeRowCarriesTheHashes", func(t *testing.T) { stepPromptOutcomeRoundTrip(t, outcomes) })
}

func stepPromptSaveGet(t *testing.T, prompts persistence.StepPromptRepository) {
	t.Helper()
	ctx := context.Background()
	{
		body := "You are interacting with an AI system.\n  indented — with 'quotes' and ünïcode " + uniqueID("salt")
		hash, err := prompts.Save(ctx, persistence.StepPromptSystem, body)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if hash != persistence.HashStepPrompt(body) {
			t.Fatalf("Save returned %q, want the sha256 of the stored bytes", hash)
		}
		again, err := prompts.Save(ctx, persistence.StepPromptSystem, body)
		if err != nil || again != hash {
			t.Fatalf("Save (second, same bytes) = %q, %v; want the same hash and no error", again, err)
		}
		got, err := prompts.Get(ctx, hash)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Body != body || got.Part != persistence.StepPromptSystem || got.Hash != hash {
			t.Fatalf("Get round-trip lost something: %+v", got)
		}
		if _, err := prompts.Get(ctx, uniqueID("absent")); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("Get(absent) = %v, want ErrNotFound", err)
		}
	}
}

func stepPromptPrune(t *testing.T, prompts persistence.StepPromptRepository, outcomes persistence.ExecutionStepOutcomeRepository) {
	t.Helper()
	ctx := context.Background()
	{
		save := func(part persistence.StepPromptPart, seed string) string {
			h, err := prompts.Save(ctx, part, "body-"+uniqueID(seed))
			if err != nil {
				t.Fatalf("Save %s: %v", seed, err)
			}
			return h
		}
		sys := save(persistence.StepPromptSystem, "sys")
		usr := save(persistence.StepPromptUser, "usr")
		tools := save(persistence.StepPromptTools, "tools")
		// The two boundary files (step-I/O persistence design §3): referenced
		// through their own columns, kept by the same rule.
		input := save(persistence.StepPromptInput, "input")
		result := save(persistence.StepPromptResult, "result")
		orphan := save(persistence.StepPromptUser, "orphan")
		// Five outcome rows, each referencing ONE part through a different column.
		for i, h := range []persistence.StepPromptHashes{{System: sys}, {User: usr}, {Tools: tools}, {Input: input}, {Result: result}} {
			o := &persistence.ExecutionStepOutcome{
				ID: uniqueID("out"), ProjectID: "p", TaskID: uniqueID("task"), ExecutionID: uniqueID("exec"),
				StepID: "plan", Role: "planner", Model: "m", Outcome: "ok", RecordedAt: time.Now().UTC(),
				PromptHashes: h,
			}
			if err := outcomes.Record(ctx, o); err != nil {
				t.Fatalf("Record %d: %v", i, err)
			}
		}
		n, err := prompts.PruneUnreferenced(ctx)
		if err != nil {
			t.Fatalf("PruneUnreferenced: %v", err)
		}
		if n < 1 {
			t.Fatalf("prune removed %d rows, want at least the orphan", n)
		}
		for _, kept := range []string{sys, usr, tools, input, result} {
			if _, err := prompts.Get(ctx, kept); err != nil {
				t.Errorf("%s was referenced by an outcome row and must survive: %v", kept, err)
			}
		}
		if _, err := prompts.Get(ctx, orphan); !errors.Is(err, persistence.ErrNotFound) {
			t.Errorf("the unreferenced part must be gone, got %v", err)
		}
	}
}

func stepPromptOutcomeRoundTrip(t *testing.T, outcomes persistence.ExecutionStepOutcomeRepository) {
	t.Helper()
	ctx := context.Background()
	{
		exec := uniqueID("exec")
		o := &persistence.ExecutionStepOutcome{
			ID: uniqueID("out"), ProjectID: "p", TaskID: uniqueID("task"), ExecutionID: exec,
			StepID: "write", Role: "writer", Model: "m", Outcome: "ok", RecordedAt: time.Now().UTC(),
			PromptHashes: persistence.StepPromptHashes{System: "s1", User: "u1", Tools: "t1", Input: "i1", Result: "r1"},
		}
		if err := outcomes.Record(ctx, o); err != nil {
			t.Fatalf("Record: %v", err)
		}
		rows, err := outcomes.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionID: &exec, PageSize: 10})
		if err != nil || len(rows) != 1 {
			t.Fatalf("List: %v (%d rows)", err, len(rows))
		}
		if rows[0].PromptHashes != o.PromptHashes {
			t.Fatalf("hashes round-trip: got %+v want %+v", rows[0].PromptHashes, o.PromptHashes)
		}
	}
}
