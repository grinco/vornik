package fixitdoctor

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"vornik.io/vornik/internal/version"
)

func TestValidateActions_DropsUnknownKind(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	actions := []ProposedAction{
		{Kind: "shell_exec", Label: "run a shell command", Params: map[string]string{"cmd": "rm -rf /"}},
	}
	out := ValidateActions(actions, version.EditionCommunity, m)
	if len(out) != 0 {
		t.Fatalf("expected unknown kind dropped, got %+v", out)
	}
	if got := testutilCounterTotal(t, m.GuardrailHitsTotal, GuardrailReasonUnknownKind); got != 1 {
		t.Fatalf("expected 1 unknown_kind guardrail hit, got %v", got)
	}
}

func TestValidateActions_DropsEditionGatedKindEvenIfEmittedAsFreeText(t *testing.T) {
	// A Community deployment's schema never offers config_apply, but a
	// compromised/hallucinating model could still emit it via the
	// prose fallback — the server must re-validate against
	// AllowedActionKinds regardless of what schema was on offer.
	m := NewMetrics(prometheus.NewRegistry())
	actions := []ProposedAction{
		{Kind: ActionKindConfigApply, Label: "apply config", Params: map[string]string{"key": "x", "value": "y"}},
	}
	out := ValidateActions(actions, version.EditionCommunity, m)
	if len(out) != 0 {
		t.Fatalf("expected config_apply dropped on community edition, got %+v", out)
	}
	if got := testutilCounterTotal(t, m.GuardrailHitsTotal, GuardrailReasonUnknownKind); got != 1 {
		t.Fatalf("expected 1 unknown_kind guardrail hit, got %v", got)
	}
}

func TestValidateActions_AllowsConfigApplyOnEnterprise(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	actions := []ProposedAction{
		{Kind: ActionKindConfigApply, Label: "apply config", Params: map[string]string{"key": "x", "value": "y"}},
	}
	out := ValidateActions(actions, version.EditionEnterprise, m)
	if len(out) != 1 {
		t.Fatalf("expected config_apply allowed on enterprise, got %+v", out)
	}
}

func TestValidateActions_DropsParamInvalid(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	actions := []ProposedAction{
		{Kind: ActionKindRetryTask, Label: "retry", Params: map[string]string{}}, // missing task_id
		{Kind: ActionKindLinkOut, Label: "go look", Params: map[string]string{"url": "https://evil.example.com/steal"}},
		{Kind: ActionKindLinkOut, Label: "go look", Params: map[string]string{"url": "javascript:alert(1)"}},
		{Kind: ActionKindLinkOut, Label: "go look", Params: map[string]string{"url": "//evil.example.com"}},
	}
	out := ValidateActions(actions, version.EditionCommunity, m)
	if len(out) != 0 {
		t.Fatalf("expected all param-invalid actions dropped, got %+v", out)
	}
	if got := testutilCounterTotal(t, m.GuardrailHitsTotal, GuardrailReasonParamsInvalid); got != 4 {
		t.Fatalf("expected 4 params_invalid guardrail hits, got %v", got)
	}
}

func TestValidateActions_MessageOnlyWhenAllDropped(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	env := &FixItEnvelope{
		Message: "here's a plan",
		Actions: []ProposedAction{{Kind: "not_a_real_kind"}},
	}
	env.Actions = ValidateActions(env.Actions, version.EditionCommunity, m)
	if len(env.Actions) != 0 {
		t.Fatalf("expected no surviving actions, got %+v", env.Actions)
	}
	if env.Message == "" {
		t.Fatalf("message must survive even when every action is dropped")
	}
}

func TestValidateActions_KeepsValidActions(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	actions := []ProposedAction{
		{Kind: ActionKindRetryTask, Label: "retry the task", Params: map[string]string{"task_id": "task-1"}},
		{Kind: ActionKindLinkOut, Label: "open integration settings", Params: map[string]string{"url": "/ui/integrations/github"}},
	}
	out := ValidateActions(actions, version.EditionCommunity, m)
	if len(out) != 2 {
		t.Fatalf("expected both valid actions kept, got %+v", out)
	}
	if got := testutilCounterTotal(t, m.GuardrailHitsTotal, GuardrailReasonUnknownKind); got != 0 {
		t.Fatalf("expected 0 guardrail hits for valid actions, got %v", got)
	}
}

func TestValidActionParams_AllKinds(t *testing.T) {
	cases := []struct {
		kind   ActionKind
		params map[string]string
		want   bool
	}{
		{ActionKindConfigApplyGate, map[string]string{"key": "instinct.enabled"}, true},
		{ActionKindConfigApplyGate, map[string]string{}, false},
		{ActionKindConfigApply, map[string]string{"key": "x", "value": "y"}, true},
		{ActionKindConfigApply, map[string]string{"key": "x"}, false},
		{ActionKindSetSecret, map[string]string{"key": "telegram.bot_token"}, true},
		{ActionKindSetSecret, map[string]string{}, false},
		{ActionKindReprobeIntegration, map[string]string{"integration_id": "github"}, true},
		{ActionKindReprobeIntegration, map[string]string{}, false},
		{ActionKind("bogus"), map[string]string{}, false},
	}
	for _, c := range cases {
		if got := validActionParams(c.kind, c.params); got != c.want {
			t.Errorf("validActionParams(%q, %v) = %v, want %v", c.kind, c.params, got, c.want)
		}
	}
}

func TestValidLinkOutURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"/ui/integrations/github", true},
		{"/ui/tasks/abc-123", true},
		{"", false},
		{"http://example.com", false},
		{"https://example.com/x", false},
		{"javascript:alert(1)", false},
		{"//example.com", false},
		{"data:text/html,hi", false},
		{"/ui/x y", false},
		{"relative/path", false},
	}
	for _, c := range cases {
		if got := validLinkOutURL(c.url); got != c.want {
			t.Errorf("validLinkOutURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// testutilCounterTotal reads a single label's counter value via a
// fresh GetMetricWithLabelValues lookup — every test here constructs a
// fresh registry per Metrics instance, so there's exactly one relevant
// series per reason.
func testutilCounterTotal(t *testing.T, cv *prometheus.CounterVec, labelValue string) float64 {
	t.Helper()
	c, err := cv.GetMetricWithLabelValues(labelValue)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	return testutil.ToFloat64(c)
}
