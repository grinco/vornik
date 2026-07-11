package projectwizard

import "testing"

func testPriors() []TemplatePrior {
	return []TemplatePrior{
		{Slug: "ai-news-digest", DisplayName: "AI News Digest", Description: "Daily AI news digest emailed to you", Domain: "research"},
		{Slug: "trading-bot", DisplayName: "Trading Bot", Description: "Automated equities trading with risk caps", Domain: "trading"},
	}
}

func TestAnchorScore_StrongMatch(t *testing.T) {
	slug, score := anchorScore("I want a daily AI news digest sent to me", testPriors())
	if slug != "ai-news-digest" {
		t.Fatalf("slug = %q, want ai-news-digest", slug)
	}
	if score < anchorConfidenceThreshold {
		t.Errorf("score = %.2f, want >= threshold %.2f for a strong match", score, anchorConfidenceThreshold)
	}
}

func TestAnchorScore_WeakMatch(t *testing.T) {
	slug, score := anchorScore("build me something to track my houseplants", testPriors())
	if score >= anchorConfidenceThreshold {
		t.Errorf("score = %.2f for slug %q, want below threshold %.2f for an unrelated ask", score, slug, anchorConfidenceThreshold)
	}
}

func TestAnchorScore_NoPriors(t *testing.T) {
	slug, score := anchorScore("anything", nil)
	if slug != "" || score != 0 {
		t.Errorf("expected zero-value result with no priors, got slug=%q score=%.2f", slug, score)
	}
}

func TestAnchorScore_EmptyDescription(t *testing.T) {
	slug, score := anchorScore("   ", testPriors())
	if slug != "" || score != 0 {
		t.Errorf("expected zero-value result for empty description, got slug=%q score=%.2f", slug, score)
	}
}

func TestTokenize_DropsShortTokens(t *testing.T) {
	toks := tokenize("a to an AI-news Digest, daily!")
	if toks["a"] || toks["to"] || toks["an"] {
		t.Errorf("short tokens should be dropped, got %v", toks)
	}
	if !toks["news"] || !toks["digest"] || !toks["daily"] {
		t.Errorf("expected meaningful tokens present, got %v", toks)
	}
	if toks["ai"] {
		t.Errorf("2-char token 'ai' should be dropped by the <3-char noise filter, got %v", toks)
	}
}

func TestTierLabel(t *testing.T) {
	cases := []struct {
		name string
		env  *Envelope
		want string
	}{
		{"nil", nil, "n/a"},
		{"explicit tier 3", &Envelope{Tier: 3}, "3"},
		{"explicit tier 1", &Envelope{Tier: 1}, "1"},
		{"legacy composition", &Envelope{Composition: &Composition{}}, "2"},
		{"legacy proposal only", &Envelope{}, "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tierLabel(c.env); got != c.want {
				t.Errorf("tierLabel = %q, want %q", got, c.want)
			}
		})
	}
}

func TestAffirmativeReply(t *testing.T) {
	yes := []string{"yes", "Yes!", "yep", "go ahead", "Build it from scratch.", "confirm"}
	for _, m := range yes {
		if !affirmativeReply(m) {
			t.Errorf("expected %q to be affirmative", m)
		}
	}
	no := []string{"maybe", "yes but change the schedule", "no", "what do you mean", ""}
	for _, m := range no {
		if affirmativeReply(m) {
			t.Errorf("expected %q to NOT be treated as an explicit affirmative", m)
		}
	}
}

func TestDetectTier3Unlock(t *testing.T) {
	transcript := []Turn{
		{Role: "user", Content: "build me something fully custom"},
		{Role: "assistant", Content: "That looks like a fully custom automation — reply to confirm a from-scratch automation, or I can use the closest template instead."},
	}
	if !detectTier3Unlock(transcript, "yes, build it from scratch") {
		t.Error("expected unlock on explicit affirmative following the confirm-sentinel turn")
	}
	if detectTier3Unlock(transcript, "no, use the template") {
		t.Error("non-affirmative reply must not unlock")
	}
	noSentinel := []Turn{
		{Role: "assistant", Content: "Here's a draft."},
	}
	if detectTier3Unlock(noSentinel, "yes") {
		t.Error("affirmative without a preceding confirm-sentinel turn must not unlock")
	}
	if detectTier3Unlock(nil, "yes") {
		t.Error("empty transcript must not unlock")
	}
}
