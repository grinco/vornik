package dispatcher

import (
	"context"
	"strings"
	"testing"
)

// stubMemoryWriteGate answers the capability question from a fixed allow-set.
type stubMemoryWriteGate struct {
	allow map[string]bool
	asked []string
}

func (g *stubMemoryWriteGate) CanWriteMemory(_ context.Context, channel, sessionID string) bool {
	g.asked = append(g.asked, channel+"|"+sessionID)
	return g.allow[channel+"|"+sessionID]
}

// SLICE 1 of the chat memory-write design
// (https://docs.vornik.io §5.1), after three review
// rounds. This slice is the HARD GATE and nothing else: no write path exists yet, because
// the gate has to exist before anything can write.
//
// Review round 1 killed the original design for offering downranking as the injection
// mitigation: "The difference between 'never writes' and 'writes with low weight' is not a
// security boundary — it's a ranking problem with an adversarial optimizer on the input
// side." §5.1 is the boundary that replaced it.
func TestRemember_DeniedByDefault(t *testing.T) {
	// No gate wired at all — the capability is absent.
	te := &ToolExecutor{}
	res := te.remember(context.Background(), `{"content":"the deadline is Friday"}`)
	if !strings.Contains(res.Content, "not available on this deployment") {
		t.Fatalf("an unwired capability must refuse; got: %s", res.Content)
	}

	// Gate wired but this channel was never granted the capability.
	gate := &stubMemoryWriteGate{allow: map[string]bool{}}
	te = &ToolExecutor{memoryWrite: gate}
	res = te.remember(
		WithCallSiteForTest(context.Background(), "slack", "T1/C1#main"),
		`{"content":"the deadline is Friday"}`,
	)
	if !strings.Contains(res.Content, "not available on this deployment") {
		t.Fatalf("an ungranted channel must refuse; got: %s", res.Content)
	}
}

// The refusal must be IDENTICAL whether the capability is absent or merely ungranted for
// this channel (§5.1, review-2 minor). Two different messages would tell a caller which
// deployments have the feature configured but withheld — a capability-existence oracle.
func TestRemember_RefusalDoesNotLeakWhetherTheCapabilityExists(t *testing.T) {
	absent := (&ToolExecutor{}).remember(context.Background(), `{"content":"x"}`)

	ungranted := (&ToolExecutor{memoryWrite: &stubMemoryWriteGate{allow: map[string]bool{}}}).
		remember(WithCallSiteForTest(context.Background(), "slack", "T1/C1#main"), `{"content":"x"}`)

	if absent.Content != ungranted.Content {
		t.Fatalf("refusals differ, which leaks whether the capability exists:\n absent:  %q\n ungranted: %q",
			absent.Content, ungranted.Content)
	}
}

// The gate is asked about THIS channel and THIS session, not about the deployment in
// general — the grant is per channel (§5.1), so the identity being checked matters.
func TestRemember_AsksTheGateAboutTheOriginatingChannel(t *testing.T) {
	gate := &stubMemoryWriteGate{allow: map[string]bool{}}
	te := &ToolExecutor{memoryWrite: gate}

	te.remember(WithCallSiteForTest(context.Background(), "slack", "T9/C9#main"), `{"content":"x"}`)

	if len(gate.asked) != 1 || gate.asked[0] != "slack|T9/C9#main" {
		t.Fatalf("gate consulted with %v, want one question about slack|T9/C9#main", gate.asked)
	}
}

// A turn with no originating channel (API, A2A, an autonomy tick) has no channel whose
// grant could be checked, so it must refuse rather than fall through to a default.
func TestRemember_RefusesWithoutAnOriginatingChannel(t *testing.T) {
	gate := &stubMemoryWriteGate{allow: map[string]bool{"|": true}}
	te := &ToolExecutor{memoryWrite: gate}

	res := te.remember(context.Background(), `{"content":"x"}`)
	if !strings.Contains(res.Content, "not available on this deployment") {
		t.Fatalf("no originating channel must refuse; got: %s", res.Content)
	}
	if len(gate.asked) != 0 {
		t.Errorf("gate should not be consulted without a channel; asked=%v", gate.asked)
	}
}

// Slice 1 has NO write path. A granted channel must say so plainly rather than silently
// doing nothing — a tool that accepts and discards is the "it said it would remember and
// nothing happened" bug this whole design exists to fix.
func TestRemember_GrantedChannelSaysTheWritePathIsNotBuiltYet(t *testing.T) {
	gate := &stubMemoryWriteGate{allow: map[string]bool{"slack|T1/C1#main": true}}
	te := &ToolExecutor{memoryWrite: gate}

	res := te.remember(
		WithCallSiteForTest(context.Background(), "slack", "T1/C1#main"),
		`{"content":"the deadline is Friday"}`,
	)
	low := strings.ToLower(res.Content)
	if strings.Contains(low, "not available on this deployment") {
		t.Fatalf("a granted channel must pass the gate; got: %s", res.Content)
	}
	if !strings.Contains(low, "not") || !strings.Contains(low, "yet") {
		t.Errorf("a granted channel must be told the write path is not built yet, so the "+
			"model does not claim the memory was saved; got: %s", res.Content)
	}
	// And it must not imply success.
	for _, forbidden := range []string{"saved", "stored", "remembered"} {
		if strings.Contains(low, forbidden) {
			t.Errorf("refusal implies the write happened via %q: %s", forbidden, res.Content)
		}
	}
}

// Empty or malformed input asks for what is missing rather than writing nothing silently.
func TestRemember_RejectsEmptyAndMalformedInput(t *testing.T) {
	gate := &stubMemoryWriteGate{allow: map[string]bool{"slack|T1/C1#main": true}}
	te := &ToolExecutor{memoryWrite: gate}
	ctx := WithCallSiteForTest(context.Background(), "slack", "T1/C1#main")

	for _, args := range []string{`{"content":"  "}`, `{}`, ``, `not json`} {
		res := te.remember(ctx, args)
		if !strings.Contains(strings.ToLower(res.Content), "content") {
			t.Errorf("args %q should ask for content; got: %s", args, res.Content)
		}
	}
}

// REGRESSION GUARD, learned the hard way. NewAgent builds a.toolExecutor ONCE, so a
// late-bind setter that assigns only the agent field leaves the executor holding nil — that
// is exactly how the chat bug-report tool shipped permanently dark (fixed 1234aea37, found
// in production because the daemon logged the path as wired while the bot said it was
// unavailable). Every late-bound tool seam needs this test.
func TestSetMemoryWriteGate_WritesThroughToTheToolExecutor(t *testing.T) {
	a := &Agent{toolExecutor: &ToolExecutor{}}
	gate := &stubMemoryWriteGate{allow: map[string]bool{"slack|T1/C1#main": true}}

	a.SetMemoryWriteGate(gate)

	if a.toolExecutor.memoryWrite == nil {
		t.Fatal("tool executor still holds nil — the tool would refuse forever")
	}
	res := a.toolExecutor.remember(
		WithCallSiteForTest(context.Background(), "slack", "T1/C1#main"),
		`{"content":"x"}`,
	)
	if strings.Contains(res.Content, "not available on this deployment") {
		t.Errorf("still refusing after the gate was wired: %s", res.Content)
	}
}

// Nil agent and nil executor must both be safe: the container wires optional seams
// unconditionally.
func TestSetMemoryWriteGate_NilSafe(_ *testing.T) {
	var a *Agent
	a.SetMemoryWriteGate(&stubMemoryWriteGate{})
	(&Agent{}).SetMemoryWriteGate(&stubMemoryWriteGate{})
}

// The tool must be in the catalogue, or the model can never call it.
func TestRemember_IsInTheToolCatalogue(t *testing.T) {
	for _, tool := range DispatcherTools() {
		if tool.Function.Name == "remember" {
			if tool.Function.Parameters == nil {
				t.Fatal("remember has no parameter schema")
			}
			return
		}
	}
	t.Fatal("remember is not registered in DispatcherTools")
}
