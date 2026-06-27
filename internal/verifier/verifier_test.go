package verifier

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// withBodyReader installs a test body reader and returns a
// cleanup func. Lets tests skip the filesystem completely while
// still exercising the artifact-content verifiers.
func withBodyReader(t *testing.T, fn func(a *persistence.Artifact) ([]byte, error)) {
	t.Helper()
	prev := bodyReader
	bodyReader = fn
	t.Cleanup(func() { bodyReader = prev })
}

// TestArtifactMinEntries_PassesWhenCountMet — pin the canonical
// success path: a markdown file with N+1 list items satisfies
// a min=N verifier.
func TestArtifactMinEntries_PassesWhenCountMet(t *testing.T) {
	withBodyReader(t, func(a *persistence.Artifact) ([]byte, error) {
		return []byte("- item 1\n- item 2\n- item 3\n- item 4\n- item 5\n- item 6\n"), nil
	})
	cfg := Config{Type: "artifact_min_entries", Params: map[string]any{
		"artifact_pattern": "scan-*.md",
		"min":              5,
	}}
	in := Input{
		Artifacts: []*persistence.Artifact{
			{Name: "scan-eu-2026-05-02.md"},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "6 items must satisfy min=5")
}

// TestArtifactMinEntries_FailsWhenShortFall — the production
// failure mode: the agent reported success but the file has 1
// item where 5 were expected. Verifier returns a Violation so
// the executor fails the step.
func TestArtifactMinEntries_FailsWhenShortFall(t *testing.T) {
	withBodyReader(t, func(a *persistence.Artifact) ([]byte, error) {
		return []byte("- only item\n"), nil
	})
	cfg := Config{
		Name: "scan_min",
		Type: "artifact_min_entries",
		Params: map[string]any{
			"artifact_pattern": "scan-*.md",
			"min":              5,
		},
	}
	in := Input{Artifacts: []*persistence.Artifact{{Name: "scan-x.md"}}}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Equal(t, "scan_min", v.VerifierName)
	assert.Contains(t, v.Detail, "1 list-item")
	assert.Contains(t, v.Detail, "≥5")
}

// TestArtifactMinEntries_FailsWhenNoArtifactMatched — a step
// that produced NO artifact matching the required pattern is
// itself a violation. Without this branch, an empty output
// silently slipped through Phase 2.
func TestArtifactMinEntries_FailsWhenNoArtifactMatched(t *testing.T) {
	cfg := Config{Type: "artifact_min_entries", Params: map[string]any{
		"artifact_pattern": "scan-*.md",
		"min":              5,
	}}
	in := Input{Artifacts: []*persistence.Artifact{{Name: "wrong-name.txt"}}}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "no artifact matched pattern")
}

// TestNoStatus429_FlagsRateLimitInAudit — the canonical Phase 2
// failure mode the user complained about: scrape worker got
// a 429 but reported success.
func TestNoStatus429_FlagsRateLimitInAudit(t *testing.T) {
	cfg := Config{Type: "no_status_429_in_audit"}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{
				ToolName:   "mcp__scraper__web_fetch",
				ToolOutput: `{"status_code":429,"body":"<html>Too Many Requests</html>"}`,
			},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "429")
	assert.Contains(t, v.Detail, "mcp__scraper__web_fetch")
}

// TestNoStatus429_IgnoresMediaWikiCaptchaMention — the
// regression that motivated the rewrite (2026-05-14,
// task_20260514105901_676055db72a73dab). A successful Wikipedia /
// Wikivoyage fetch ships `wgConfirmEditCaptcha*` in its inline
// RLCONF JS config. The old marker scan saw "captcha" anywhere in
// tool_output and failed every successful MediaWiki fetch. With
// the structured-signal path the block_reason field is "" so the
// entry is a pass, not a block.
func TestNoStatus429_IgnoresMediaWikiCaptchaMention(t *testing.T) {
	cfg := Config{Type: "no_status_429_in_audit"}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{
				ToolName:   "mcp__scraper__web_fetch",
				ToolInput:  `{"url":"https://en.wikipedia.org/wiki/Phi_Phi_Islands"}`,
				ToolOutput: `{"status":200,"final_url":"https://en.wikipedia.org/wiki/Phi_Phi_Islands","body":"<html>wgConfirmEditCaptchaNeededForGenericEdit:false wgConfirmEditHCaptchaSiteKey:abc</html>","block_reason":"","block_detail":""}`,
			},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "successful MediaWiki fetch must not trip on inline captcha config strings")
}

// TestNoStatus429_StructuredSignalCatchesDataDome — the inverse
// pin: when the scraper itself sets block_reason, the verifier
// fires regardless of what's in the body. From the same incident
// audit row: tripadvisor.com returns 403 + a DataDome challenge,
// scraper sets block_reason="http_403".
func TestNoStatus429_StructuredSignalCatchesDataDome(t *testing.T) {
	cfg := Config{Type: "no_status_429_in_audit"}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{
				ToolName:   "mcp__scraper__web_fetch",
				ToolInput:  `{"url":"https://www.tripadvisor.com/foo"}`,
				ToolOutput: `{"status":403,"final_url":"https://www.tripadvisor.com/foo","body":"<iframe title=\"DataDome CAPTCHA\"/>","block_reason":"http_403","block_detail":"HTTP 403"}`,
			},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "scraper-emitted block_reason must fire the verifier")
	assert.Contains(t, v.Detail, "tripadvisor.com")
	assert.Contains(t, v.Detail, "http_403")
}

// TestNoStatus429_RatioToleratesPartialFailure — coverage check
// from feature item #1: a 30-fetch step with 1 block should pass
// when max_block_ratio is 0.2 (1/30 = 0.033). With the old zero-
// tolerance behaviour this would have failed the whole step.
func TestNoStatus429_RatioToleratesPartialFailure(t *testing.T) {
	cfg := Config{
		Type: "no_status_429_in_audit",
		Params: map[string]any{
			"max_block_ratio": 0.2,
		},
	}
	entries := make([]*persistence.ToolAuditEntry, 0, 30)
	// 29 successful fetches.
	for i := 0; i < 29; i++ {
		entries = append(entries, &persistence.ToolAuditEntry{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://ok.example/page"}`,
			ToolOutput: `{"status":200,"final_url":"https://ok.example/page","block_reason":""}`,
		})
	}
	// 1 genuinely blocked.
	entries = append(entries, &persistence.ToolAuditEntry{
		ToolName:   "mcp__scraper__web_fetch",
		ToolInput:  `{"url":"https://bad.example/page"}`,
		ToolOutput: `{"status":403,"final_url":"https://bad.example/page","block_reason":"http_403"}`,
	})
	v, err := Run(context.Background(), cfg, Input{AuditEntries: entries})
	require.NoError(t, err)
	assert.Nil(t, v, "1/30 blocked with max_block_ratio=0.2 must pass")
}

// TestNoStatus429_RatioStillFailsWhenAboveThreshold — ensure the
// threshold is real, not a no-op: 3 blocked / 4 total = 0.75 must
// exceed a 0.5 ratio.
func TestNoStatus429_RatioStillFailsWhenAboveThreshold(t *testing.T) {
	cfg := Config{
		Type: "no_status_429_in_audit",
		Params: map[string]any{
			"max_block_ratio": 0.5,
		},
	}
	entries := []*persistence.ToolAuditEntry{
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://ok.example"}`,
			ToolOutput: `{"status":200,"final_url":"https://ok.example","block_reason":""}`,
		},
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://b1.example"}`,
			ToolOutput: `{"status":429,"final_url":"https://b1.example","block_reason":"rate_limit"}`,
		},
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://b2.example"}`,
			ToolOutput: `{"status":403,"final_url":"https://b2.example","block_reason":"http_403"}`,
		},
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://b3.example"}`,
			ToolOutput: `{"status":403,"final_url":"https://b3.example","block_reason":"captcha"}`,
		},
	}
	v, err := Run(context.Background(), cfg, Input{AuditEntries: entries})
	require.NoError(t, err)
	require.NotNil(t, v, "3/4 blocked must exceed 0.5 ratio")
	assert.Contains(t, v.Detail, "ratio 0.75")
}

// TestNoStatus429_MinSuccessfulFloor — a step where everything
// failed (denominator dominated by blocks) must fail even when
// max_block_ratio would allow it, if the operator sets a floor.
func TestNoStatus429_MinSuccessfulFloor(t *testing.T) {
	cfg := Config{
		Type: "no_status_429_in_audit",
		Params: map[string]any{
			"max_block_ratio":        1.0, // ratio gate disabled
			"min_successful_fetches": 3,
		},
	}
	// Only 2 successful — below the floor of 3.
	entries := []*persistence.ToolAuditEntry{
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://ok1.example"}`,
			ToolOutput: `{"status":200,"final_url":"https://ok1.example","block_reason":""}`,
		},
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://ok2.example"}`,
			ToolOutput: `{"status":200,"final_url":"https://ok2.example","block_reason":""}`,
		},
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://b1.example"}`,
			ToolOutput: `{"status":403,"final_url":"https://b1.example","block_reason":"http_403"}`,
		},
	}
	v, err := Run(context.Background(), cfg, Input{AuditEntries: entries})
	require.NoError(t, err)
	require.NotNil(t, v, "2 successful below floor of 3 must fail")
	assert.Contains(t, v.Detail, "below floor")
}

// TestNoStatus429_ExcuseSkipPathFromResult — feature item #4
// cross-check: a URL the agent acknowledged as blocked in
// result.json's skipped[] (with a documented reason) does NOT
// count against the verifier.
func TestNoStatus429_ExcuseSkipPathFromResult(t *testing.T) {
	cfg := Config{
		Type: "no_status_429_in_audit",
		Params: map[string]any{
			"excuse_skip_path": "skipped",
		},
	}
	entries := []*persistence.ToolAuditEntry{
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://ok.example"}`,
			ToolOutput: `{"status":200,"final_url":"https://ok.example","block_reason":""}`,
		},
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://tripadvisor.com/foo"}`,
			ToolOutput: `{"status":403,"final_url":"https://tripadvisor.com/foo","block_reason":"http_403"}`,
		},
	}
	result := []byte(`{
		"skipped": [
			{"url":"https://tripadvisor.com/foo","reason":"datadome_challenge","alternative":"used wikitravel instead"}
		]
	}`)
	v, err := Run(context.Background(), cfg, Input{
		AuditEntries: entries,
		ResultJSON:   result,
	})
	require.NoError(t, err)
	assert.Nil(t, v, "documented skip must excuse the block")
}

// TestNoStatus429_ExcuseRequiresReason — empty-reason entries
// must NOT excuse a block, mirroring hasDocumentedSkipped's
// contract. Otherwise a model could trivially launder failures.
func TestNoStatus429_ExcuseRequiresReason(t *testing.T) {
	cfg := Config{
		Type: "no_status_429_in_audit",
		Params: map[string]any{
			"excuse_skip_path": "skipped",
		},
	}
	entries := []*persistence.ToolAuditEntry{
		{
			ToolName:   "mcp__scraper__web_fetch",
			ToolInput:  `{"url":"https://bad.example"}`,
			ToolOutput: `{"status":403,"final_url":"https://bad.example","block_reason":"http_403"}`,
		},
	}
	v, err := Run(context.Background(), cfg, Input{
		AuditEntries: entries,
		ResultJSON:   []byte(`{"skipped":[{"url":"https://bad.example","reason":""}]}`),
	})
	require.NoError(t, err)
	require.NotNil(t, v, "skip without a reason must not launder a block")
}

// TestNoStatus429_SeverityOverrideFromConfig — feature item #2:
// operators can downgrade this verifier to warn-tier via YAML so
// the must_contain_url rule owns the hard-fail path.
func TestNoStatus429_SeverityOverrideFromConfig(t *testing.T) {
	cfg := Config{
		Type:     "no_status_429_in_audit",
		Severity: "warn",
	}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{
				ToolName:   "mcp__scraper__web_fetch",
				ToolInput:  `{"url":"https://x.example"}`,
				ToolOutput: `{"status":403,"final_url":"https://x.example","block_reason":"http_403"}`,
			},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Equal(t, SeverityWarn, v.Severity)
	assert.Contains(t, v.Error(), "[warn]")
}

// TestNoStatus429_DefaultSeverityIsFail — backward compat pin:
// every existing YAML config and every existing test relies on
// "violation == hard fail". Severity must default to fail when
// neither the verifier nor the config overrides it.
func TestNoStatus429_DefaultSeverityIsFail(t *testing.T) {
	cfg := Config{Type: "no_status_429_in_audit"}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "fetch", ToolOutput: `status=429`},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Equal(t, SeverityFail, v.Severity)
}

// TestConfigFromMap_ParsesSeverity covers the YAML decode for the
// new severity key so operators can actually set it.
func TestConfigFromMap_ParsesSeverity(t *testing.T) {
	cfg, ok := ConfigFromMap(map[string]any{
		"type":     "no_status_429_in_audit",
		"severity": "warn",
	})
	require.True(t, ok)
	assert.Equal(t, "warn", cfg.Severity)
}

// TestNoStatus429_ScopedToTools — the verifier honours its
// `tools` filter so a 429 from an unrelated MCP doesn't trip
// it. Concretely: a scout step's 429 against a different
// portal isn't a violation if the verifier was set for the
// scrape portal only.
func TestNoStatus429_ScopedToTools(t *testing.T) {
	cfg := Config{
		Type: "no_status_429_in_audit",
		Params: map[string]any{
			"tools": []any{"mcp__a__fetch"},
		},
	}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "mcp__b__fetch", ToolOutput: `status_code":429`},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "429 from a tool outside the scope must NOT trip the verifier")
}

// TestMustContainURL_FlagsMissingURL — explicit positive
// requirement: every URL listed in params must show up in the
// audit. Useful for "you were supposed to scrape these portals"
// rules.
func TestMustContainURL_FlagsMissingURL(t *testing.T) {
	cfg := Config{Type: "must_contain_url", Params: map[string]any{
		"urls": []any{"https://a.example", "https://b.example"},
	}}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolInput: "https://a.example"},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "https://b.example")
	assert.NotContains(t, v.Detail, "https://a.example")
}

func TestMustContainURL_RejectsEmptyURLListAfterTrimming(t *testing.T) {
	cfg := Config{Type: "must_contain_url", Params: map[string]any{
		"urls": []any{"", "   "},
	}}
	v, err := Run(context.Background(), cfg, Input{
		AuditEntries: []*persistence.ToolAuditEntry{{ToolInput: "anything"}},
	})
	require.Error(t, err)
	assert.Nil(t, v)
	assert.Contains(t, err.Error(), "urls is required")
}

func TestArtifactMinEntries_RejectsFractionalMin(t *testing.T) {
	cfg := Config{Type: "artifact_min_entries", Params: map[string]any{
		"artifact_pattern": "scan-*.md",
		"min":              1.5,
	}}
	v, err := Run(context.Background(), cfg, Input{})
	require.Error(t, err)
	assert.Nil(t, v)
	assert.Contains(t, err.Error(), "min must be > 0")
}

func TestRunHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := Run(ctx, Config{Type: "no_status_429_in_audit"}, Input{})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, v)
}

// TestRunAll_AggregatesViolations — multi-rule projects need
// every failure surfaced, not just the first. The executor
// passes the full slice into the error string so the operator
// can see all gaps in one retry.
func TestRunAll_AggregatesViolations(t *testing.T) {
	cfgs := []Config{
		{Name: "min", Type: "artifact_min_entries", Params: map[string]any{
			"artifact_pattern": "scan-*.md", "min": 5,
		}},
		{Name: "block", Type: "no_status_429_in_audit"},
	}
	withBodyReader(t, func(a *persistence.Artifact) ([]byte, error) {
		return []byte("- one\n"), nil
	})
	in := Input{
		Artifacts: []*persistence.Artifact{{Name: "scan-x.md"}},
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "fetch", ToolOutput: `status=429`},
		},
	}
	vs := RunAll(context.Background(), cfgs, in)
	require.Len(t, vs, 2)
	names := []string{vs[0].VerifierName, vs[1].VerifierName}
	assert.Contains(t, names, "min")
	assert.Contains(t, names, "block")
}

// TestWhenTaskType_FiltersByType — task-type scoping must work,
// otherwise verifiers run on tasks they shouldn't.
func TestWhenTaskType_FiltersByType(t *testing.T) {
	cfg := Config{
		Name:         "research_only",
		Type:         "no_status_429_in_audit",
		WhenTaskType: "research",
	}
	in := Input{
		TaskType: "feature",
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "fetch", ToolOutput: "status=429"},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "verifier scoped to research must skip a feature task")
}

// TestWhenStep_FiltersByStep — step-scoping must work so a
// verifier scoped to the trading "execute" step doesn't fail
// the strategist/risk-officer steps that precede it. A NO_ACTION
// tick (gate routes past the execute step) naturally skips the
// verifier because the engine isn't invoked for steps that
// never ran.
func TestWhenStep_FiltersByStep(t *testing.T) {
	cfg := Config{
		Name:     "executor_only",
		Type:     "must_contain_url",
		WhenStep: "execute",
		Params:   map[string]any{"urls": []string{"mcp__broker__place_order"}},
	}
	// A strategize-step input lacks place_order in the audit —
	// that should NOT fail the verifier because the verifier is
	// scoped to the execute step.
	in := Input{
		TaskType: "trading",
		StepID:   "strategize",
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "mcp__broker__get_quote", ToolOutput: "{}"},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "verifier scoped to execute must skip strategize step")

	// Same Config + audit on the executor step, with no
	// place_order call — must fail because the executor was
	// supposed to place orders.
	in.StepID = "execute"
	v, err = Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "verifier scoped to execute must fire on the execute step")
	assert.Contains(t, v.Detail, "mcp__broker__place_order")
}

// TestConfigFromMap_ParsesWhenStep covers both the camelCase and
// snake_case YAML keys for whenStep so existing operator-authored
// templates accepting either spelling continue to work.
func TestConfigFromMap_ParsesWhenStep(t *testing.T) {
	camel, ok := ConfigFromMap(map[string]any{
		"type":     "must_contain_url",
		"whenStep": "execute",
		"params":   map[string]any{"urls": []string{"x"}},
	})
	require.True(t, ok)
	assert.Equal(t, "execute", camel.WhenStep)

	snake, ok := ConfigFromMap(map[string]any{
		"type":      "must_contain_url",
		"when_step": "execute",
		"params":    map[string]any{"urls": []string{"x"}},
	})
	require.True(t, ok)
	assert.Equal(t, "execute", snake.WhenStep)
}

// TestUnknownVerifierType_PropagatesError — a typo in the YAML
// type field surfaces as a Violation tagged config_error so
// operators see "I wrote 'artfact_min_entries' instead of
// 'artifact_min_entries'" without it silently no-opping.
func TestUnknownVerifierType_PropagatesError(t *testing.T) {
	cfgs := []Config{{Name: "typo", Type: "artfact_min_entries"}}
	vs := RunAll(context.Background(), cfgs, Input{})
	require.Len(t, vs, 1)
	assert.Contains(t, vs[0].Type, "config_error")
}

// TestProposalsMatchWatchlist_FlagsHallucinatedSymbol — the
// motivating case: kimi-k2.5 strategist saw "SHEL" in the
// pre-warmed indicator block and proposed "SHELL" in
// proposals[]. The verifier must catch this as a violation.
func TestProposalsMatchWatchlist_FlagsHallucinatedSymbol(t *testing.T) {
	cfg := Config{Type: "proposals_match_watchlist"}
	in := Input{
		WatchlistAllowList: []string{"AAPL", "MSFT", "SHEL", "TSLA"},
		ResultJSON: []byte(`{
			"proposals": [
				{"symbol": "SHELL", "action": "BUY"}
			],
			"has_proposals": true
		}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "expected violation for off-watchlist symbol")
	assert.Contains(t, v.Detail, "SHELL")
	assert.Contains(t, v.Detail, "SHEL") // the watchlist is shown for ops triage
}

// TestProposalsMatchWatchlist_AcceptsKnownSymbols — clean case:
// every proposed symbol is in the watchlist, no violation.
func TestProposalsMatchWatchlist_AcceptsKnownSymbols(t *testing.T) {
	cfg := Config{Type: "proposals_match_watchlist"}
	in := Input{
		WatchlistAllowList: []string{"AAPL", "MSFT", "TSLA"},
		ResultJSON: []byte(`{
			"proposals": [
				{"symbol": "AAPL"},
				{"symbol": "TSLA"}
			]
		}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestProposalsMatchWatchlist_NoOpWhenWatchlistEmpty — non-trading
// project (or trading project with empty watchlist) gets a clean
// no-op so the verifier doesn't false-positive everywhere it's
// declared but doesn't apply.
func TestProposalsMatchWatchlist_NoOpWhenWatchlistEmpty(t *testing.T) {
	cfg := Config{Type: "proposals_match_watchlist"}
	in := Input{
		ResultJSON: []byte(`{"proposals":[{"symbol":"ANYTHING"}]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestProposalsMatchWatchlist_HoistsFromMessageField — the
// envelope-merge fallback: when the agent harness's pass-3
// extraction failed and proposals ended up inside the `message`
// JSON-string, the verifier still recovers them.
func TestProposalsMatchWatchlist_HoistsFromMessageField(t *testing.T) {
	cfg := Config{Type: "proposals_match_watchlist"}
	in := Input{
		WatchlistAllowList: []string{"AAPL"},
		ResultJSON: []byte(`{
			"status": "COMPLETED",
			"message": "{\"proposals\": [{\"symbol\": \"BOGUS\"}]}"
		}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "expected violation when proposals were buried in message")
	assert.Contains(t, v.Detail, "BOGUS")
}

// TestProposalsMatchWatchlist_NormalisesCase — IBKR symbols are
// upper-case canonical. Allow watchlists with mixed case (a
// legitimate operator typo) without false-positiving.
func TestProposalsMatchWatchlist_NormalisesCase(t *testing.T) {
	cfg := Config{Type: "proposals_match_watchlist"}
	in := Input{
		WatchlistAllowList: []string{"shel", "AAPL"},
		ResultJSON:         []byte(`{"proposals":[{"symbol":"SHEL"}]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestMustContainURL_SkipAlternativePassesWithDocumentedSkip —
// the escape hatch: when the executor approved orders but
// legitimately skipped (broker rejected with no-conId, quote
// drifted, etc.), the verifier treats skipped[].reason as
// equivalent to a place_order audit row so the operator doesn't
// get paged for correct behaviour.
func TestMustContainURL_SkipAlternativePassesWithDocumentedSkip(t *testing.T) {
	cfg := Config{Type: "must_contain_url", Params: map[string]any{
		"urls":                  []any{"mcp__broker__place_order"},
		"skip_alternative_path": "skipped",
	}}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolInput: "get_quote(SHELL)"},
		},
		ResultJSON: []byte(`{
			"placed": [],
			"skipped": [{"symbol":"SHELL","reason":"quote_drifted","detail":"no conId"}]
		}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "documented skip must satisfy the verifier")
}

// TestMustContainURL_SkipAlternativeRejectsEmptyReason — the
// escape hatch must require the reason field; otherwise a model
// could trivially bypass the verifier by emitting `[{}]` and
// calling it a skip.
func TestMustContainURL_SkipAlternativeRejectsEmptyReason(t *testing.T) {
	cfg := Config{Type: "must_contain_url", Params: map[string]any{
		"urls":                  []any{"mcp__broker__place_order"},
		"skip_alternative_path": "skipped",
	}}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{{ToolInput: "noise"}},
		ResultJSON:   []byte(`{"skipped":[{"symbol":"X"}]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "skip without a documented reason must NOT bypass the verifier")
}

// TestMustContainURL_SkipAlternativeFallsBackForFullyQualifiedFlow —
// when both URLs are present in audit AND skipped[] is empty
// (the all-orders-placed happy path), the verifier passes via
// the URL-presence path, not the skip alternative.
func TestMustContainURL_SkipAlternativeFallsBackForFullyQualifiedFlow(t *testing.T) {
	cfg := Config{Type: "must_contain_url", Params: map[string]any{
		"urls":                  []any{"mcp__broker__place_order"},
		"skip_alternative_path": "skipped",
	}}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolInput: "mcp__broker__place_order(symbol=AAPL)"},
		},
		ResultJSON: []byte(`{"placed":[{"symbol":"AAPL"}],"skipped":[]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestAppliesTo_SkipsWhenSourceNotInAllowlist pins the
// canonical gating semantics: a verifier scoped to autonomous
// tasks (the autonomy-shape rules like scan_min_entries) must
// no-op when the runtime hands it a USER-created task. Without
// this, operator-initiated ad-hoc work (ingest a CV, summarise
// an email) trips the "artifact must match scan-*.md" contract
// that only fits the autonomy loop.
func TestAppliesTo_SkipsWhenSourceNotInAllowlist(t *testing.T) {
	cfg := Config{
		Name:      "scan_min",
		Type:      "artifact_min_entries",
		AppliesTo: []string{"autonomous"},
		Params: map[string]any{
			"artifact_pattern": "scan-*.md",
			"min":              5,
		},
	}
	// No matching artifact — without gating this would fail with
	// "no artifact matched pattern". With gating, the verifier
	// skips cleanly because the task wasn't autonomy-shaped.
	in := Input{
		CreationSource: "USER",
		Artifacts:      []*persistence.Artifact{{Name: "cv-ingest.md"}},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "USER task must skip an autonomous-only verifier")
}

// TestAppliesTo_RunsWhenSourceMatches — the inverse: an
// autonomy tick (CreationSource=AUTONOMOUS) must still see the
// scan_min_entries rule fire when the artifact is missing.
func TestAppliesTo_RunsWhenSourceMatches(t *testing.T) {
	cfg := Config{
		Name:      "scan_min",
		Type:      "artifact_min_entries",
		AppliesTo: []string{"autonomous"},
		Params: map[string]any{
			"artifact_pattern": "scan-*.md",
			"min":              5,
		},
	}
	in := Input{
		CreationSource: "AUTONOMOUS",
		Artifacts:      []*persistence.Artifact{{Name: "wrong-name.txt"}},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "AUTONOMOUS task must still trigger the verifier")
	assert.Contains(t, v.Detail, "no artifact matched")
}

// TestAppliesTo_EmptyMeansAllSources — backward-compat pin: a
// verifier without an appliesTo list (the legacy shape) runs
// for every source, including USER. Tests that pre-existed the
// gating feature continue to pass.
func TestAppliesTo_EmptyMeansAllSources(t *testing.T) {
	cfg := Config{
		Name: "scan_min",
		Type: "artifact_min_entries",
		Params: map[string]any{
			"artifact_pattern": "scan-*.md",
			"min":              5,
		},
	}
	for _, src := range []string{"USER", "AUTONOMOUS", "DELEGATION", ""} {
		in := Input{
			CreationSource: src,
			Artifacts:      []*persistence.Artifact{{Name: "wrong-name.txt"}},
		}
		v, err := Run(context.Background(), cfg, in)
		require.NoError(t, err, "src=%s", src)
		require.NotNil(t, v, "src=%s: empty appliesTo must run for every source", src)
	}
}

// TestAppliesTo_CaseInsensitive — operators write YAML in
// lower-case ("autonomous") but the runtime stores the source
// in upper-case ("AUTONOMOUS"). The match must work either way
// so a config file isn't shape-fragile.
func TestAppliesTo_CaseInsensitive(t *testing.T) {
	cfg := Config{
		Type:      "artifact_min_entries",
		AppliesTo: []string{"AUTONOMOUS", "user"},
		Params: map[string]any{
			"artifact_pattern": "x-*.md",
			"min":              1,
		},
	}
	for _, src := range []string{"USER", "AUTONOMOUS", "user", "autonomous"} {
		in := Input{
			CreationSource: src,
			Artifacts:      []*persistence.Artifact{{Name: "no-match.txt"}},
		}
		v, err := Run(context.Background(), cfg, in)
		require.NoError(t, err, "src=%s", src)
		require.NotNil(t, v, "src=%s must trigger (case-insensitive match)", src)
	}
}

// TestAppliesTo_UnknownSourceBlocksWhenAllowlisted — when
// CreationSource is empty (legacy task rows that pre-date the
// column, or a test that forgot to set it), a verifier with an
// explicit allowlist is treated as not-applicable. Safer than
// the alternative: an autonomy-shape rule running against a
// task whose shape we can't classify.
func TestAppliesTo_UnknownSourceBlocksWhenAllowlisted(t *testing.T) {
	cfg := Config{
		Type:      "artifact_min_entries",
		AppliesTo: []string{"autonomous"},
		Params: map[string]any{
			"artifact_pattern": "x-*.md",
			"min":              1,
		},
	}
	in := Input{
		CreationSource: "",
		Artifacts:      []*persistence.Artifact{{Name: "no-match.txt"}},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "empty CreationSource + explicit allowlist must skip the verifier")
}

// TestConfigFromMap_ParsesAppliesTo pins the YAML decode path
// that loads verifier configs at startup. Without this round-
// trip, a working appliesTo dispatch is invisible to YAML
// operators.
func TestConfigFromMap_ParsesAppliesTo(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want []string
	}{
		{
			name: "camelCase_yaml",
			in: map[string]any{
				"type":      "artifact_min_entries",
				"appliesTo": []any{"autonomous"},
			},
			want: []string{"autonomous"},
		},
		{
			name: "snake_case_yaml",
			in: map[string]any{
				"type":       "artifact_min_entries",
				"applies_to": []any{"USER", "autonomous"},
			},
			want: []string{"USER", "autonomous"},
		},
		{
			name: "typed_string_slice",
			in: map[string]any{
				"type":      "artifact_min_entries",
				"appliesTo": []string{"autonomous"},
			},
			want: []string{"autonomous"},
		},
		{
			name: "empty_returns_nil",
			in: map[string]any{
				"type":      "artifact_min_entries",
				"appliesTo": []any{},
			},
			want: nil,
		},
		{
			name: "missing_returns_nil",
			in: map[string]any{
				"type": "artifact_min_entries",
			},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, ok := ConfigFromMap(c.in)
			require.True(t, ok)
			assert.Equal(t, c.want, cfg.AppliesTo)
		})
	}
}

func TestDefaultBodyReaderRejectsOversizedArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.md")
	body := make([]byte, maxVerifierBodyBytes+1)
	for i := range body {
		body[i] = 'x'
	}
	require.NoError(t, os.WriteFile(path, body, 0o644))

	_, err := defaultBodyReader(&persistence.Artifact{Name: "large.md", StoragePath: path})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds verifier read cap")
}

// TestPlacementsMatchAudit_FlagsHallucinatedPlacement is the motivating
// failure: minimax.minimax-m2.5 emitted placed[] with sequential fake
// broker_order_ids ("86287740"...) and hexalphabetical
// idempotency_keys, never calling mcp__broker__place_order. The verifier
// must fire and surface the precise claim-vs-audit mismatch in the
// detail message so triage doesn't depend on string-matching against
// "URL absent".
func TestPlacementsMatchAudit_FlagsHallucinatedPlacement(t *testing.T) {
	result := []byte(`{
		"placed": [
			{"symbol":"TSM","broker_order_id":"86287744","status":"submitted","idempotency_key":"a1b2c3d4e5f6789012345678abcdef0123456789abcdef0123456789abcd"}
		],
		"skipped": [],
		"fills_observed": []
	}`)
	cfg := Config{Name: "placements_match_audit", Type: "placements_match_audit"}
	in := Input{
		StepID:     "execute",
		ResultJSON: result,
		AuditEntries: []*persistence.ToolAuditEntry{
			// Note: NO mcp__broker__place_order call, only get_quote.
			{ToolName: "mcp__broker__get_quote", ToolOutput: `{"last":146.3}`},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "claim of 1 placement with 0 audit calls must fail")
	assert.Equal(t, "placements_match_audit", v.VerifierName)
	assert.Contains(t, v.Detail, "hallucinated placement")
	assert.Contains(t, v.Detail, "1 placed entry")
	assert.Contains(t, v.Detail, "0")
	assert.Contains(t, v.Detail, "mcp__broker__place_order")
}

// TestPlacementsMatchAudit_PassesWhenClaimMatchesAudit — the happy path:
// agent emits placed[] with N entries and the audit has at least N
// place_order calls. Common case: a tick where the broker actually
// took the order.
func TestPlacementsMatchAudit_PassesWhenClaimMatchesAudit(t *testing.T) {
	result := []byte(`{"placed":[{"symbol":"AAPL"},{"symbol":"MSFT"}]}`)
	cfg := Config{Type: "placements_match_audit"}
	in := Input{
		StepID:     "execute",
		ResultJSON: result,
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "mcp__broker__place_order", ToolOutput: `{"broker_order_id":"X"}`},
			{ToolName: "mcp__broker__place_order", ToolOutput: `{"broker_order_id":"Y"}`},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "2 claims + 2 audit calls is the happy path")
}

// TestPlacementsMatchAudit_PassesWhenClaimEmpty — the verifier is
// one-way: empty placed[] is fine regardless of audit. A legit "broker
// offline, all skipped" tick has no claims to cross-check.
func TestPlacementsMatchAudit_PassesWhenClaimEmpty(t *testing.T) {
	result := []byte(`{"placed":[],"skipped":[{"symbol":"AAPL","reason":"broker_unreachable_preflight"}]}`)
	cfg := Config{Type: "placements_match_audit"}
	in := Input{
		StepID:     "execute",
		ResultJSON: result,
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "empty placed[] must not fire — nothing claimed to cross-check")
}

// TestPlacementsMatchAudit_PassesWhenAuditExceedsClaim — under-counting
// is intentionally not a failure. The agent might have legitimately
// placed an order and then forgotten to include it in placed[] (a
// separate bug class). We're only catching claim inflation.
func TestPlacementsMatchAudit_PassesWhenAuditExceedsClaim(t *testing.T) {
	result := []byte(`{"placed":[{"symbol":"AAPL"}]}`)
	cfg := Config{Type: "placements_match_audit"}
	in := Input{
		StepID:     "execute",
		ResultJSON: result,
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "mcp__broker__place_order"},
			{ToolName: "mcp__broker__place_order"},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "claim < audit is fine (potential under-reporting, separate class)")
}

// TestPlacementsMatchAudit_CustomToolAndPath — the verifier is generic;
// trading happens to use ("placed", "mcp__broker__place_order") but
// another workflow could parameterise both. Make sure the params plumb
// through.
func TestPlacementsMatchAudit_CustomToolAndPath(t *testing.T) {
	result := []byte(`{"dispatched":[{"id":"a"},{"id":"b"},{"id":"c"}]}`)
	cfg := Config{
		Type: "placements_match_audit",
		Params: map[string]any{
			"tool":       "mcp__custom__dispatch",
			"claim_path": "dispatched",
		},
	}
	in := Input{
		ResultJSON: result,
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "mcp__custom__dispatch"},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "3 dispatched but only 1 audit call must fail")
	assert.Contains(t, v.Detail, "3 dispatched entries")
	assert.Contains(t, v.Detail, "mcp__custom__dispatch")
}

// TestPlacementsMatchAudit_HandlesEnvelopeMessageFallback — the
// envelope-merge path. When the model's structured output didn't get
// hoisted to top level, the claim still sits inside envelope.message
// as a JSON string. extractProposalSymbols already handles this for
// proposals; placements_match_audit should match.
func TestPlacementsMatchAudit_HandlesEnvelopeMessageFallback(t *testing.T) {
	result := []byte(`{"message":"{\"placed\":[{\"symbol\":\"TSLA\"},{\"symbol\":\"NVDA\"}]}"}`)
	cfg := Config{Type: "placements_match_audit"}
	in := Input{
		ResultJSON: result,
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "mcp__broker__place_order"},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "envelope.message fallback must extract placed[] and fire when claim > audit")
	assert.Contains(t, v.Detail, "2 placed entries")
}

// TestPlacementsMatchAudit_IgnoresMalformedResultJSON — silent no-op
// when result.json can't be parsed. A different verifier (shape
// validator upstream) handles that class; we don't want a parse error
// to mask an actual hallucination failure with a config_error.
func TestPlacementsMatchAudit_IgnoresMalformedResultJSON(t *testing.T) {
	cfg := Config{Type: "placements_match_audit"}
	in := Input{ResultJSON: []byte(`{not json`)}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "malformed result.json must leave the verifier silent")
}

// TestPlacementsMatchAudit_IgnoresNonArrayClaimPath — when claim_path
// resolves to a scalar / object / null, the verifier silently no-ops
// (the shape was wrong but this verifier doesn't claim shape
// authority).
func TestPlacementsMatchAudit_IgnoresNonArrayClaimPath(t *testing.T) {
	cfg := Config{Type: "placements_match_audit"}
	in := Input{ResultJSON: []byte(`{"placed":"not-an-array"}`)}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "non-array placed must not fire — shape mismatch is a different class")
}

// TestPlacementsMatchAudit_HonorsNilAuditEntries — defensive: an
// auditEntries slice with nil interlopers must not panic and must not
// be counted as place_order calls.
func TestPlacementsMatchAudit_HonorsNilAuditEntries(t *testing.T) {
	cfg := Config{Type: "placements_match_audit"}
	in := Input{
		ResultJSON: []byte(`{"placed":[{"x":1}]}`),
		AuditEntries: []*persistence.ToolAuditEntry{
			nil,
			{ToolName: "mcp__broker__get_quote"},
			nil,
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "claim=1 with no place_order audit must fire (nils ignored)")
}

// TestPlacementsMatchAudit_PluralYHelper covers both pluralisation
// branches via the public interface (1 → "entry", 2 → "entries").
func TestPlacementsMatchAudit_PluralYHelper(t *testing.T) {
	cfg := Config{Type: "placements_match_audit"}
	single := Input{ResultJSON: []byte(`{"placed":[{"a":1}]}`)}
	v, err := Run(context.Background(), cfg, single)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "1 placed entry ")

	multi := Input{ResultJSON: []byte(`{"placed":[{"a":1},{"b":2}]}`)}
	v, err = Run(context.Background(), cfg, multi)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "2 placed entries")
}

// TestConfigsFromMaps_DropsMalformed pins the bulk loader: a slice of
// configs with a missing `type` field gets dropped and counted, rather
// than blowing up the whole project's verifier registration.
func TestConfigsFromMaps_DropsMalformed(t *testing.T) {
	in := []map[string]any{
		{"type": "must_contain_url", "params": map[string]any{"urls": []string{"x"}}},
		{"name": "no-type-here"},
		{"type": "no_status_429_in_audit"},
	}
	cfgs, skipped := ConfigsFromMaps(in)
	assert.Equal(t, 2, len(cfgs), "two well-formed configs survive")
	assert.Equal(t, 1, skipped, "one malformed config dropped")

	empty, skippedEmpty := ConfigsFromMaps(nil)
	assert.Nil(t, empty)
	assert.Zero(t, skippedEmpty)
}

// TestArtifactNonEmpty_PassesAndFails — direct coverage on the
// lighter verify_artifact_non_empty path (the bulk
// no_empty_artifacts has its own test below).
func TestArtifactNonEmpty_PassesAndFails(t *testing.T) {
	sz := int64(42)
	zero := int64(0)
	cfg := Config{
		Type: "artifact_non_empty",
		Params: map[string]any{
			"artifact_pattern": "out-*.txt",
		},
	}

	pass, err := Run(context.Background(), cfg, Input{
		Artifacts: []*persistence.Artifact{{Name: "out-a.txt", SizeBytes: &sz}},
	})
	require.NoError(t, err)
	assert.Nil(t, pass, "non-zero-size artifact must pass")

	empty, err := Run(context.Background(), cfg, Input{
		Artifacts: []*persistence.Artifact{{Name: "out-b.txt", SizeBytes: &zero}},
	})
	require.NoError(t, err)
	require.NotNil(t, empty)
	assert.Contains(t, empty.Detail, "is empty")

	missing, err := Run(context.Background(), cfg, Input{
		Artifacts: []*persistence.Artifact{{Name: "wrong.bin", SizeBytes: &sz}},
	})
	require.NoError(t, err)
	require.NotNil(t, missing)
	assert.Contains(t, missing.Detail, "no artifact matched pattern")

	// Required-param branch.
	_, err = Run(context.Background(), Config{Type: "artifact_non_empty"}, Input{})
	require.Error(t, err)
}

// TestNoEmptyArtifacts_OnlyFlagsOutputClass — the bulk variant skips
// input artifacts (it would false-fail "input file you read" cases)
// and only fails on output-class artifacts that are empty.
func TestNoEmptyArtifacts_OnlyFlagsOutputClass(t *testing.T) {
	zero := int64(0)
	nonzero := int64(7)
	cfg := Config{Type: "no_empty_artifacts"}

	// Input-class empty artifact: must pass (we're not the gatekeeper for inputs).
	pass, err := Run(context.Background(), cfg, Input{
		Artifacts: []*persistence.Artifact{
			{Name: "input.md", ArtifactClass: persistence.ArtifactClassInput, SizeBytes: &zero},
			{Name: "out.md", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: &nonzero},
		},
	})
	require.NoError(t, err)
	assert.Nil(t, pass)

	// Output-class empty artifact: must fail.
	fail, err := Run(context.Background(), cfg, Input{
		Artifacts: []*persistence.Artifact{
			{Name: "out.md", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: &zero},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, fail)
	assert.Contains(t, fail.Detail, "is empty")
}

// TestHasDocumentedSkipped_RejectsBlankReason — the
// skip_alternative_path escape hatch only fires when every skipped
// entry carries a non-empty reason. A model emitting `[{}]` or
// `[{"reason":""}]` must NOT bypass the URL gate.
func TestHasDocumentedSkipped_RejectsBlankReason(t *testing.T) {
	cfg := Config{
		Type: "must_contain_url",
		Params: map[string]any{
			"urls":                  []string{"mcp__broker__place_order"},
			"skip_alternative_path": "skipped",
		},
	}

	// Documented skip — non-empty reason on every entry — must pass.
	pass, err := Run(context.Background(), cfg, Input{
		ResultJSON: []byte(`{"skipped":[{"symbol":"AAPL","reason":"quote_drifted"}]}`),
	})
	require.NoError(t, err)
	assert.Nil(t, pass, "documented skipped[] with reason must bypass URL gate")

	// Blank reason — must fail (bypass denied).
	fail, err := Run(context.Background(), cfg, Input{
		ResultJSON: []byte(`{"skipped":[{"symbol":"AAPL","reason":""}]}`),
	})
	require.NoError(t, err)
	require.NotNil(t, fail, "blank reason must not bypass the URL gate")

	// One blank + one ok ⇒ still fails (every entry must qualify).
	fail2, err := Run(context.Background(), cfg, Input{
		ResultJSON: []byte(`{"skipped":[{"reason":"x"},{"reason":""}]}`),
	})
	require.NoError(t, err)
	require.NotNil(t, fail2, "any blank reason invalidates the whole skip alternative")

	// Entry that's not a map (e.g. raw string) ⇒ also disqualifies.
	fail3, err := Run(context.Background(), cfg, Input{
		ResultJSON: []byte(`{"skipped":["just-a-string"]}`),
	})
	require.NoError(t, err)
	require.NotNil(t, fail3, "non-object skipped entry must not bypass the URL gate")

	// skipped[] inside envelope.message (merge fallback).
	envelope := []byte(`{"message":"{\"skipped\":[{\"symbol\":\"AAPL\",\"reason\":\"quote_drifted\"}]}"}`)
	pass2, err := Run(context.Background(), cfg, Input{ResultJSON: envelope})
	require.NoError(t, err)
	assert.Nil(t, pass2, "envelope.message fallback must also qualify a documented skip")
}

// TestParamInt_CoercesAcrossNumericKinds pins the param parser's
// numeric-kind coercion. Operators sometimes write YAML where the
// loader yields int64 or float64 for what looks like a plain int; the
// verifier must accept all three. Other shapes must return zero so the
// caller's "min must be > 0" guard catches the typo.
func TestParamInt_CoercesAcrossNumericKinds(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"int", 5, 5},
		{"int64", int64(7), 7},
		{"float_whole", 9.0, 9},
		{"float_fractional", 1.5, 0},
		{"string_ignored", "8", 0},
		{"nil_zero", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := map[string]any{"k": c.in}
			assert.Equal(t, c.want, paramInt(p, "k"))
		})
	}
	assert.Zero(t, paramInt(nil, "k"), "nil map returns 0")
	assert.Zero(t, paramInt(map[string]any{"other": 1}, "missing"), "absent key returns 0")
}

// TestParamString_GuardsNonStringAndMissing — sister tests for the
// string helper. Non-string values fall through to "" so the verifier
// can detect "not set" without panicking on a YAML int.
func TestParamString_GuardsNonStringAndMissing(t *testing.T) {
	assert.Equal(t, "x", paramString(map[string]any{"k": "x"}, "k"))
	assert.Empty(t, paramString(map[string]any{"k": 42}, "k"), "non-string returns empty")
	assert.Empty(t, paramString(map[string]any{}, "missing"), "missing key returns empty")
	assert.Empty(t, paramString(nil, "k"), "nil map returns empty")
}

// TestParamStringSlice_AcceptsBothStringAndAnySlices — YAML decoders
// vary in whether they yield []string or []any for a list of
// strings. The helper must normalise both.
func TestParamStringSlice_AcceptsBothStringAndAnySlices(t *testing.T) {
	assert.Equal(t, []string{"a", "b"},
		paramStringSlice(map[string]any{"k": []string{"a", "b"}}, "k"))
	assert.Equal(t, []string{"a", "b"},
		paramStringSlice(map[string]any{"k": []any{"a", "b"}}, "k"))
	assert.Equal(t, []string{"a"},
		paramStringSlice(map[string]any{"k": []any{"a", 42, "  "}}, "k"),
		"non-string entries and blank-after-trim entries are dropped")
	assert.Nil(t, paramStringSlice(map[string]any{"k": "string-not-slice"}, "k"),
		"wrong type returns nil")
	assert.Nil(t, paramStringSlice(nil, "k"), "nil map returns nil")
}

// TestCountClaimedEntries_DirectEdges exercises the helper across the
// envelope/fallback/parse-error branches directly so coverage doesn't
// rely on the verifier's wrapping (which only counts under the
// claim>audit branch).
func TestCountClaimedEntries_DirectEdges(t *testing.T) {
	assert.Zero(t, countClaimedEntries(nil, "placed"))
	assert.Zero(t, countClaimedEntries([]byte(`{}`), ""))
	assert.Zero(t, countClaimedEntries([]byte(`{`), "placed"))
	assert.Equal(t, 0, countClaimedEntries([]byte(`{"placed":null}`), "placed"))
	assert.Equal(t, 2, countClaimedEntries([]byte(`{"placed":[1,2]}`), "placed"))
	// envelope.message fallback when the structured output didn't hoist.
	assert.Equal(t, 1,
		countClaimedEntries([]byte(`{"message":"{\"placed\":[{\"x\":1}]}"}`), "placed"))
	// envelope.message present but the inner key isn't an array.
	assert.Zero(t,
		countClaimedEntries([]byte(`{"message":"{\"placed\":\"oops\"}"}`), "placed"))
	// envelope.message present but unparseable.
	assert.Zero(t,
		countClaimedEntries([]byte(`{"message":"{not-json"}`), "placed"))
}

// TestEntryGateConsistent_FlagsLongOpenBelowSMA50 is the regression
// test for the 2026-06-12 NVDA whipsaw (exec_20260612191342): the
// strategist opened a long whose entry price ($204.90) sat below the
// daily SMA(50) ($206.90), asserting false inequalities in its prose
// ("price $204.80 > SMA50 $206.90") that the value-only hallucination
// detector couldn't catch. price < SMA(50) is itself the documented
// exit condition, so the very next tick closed the position at a loss.
// Every long-entry tier in the strategy requires price > SMA(50); this
// verifier re-checks that floor deterministically.
func TestEntryGateConsistent_FlagsLongOpenBelowSMA50(t *testing.T) {
	cfg := Config{Type: "entry_gate_consistent"}
	in := Input{
		EntryGateIndicators: map[string]EntryGateIndicator{
			"NVDA": {SMA50: 206.90},
		},
		ResultJSON: []byte(`{
			"proposals": [
				{"symbol": "NVDA", "action": "BUY", "intent": "open", "limit_price": 204.90}
			],
			"has_proposals": true
		}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "expected violation for long entry below SMA(50)")
	assert.Contains(t, v.Detail, "NVDA")
	assert.Contains(t, v.Detail, "SMA(50)")
	assert.Contains(t, v.Detail, "204.90")
	assert.Contains(t, v.Detail, "206.90")
}

// TestEntryGateConsistent_AcceptsLongOpenAboveSMA50 — the clean path:
// a long entry priced above the daily SMA(50) satisfies the trend floor.
func TestEntryGateConsistent_AcceptsLongOpenAboveSMA50(t *testing.T) {
	cfg := Config{Type: "entry_gate_consistent"}
	in := Input{
		EntryGateIndicators: map[string]EntryGateIndicator{
			"TSM": {SMA50: 398.25},
		},
		ResultJSON: []byte(`{
			"proposals": [
				{"symbol": "TSM", "action": "BUY", "intent": "open", "limit_price": 425.56}
			]
		}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestEntryGateConsistent_ExemptsCloseBelowSMA50 — exits are
// LEGITIMATE below SMA(50) (in fact "price < SMA(50)" is the trend-break
// exit trigger). A SELL/close must never trip the entry-floor check.
func TestEntryGateConsistent_ExemptsCloseBelowSMA50(t *testing.T) {
	cfg := Config{Type: "entry_gate_consistent"}
	in := Input{
		EntryGateIndicators: map[string]EntryGateIndicator{
			"NVDA": {SMA50: 206.91},
		},
		ResultJSON: []byte(`{
			"proposals": [
				{"symbol": "NVDA", "action": "SELL", "intent": "close", "limit_price": 204.71}
			]
		}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestEntryGateConsistent_NoOpWhenNoIndicators — without the
// deterministic indicator map (non-trading project, pre-warm failed,
// or no open proposals) the verifier is a clean no-op rather than a
// false positive.
func TestEntryGateConsistent_NoOpWhenNoIndicators(t *testing.T) {
	cfg := Config{Type: "entry_gate_consistent"}
	in := Input{
		ResultJSON: []byte(`{"proposals":[{"symbol":"NVDA","action":"BUY","intent":"open","limit_price":1}]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestEntryGateConsistent_NoOpWhenSymbolMissing — if we don't have an
// indicator row for the proposed symbol (single-symbol fetch failed)
// we can't judge the floor, so stay silent rather than block.
func TestEntryGateConsistent_NoOpWhenSymbolMissing(t *testing.T) {
	cfg := Config{Type: "entry_gate_consistent"}
	in := Input{
		EntryGateIndicators: map[string]EntryGateIndicator{
			"AAPL": {SMA50: 285.49},
		},
		ResultJSON: []byte(`{"proposals":[{"symbol":"NVDA","action":"BUY","intent":"open","limit_price":1}]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestEntryGateConsistent_SkipsWhenNoLimitPrice — a market order (no
// limit_price) carries no deterministic entry level, so the floor
// check has nothing to compare against and abstains.
func TestEntryGateConsistent_SkipsWhenNoLimitPrice(t *testing.T) {
	cfg := Config{Type: "entry_gate_consistent"}
	in := Input{
		EntryGateIndicators: map[string]EntryGateIndicator{
			"NVDA": {SMA50: 206.90},
		},
		ResultJSON: []byte(`{"proposals":[{"symbol":"NVDA","action":"BUY","intent":"open"}]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestEntryGateConsistent_HoistsFromMessageField — same envelope-merge
// fallback the watchlist verifier handles: proposals buried inside the
// `message` JSON-string are still re-checked.
func TestEntryGateConsistent_HoistsFromMessageField(t *testing.T) {
	cfg := Config{Type: "entry_gate_consistent"}
	in := Input{
		EntryGateIndicators: map[string]EntryGateIndicator{
			"NVDA": {SMA50: 206.90},
		},
		ResultJSON: []byte(`{
			"status": "COMPLETED",
			"message": "{\"proposals\": [{\"symbol\": \"NVDA\", \"action\": \"BUY\", \"intent\": \"open\", \"limit_price\": 204.90}]}"
		}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "expected violation when open proposal was buried in message")
	assert.Contains(t, v.Detail, "NVDA")
}

// TestProposedLongOpenSymbols_OnlyBuyOpens — the executor uses this to
// decide which symbols to fetch an SMA(50) for. It must return only the
// long-open legs (BUY and not intent:close), upper-cased and deduped,
// ignoring closes/sells.
func TestProposedLongOpenSymbols_OnlyBuyOpens(t *testing.T) {
	got := ProposedLongOpenSymbols([]byte(`{
		"proposals": [
			{"symbol": "nvda", "action": "BUY", "intent": "open", "limit_price": 204.90},
			{"symbol": "AAPL", "action": "SELL", "intent": "close", "limit_price": 290.0},
			{"symbol": "TSM", "action": "BUY", "intent": "open", "limit_price": 425.56},
			{"symbol": "TSM", "action": "BUY", "intent": "open", "limit_price": 426.00}
		]
	}`))
	assert.ElementsMatch(t, []string{"NVDA", "TSM"}, got)
}

// TestVerify_CVClaimsGrounded_FlagsFabricatedEmployer — B4 regression guard:
// a CV artifact that names an employer absent from the authoritative resume
// must return a violation naming the fabricated token.
func TestVerify_CVClaimsGrounded_FlagsFabricatedEmployer(t *testing.T) {
	resume := "Janka Grinco. EaseIT s.r.o. (2023-present). Scrum Alliance CSP-SM."
	cv := "Senior Delivery Lead at Globex Corp (2019-2022)." // Globex not in resume
	v := evalCVClaims(cv, resume)
	require.NotNil(t, v, "expected violation for fabricated employer")
	require.Contains(t, v.Detail, "Globex")
}

// TestVerify_CVClaimsGrounded_GroundedCVPasses — when all extracted
// employer/org tokens appear in the resume the check must return nil.
func TestVerify_CVClaimsGrounded_GroundedCVPasses(t *testing.T) {
	resume := "Janka Grinco. EaseIT s.r.o. (2023-present). Scrum Alliance CSP-SM."
	cv := "CV for Janka Grinco. Employment: EaseIT s.r.o. (2023-present). Scrum Alliance."
	v := evalCVClaims(cv, resume)
	require.Nil(t, v, "grounded CV must not produce a violation")
}

// TestVerify_CVClaimsGrounded_RunDispatch verifies the full Run path
// (case "cv_claims_grounded") fires when an artifact body contains
// ungrounded hard facts. The check is warn-tier by default.
func TestVerify_CVClaimsGrounded_RunDispatch(t *testing.T) {
	resume := "Janka Grinco. EaseIT s.r.o. (2023-present). Scrum Alliance CSP-SM."
	cv := "Senior Delivery Lead at Globex Corp (2019-2022)."

	withBodyReader(t, func(a *persistence.Artifact) ([]byte, error) {
		return []byte(cv), nil
	})
	sz := int64(len(cv))
	cfg := Config{
		Type:     "cv_claims_grounded",
		Severity: "warn",
		Params: map[string]any{
			"resume": resume,
		},
	}
	in := Input{
		Artifacts: []*persistence.Artifact{
			{Name: "cv-tailored.md", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: &sz},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Equal(t, SeverityWarn, v.Severity)
	assert.Contains(t, v.Detail, "Globex")
}
