package dispatcher

import (
	"context"
	"strings"
	"testing"
)

// SLICE 2, design §5.2. Review round 1 rejected content-based routing (it needed perfect
// classification per write); revision 3 withdrew the claim that the model never infers
// shared scope. What remains claimed: scope comes from instruction, and anything
// unrecognised fails safe to the narrower scope.
func TestResolveMemoryScope(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want memoryScope
	}{
		{"", memoryScopePersonal},
		{"personal", memoryScopePersonal},
		{"me", memoryScopePersonal},
		{"shared", memoryScopeShared},
		{"team", memoryScopeShared},
		{"for the team", memoryScopeShared},
		{"for everyone", memoryScopeShared},
		{"  SHARED  ", memoryScopeShared},
		{"For The Team", memoryScopeShared},
		// Plausible but unlisted: must NOT reach shared.
		{"for the group", memoryScopePersonal},
		{"public", memoryScopePersonal},
		{"all", memoryScopePersonal},
		{"share it widely", memoryScopePersonal},
		{"nonsense", memoryScopePersonal},
	} {
		if got := resolveMemoryScope(tc.in); got != tc.want {
			t.Errorf("resolveMemoryScope(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The asymmetry is the safety property: a wrongly-personal write is an annoyance the user
// repeats with clearer wording; a wrongly-shared write is a confidentiality breach that
// cannot be retroactively contained. So ambiguity must never resolve to shared.
func TestResolveMemoryScope_AmbiguityNeverResolvesToShared(t *testing.T) {
	for _, ambiguous := range []string{
		"maybe shared", "shared?", "team-ish", "not personal", "everyone else",
		"SHARED_SCOPE", "for-the-team", "team,project",
	} {
		if got := resolveMemoryScope(ambiguous); got == memoryScopeShared {
			t.Errorf("resolveMemoryScope(%q) resolved to SHARED; ambiguity must fail safe",
				ambiguous)
		}
	}
}

// The tool must report which scope it resolved, so the model can tell the user where the
// fact would go rather than guessing. Silence here is how a user discovers the scope was
// wrong only after a colleague reads it.
func TestRemember_ReportsTheResolvedScope(t *testing.T) {
	gate := &stubMemoryWriteGate{allow: map[string]bool{"slack|T1/C1#main": true}}
	te := &ToolExecutor{memoryWrite: gate}
	ctx := WithCallSiteForTest(context.Background(), "slack", "T1/C1#main")

	personal := te.remember(ctx, `{"content":"I prefer short answers"}`, "")
	if !strings.Contains(strings.ToLower(personal.Content), "personal") {
		t.Errorf("a default-scope call must say it resolved to personal: %s", personal.Content)
	}

	shared := te.remember(ctx, `{"content":"the deadline is Friday","scope":"for the team"}`, "")
	if !strings.Contains(strings.ToLower(shared.Content), "shared") {
		t.Errorf("an explicit shared call must say so: %s", shared.Content)
	}
}

// Slice 2 still has no write path, so neither scope may imply the fact was kept.
func TestRemember_NeitherScopeImpliesTheWriteHappened(t *testing.T) {
	gate := &stubMemoryWriteGate{allow: map[string]bool{"slack|T1/C1#main": true}}
	te := &ToolExecutor{memoryWrite: gate}
	ctx := WithCallSiteForTest(context.Background(), "slack", "T1/C1#main")

	for _, args := range []string{
		`{"content":"x"}`,
		`{"content":"x","scope":"shared"}`,
	} {
		low := strings.ToLower(te.remember(ctx, args, "").Content)
		for _, forbidden := range []string{"saved", "stored", "remembered"} {
			if strings.Contains(low, forbidden) {
				t.Errorf("args %q implies the write happened via %q", args, forbidden)
			}
		}
	}
}
