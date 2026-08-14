package agentbench

import "testing"

func TestClassifyFailure_OnePerClass(t *testing.T) {
	cases := []struct {
		name      string
		succeeded bool
		errText   string
		want      FailureClass
		why       string
	}{
		{
			name: "success", succeeded: true, want: FailureNone,
		},
		{
			name:    "agent could not do the work",
			errText: "step failed: acceptance criteria not met after 3 attempts",
			want:    FailureTask,
			why:     "the only class that reflects on agent quality",
		},
		{
			name:    "provider outage",
			errText: "curl: (7) Failed to connect to gateway port 443",
			want:    FailureInfra,
		},
		{
			name:    "lease loss",
			errText: "could not renew: lease not found for execution exec_123",
			want:    FailureInfra,
			why:     "the agent never got to fail on its own merits",
		},
		{
			name:    "harness bug",
			errText: `task "t9" has no recorded paths`,
			want:    FailureHarness,
			why:     "measuring our own bug as a Vornik regression is how a benchmark starts lying",
		},
		{
			name:    "failure with no recorded reason",
			errText: "",
			want:    FailureHarness,
			why:     "an unexplained failure is not evidence the agent failed",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyFailure(c.succeeded, c.errText); got != c.want {
				t.Errorf("ClassifyFailure(%v, %q) = %q, want %q (%s)",
					c.succeeded, c.errText, got, c.want, c.why)
			}
		})
	}
}

// The 2026-07-12 shape. A deterministic context-window overflow was sanitised
// into a generic PROVIDER_ERROR 502 and re-sent ~12x by the retry ladder because
// it read as infra. It must classify as neither infra nor task: it is the
// context policy under test failing, which is the most interesting signal a
// context-management benchmark can produce.
func TestClassifyFailure_ContextOverflowIsItsOwnClassNotInfra(t *testing.T) {
	overflow := "ValidationException: This model's maximum context length is 202752 tokens. " +
		"However, you requested 202753 tokens (194561 in the messages, 8192 in the completion)."

	if got := ClassifyFailure(false, overflow); got != FailureContextOverflow {
		t.Fatalf("got %q, want %q — bucketing an overflow as infra discards the finding "+
			"and is what produced the 2026-07-12 retry storm", got, FailureContextOverflow)
	}
}

// A wrapping layer can glue an infra-looking marker onto a deterministic
// overflow. Overflow must still win, mirroring chat.IsUpstreamInfraError's own
// ordering.
func TestClassifyFailure_OverflowBeatsACoWrappedInfraMarker(t *testing.T) {
	msg := "curl: (28) Operation timed out; upstream said: maximum context length is 202752 tokens"
	if got := ClassifyFailure(false, msg); got != FailureContextOverflow {
		t.Errorf("got %q, want %q — the infra marker must not win over a deterministic overflow",
			got, FailureContextOverflow)
	}
}

func TestSuccessBreakdown_HarnessFailuresLeaveTheDenominator(t *testing.T) {
	var b SuccessBreakdown
	for i := 0; i < 8; i++ {
		b.Add(FailureNone)
	}
	b.Add(FailureTask)
	b.Add(FailureHarness)

	rate, defined := b.TaskSuccessRate()
	if !defined {
		t.Fatal("rate undefined with 10 attempts")
	}
	// 8 of 9, not 8 of 10: the harness failure is neither a success nor a
	// failure of the system under test.
	if want := 8.0 / 9.0; rate != want {
		t.Errorf("rate = %v, want %v — a benchmark bug must not count against the "+
			"system it is measuring", rate, want)
	}
	if b.Attempted != 10 {
		t.Errorf("attempted = %d, want 10 — the harness failure is still reported", b.Attempted)
	}
}

func TestSuccessBreakdown_UndefinedWhenEverythingWasAHarnessFailure(t *testing.T) {
	var b SuccessBreakdown
	b.Add(FailureHarness)
	b.Add(FailureHarness)
	if _, defined := b.TaskSuccessRate(); defined {
		t.Error("a run that only broke the harness reported a success rate — there is " +
			"nothing to rate")
	}
}

// An overflow our own budget caught and one the provider rejected are both
// policy signals, but they need DIFFERENT fixes — a compaction change versus a
// model_limits entry — so the taxonomy carries which side caught it.
func TestClassifyOverflowSource(t *testing.T) {
	cases := []struct {
		name    string
		errText string
		want    OverflowSource
	}{
		{
			name:    "our chat proxy refused before the call",
			errText: "CONTEXT_OVERFLOW: prompt exceeds the model's context window — compact the conversation or reduce input",
			want:    OverflowSourcePolicy,
		},
		{
			name:    "our executor wrapper tripped its budget",
			errText: "step stopped: outcome prompt_token_budget",
			want:    OverflowSourcePolicy,
		},
		{
			name:    "the provider rejected a request that got past our budget",
			errText: "ValidationException: This model's maximum context length is 202752 tokens",
			want:    OverflowSourceProvider,
		},
		{
			name:    "not an overflow at all",
			errText: "curl: (7) Failed to connect",
			want:    OverflowSourceUnknown,
		},
		{
			name:    "nothing recorded",
			errText: "",
			want:    OverflowSourceUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyOverflowSource(c.errText); got != c.want {
				t.Errorf("ClassifyOverflowSource(%q) = %q, want %q", c.errText, got, c.want)
			}
		})
	}
}

// The agent did its work and the BENCHMARK's workspace was dirty. Blaming the
// agent for that inflates the number this benchmark exists to report honestly —
// and the gold builder would drop the task as one the unrestricted arm never
// passed. Observed verbatim on the first full gold pass, task 3 of 18.
func TestClassifyFailure_ADirtyWorkspaceIsOurFailureNotTheAgents(t *testing.T) {
	msg := "agent steps succeeded but changes could not be merged to master: " +
		"main workspace has uncommitted changes"

	if got := ClassifyFailure(false, msg); got != FailureHarness {
		t.Errorf("got %q, want %q — the agent's steps SUCCEEDED; the workspace was ours",
			got, FailureHarness)
	}
}
