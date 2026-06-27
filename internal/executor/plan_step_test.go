package executor

import (
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// TestParseRoleClaimsExtractsCommitAndFilesChanged verifies that the
// parseRoleClaims helper correctly surfaces the commit / file-change
// claims from every JSON shape an agent might emit. The whole
// validation-against-git flow in plan_step.go is only as reliable as
// this extraction.
func TestParseRoleClaimsExtractsCommitAndFilesChanged(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantC  bool
		wantFC int
	}{
		{
			name:   "coder success",
			input:  `{"implementation":{"files_changed":3,"committed":true,"commit":"abc1234"}}`,
			wantC:  true,
			wantFC: 3,
		},
		{
			name:   "coder empty result",
			input:  `{"implementation":{"files_changed":0,"committed":false,"reason":"spec already satisfied"}}`,
			wantC:  false,
			wantFC: 0,
		},
		{
			name:   "architect success",
			input:  `{"architect":{"files_changed":2,"committed":true,"commit":"def5678"}}`,
			wantC:  true,
			wantFC: 2,
		},
		{
			name:   "tester no commit involvement",
			input:  `{"testing":{"passed":true,"ran":"make test"}}`,
			wantC:  false,
			wantFC: 0,
		},
		{
			name:   "reviewer approved (no files_changed field)",
			input:  `{"review":{"approved":true,"all_done":true,"checked_commit":"abc1234"}}`,
			wantC:  false,
			wantFC: 0,
		},
		{
			name:   "malformed JSON produces zero claims",
			input:  `{"implementation": {broken`,
			wantC:  false,
			wantFC: 0,
		},
		{
			name:   "empty body produces zero claims",
			input:  ``,
			wantC:  false,
			wantFC: 0,
		},
		{
			name:   "multi-field: takes the highest files_changed",
			input:  `{"implementation":{"files_changed":5},"architect":{"files_changed":10,"committed":true}}`,
			wantC:  true,
			wantFC: 10,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRoleClaims([]byte(tc.input))
			require.Equal(t, tc.wantC, got.claimedCommit,
				"claimedCommit mismatch for %s", tc.name)
			require.Equal(t, tc.wantFC, got.claimedFilesChanged,
				"claimedFilesChanged mismatch for %s", tc.name)
		})
	}
}

// TestResolvePlanRoles_SubstitutesViaAlias — the headline
// failure mode the alias mechanism exists to fix. Lead names
// "editor" but the swarm only has "writer"; the alias entry
// on the writer role catches the synonym and the plan step
// gets canonicalised. Without aliases, dropped + len(valid)==0
// caused the whole adaptive plan to fail (3 recent task
// failures across assistant + snake before this lands).
func TestResolvePlanRoles_SubstitutesViaAlias(t *testing.T) {
	roles := []registry.SwarmRole{
		{Name: "lead"},
		{Name: "researcher"},
		{Name: "writer", Aliases: []string{"editor", "reviewer"}},
	}
	res := resolvePlanRoles([]string{"researcher", "editor"}, roles)
	require.Equal(t, []string{"researcher", "writer"}, res.valid)
	require.Equal(t, []string{"editor→writer"}, res.substituted)
	require.Empty(t, res.dropped)
}

// TestResolvePlanRoles_DropsUnknownNoAlias — names without a
// canonical AND without an alias hit drop, log-only. Plan
// continues with the remaining valid steps when at least one
// is left.
func TestResolvePlanRoles_DropsUnknownNoAlias(t *testing.T) {
	roles := []registry.SwarmRole{
		{Name: "lead"},
		{Name: "writer", Aliases: []string{"editor"}},
	}
	res := resolvePlanRoles([]string{"writer", "qa", "lead"}, roles)
	require.Equal(t, []string{"writer", "lead"}, res.valid)
	require.Equal(t, []string{"qa"}, res.dropped)
}

// TestResolvePlanRoles_AllUnknownLeavesValidEmpty — when
// every named role is unknown AND has no alias hit, valid is
// empty so the caller fails the plan with the proper error.
func TestResolvePlanRoles_AllUnknownLeavesValidEmpty(t *testing.T) {
	roles := []registry.SwarmRole{
		{Name: "writer"},
	}
	res := resolvePlanRoles([]string{"editor", "reviewer"}, roles)
	require.Empty(t, res.valid)
	require.Equal(t, []string{"editor", "reviewer"}, res.dropped)
}

// TestResolvePlanRoles_AliasCollisionFirstSeenWins — when
// two roles claim the same alias, the first-seen role wins
// and the conflict surfaces as a collision the caller can log.
// The alternative (refuse the entire plan) would be hostile
// to operators who mistype a YAML — first-seen-wins is the
// least-surprising fallback and the log line tells them to
// rename.
func TestResolvePlanRoles_AliasCollisionFirstSeenWins(t *testing.T) {
	roles := []registry.SwarmRole{
		{Name: "writer", Aliases: []string{"editor"}},
		{Name: "lead", Aliases: []string{"editor"}}, // collision
	}
	res := resolvePlanRoles([]string{"editor"}, roles)
	require.Equal(t, []string{"writer"}, res.valid)
	require.Len(t, res.collisions, 1)
	require.Equal(t, "editor", res.collisions[0].alias)
	require.Equal(t, "writer", res.collisions[0].firstRole)
	require.Equal(t, "lead", res.collisions[0].conflictingRole)
}

// TestResolvePlanRoles_AliasMatchingCanonicalIsNoOp — an
// alias entry that matches its own canonical name (operator
// typo) must not corrupt the lookup, and an empty alias is
// silently skipped. The role still resolves directly.
func TestResolvePlanRoles_AliasMatchingCanonicalIsNoOp(t *testing.T) {
	roles := []registry.SwarmRole{
		{Name: "writer", Aliases: []string{"writer", ""}},
	}
	res := resolvePlanRoles([]string{"writer"}, roles)
	require.Equal(t, []string{"writer"}, res.valid)
	require.Empty(t, res.substituted)
}

// TestShortSha sanity-checks the SHA shortener used in error messages.
// It must not panic on short inputs and must truncate long ones to 7.
func TestShortSha(t *testing.T) {
	require.Equal(t, "", short(""))
	require.Equal(t, "abc", short("abc"))
	require.Equal(t, "abc1234", short("abc1234"))
	require.Equal(t, "abc1234", short("abc1234deadbeef"))
}

// TestParsePlanSteps_CanonicalShape verifies the happy path: a JSON
// envelope with `plan.steps` (array of role names) and an optional
// `message` parses cleanly and the message is forwarded as-is.
func TestParsePlanSteps_CanonicalShape(t *testing.T) {
	steps, msg, err := parsePlanSteps([]byte(`{"plan":{"steps":["scout","tester"]},"message":"ctx for first role"}`))
	require.NoError(t, err)
	require.Equal(t, []string{"scout", "tester"}, steps)
	require.Equal(t, "ctx for first role", msg)
}

// TestParsePlanSteps_RegressionWrongShape pins down the exact failure
// mode that motivated the fix in commit f6d1357. Some role
// systemPrompts told the lead to produce an array-of-objects shape:
//
//	{"plan": [{"role": "scout", "prompt": "..."}]}
//
// which collides with the parser's expected `plan: {steps: []string}`
// envelope. The unmarshaler raises "cannot unmarshal array into Go
// struct field" — keep this assertion specific so a future refactor
// that silently coerces both shapes can't pass tests without us
// noticing the contract change.
func TestParsePlanSteps_RegressionWrongShape(t *testing.T) {
	_, _, err := parsePlanSteps([]byte(`{"plan":[{"role":"scout","prompt":"..."}]}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid JSON from lead agent")
	require.Contains(t, err.Error(), "cannot unmarshal array")
}

// TestParsePlanSteps_EmptyData covers the explicit empty-input guard.
func TestParsePlanSteps_EmptyData(t *testing.T) {
	_, _, err := parsePlanSteps(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty result from lead agent")

	_, _, err = parsePlanSteps([]byte{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty result from lead agent")
}

// TestParsePlanSteps_MalformedJSON exercises the json.Unmarshal failure
// path so a trailing-comma typo on the lead's side surfaces as
// "invalid JSON" rather than something more confusing.
func TestParsePlanSteps_MalformedJSON(t *testing.T) {
	_, _, err := parsePlanSteps([]byte(`{"plan":{"steps":["scout",]},`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid JSON from lead agent")
}

// TestParsePlanSteps_NoStepsNoMessage is the silent-zero case: valid
// JSON, no steps, no message. The lead produced nothing usable; the
// parser must surface "plan contains no steps" rather than returning
// an empty slice and letting downstream code stall.
func TestParsePlanSteps_NoStepsNoMessage(t *testing.T) {
	_, _, err := parsePlanSteps([]byte(`{"plan":{"steps":[]},"message":""}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "lead agent plan contains no steps")
}

// TestParsePlanSteps_RefusalWithMessage covers the deliberate-refusal
// case: empty steps but a non-empty message. The lead is signaling
// it can't plan; the message text becomes the refusal reason and
// drives the corrective-retry path in runLeadPlanning.
func TestParsePlanSteps_RefusalWithMessage(t *testing.T) {
	_, _, err := parsePlanSteps([]byte(`{"plan":{"steps":[]},"message":"missing prerequisite: project not initialized"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "lead agent refused to plan")
	require.Contains(t, err.Error(), "missing prerequisite")
}

// TestParsePlanSteps_EmbeddedJSONFallback covers the
// extractPlanFromText path: a model that wrapped the plan inside the
// `message` field as mixed prose + JSON should still parse, since
// the human-readable preamble is common when a model "explains"
// before emitting structured output.
func TestParsePlanSteps_EmbeddedJSONFallback(t *testing.T) {
	body := `{"plan":{"steps":[]},"message":"Here is my plan: {\"plan\":{\"steps\":[\"scout\"]},\"message\":\"recovered\"}"}`
	steps, msg, err := parsePlanSteps([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"scout"}, steps)
	require.Equal(t, "recovered", msg)
}

// TestParsePlanSteps_StripsReasoningFromMessage protects against the
// `<think>...</think>` blocks some thinking models leak into the
// visible message. They must not survive into the message forwarded
// to the next role.
func TestParsePlanSteps_StripsReasoningFromMessage(t *testing.T) {
	body := `{"plan":{"steps":["scout"]},"message":"<think>internal reasoning</think>actual context"}`
	_, msg, err := parsePlanSteps([]byte(body))
	require.NoError(t, err)
	require.NotContains(t, msg, "internal reasoning")
	require.NotContains(t, msg, "<think>")
	require.Contains(t, msg, "actual context")
}
