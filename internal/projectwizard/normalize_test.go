package projectwizard

import (
	"encoding/json"
	"strings"
	"testing"
)

func addon(t *testing.T, obj map[string]any) Addon {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal addon: %v", err)
	}
	var a Addon
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatalf("unmarshal addon: %v", err)
	}
	return a
}

func autonomyOf(t *testing.T, addons []Addon) autonomyArgs {
	t.Helper()
	var count int
	var got autonomyArgs
	for _, a := range addons {
		if a.Type == "autonomy" {
			count++
			if err := json.Unmarshal(a.Args, &got); err != nil {
				t.Fatalf("decode autonomy: %v", err)
			}
		}
	}
	if count != 1 {
		t.Fatalf("want exactly 1 autonomy addon, got %d in %d addons", count, len(addons))
	}
	return got
}

func TestMergeAutonomy_ScheduleAndRagSource(t *testing.T) {
	in := []Addon{
		addon(t, map[string]any{"type": "schedule", "interval": "24h", "goal": "produce the daily digest"}),
		addon(t, map[string]any{"type": "rag_source", "source": "https://mail.example/export", "cadence": "5m"}),
	}
	out, notes, cerr := mergeAutonomyAddons(in, BaseAutonomy{})
	if cerr != nil {
		t.Fatalf("unexpected error: %v", cerr)
	}
	if len(notes) == 0 {
		t.Fatal("merge must surface a RepairNote")
	}
	a := autonomyOf(t, out)
	if a.Mode != "llm" {
		t.Errorf("mode = %q, want llm", a.Mode)
	}
	if a.PollInterval != "5m" {
		t.Errorf("poll_interval = %q, want finest 5m", a.PollInterval)
	}
	for _, want := range []string{"https://mail.example/export", "produce the daily digest", "24h"} {
		if !strings.Contains(a.Goal, want) {
			t.Errorf("goal missing %q:\n%s", want, a.Goal)
		}
	}
	if !strings.Contains(strings.ToLower(a.Goal), "governed by this loop") {
		t.Errorf("goal missing loop-governs-cadence wrapper:\n%s", a.Goal)
	}
}

func TestMergeAutonomy_MultipleRagSourceMerged(t *testing.T) {
	in := []Addon{
		addon(t, map[string]any{"type": "rag_source", "source": "https://a.example", "cadence": "10m"}),
		addon(t, map[string]any{"type": "rag_source", "source": "https://b.example", "cadence": "5m"}),
	}
	out, notes, cerr := mergeAutonomyAddons(in, BaseAutonomy{})
	if cerr != nil {
		t.Fatalf("unexpected error: %v", cerr)
	}
	a := autonomyOf(t, out)
	if a.PollInterval != "5m" {
		t.Errorf("poll_interval = %q, want finest 5m", a.PollInterval)
	}
	for _, want := range []string{"https://a.example", "https://b.example"} {
		if !strings.Contains(a.Goal, want) {
			t.Errorf("goal missing source %q", want)
		}
	}
	// differing cadences collapsed to finest -> must be surfaced (F3)
	var surfaced bool
	for _, n := range notes {
		if strings.Contains(strings.ToLower(string(n)), "caden") {
			surfaced = true
		}
	}
	if !surfaced {
		t.Errorf("differing cadences collapsed silently; want a RepairNote: %v", notes)
	}
}

func TestMergeAutonomy_NoPositiveCadenceRejected(t *testing.T) {
	in := []Addon{
		addon(t, map[string]any{"type": "schedule", "interval": "24h", "goal": "digest"}),
		addon(t, map[string]any{"type": "rag_source", "source": "https://a.example", "cadence": "0s"}),
	}
	_, _, cerr := mergeAutonomyAddons(in, BaseAutonomy{})
	if cerr == nil {
		t.Fatal("want reject when no ingest cadence is positive (no empty poll_interval)")
	}
}

func TestMergeAutonomy_NoopBelowThreshold(t *testing.T) {
	in := []Addon{
		addon(t, map[string]any{"type": "rag_source", "source": "https://a.example", "cadence": "5m"}),
		addon(t, map[string]any{"type": "mcp_server", "name": "scraper"}),
	}
	out, notes, cerr := mergeAutonomyAddons(in, BaseAutonomy{})
	if cerr != nil {
		t.Fatalf("unexpected error: %v", cerr)
	}
	if len(notes) != 0 {
		t.Errorf("no-op must emit no notes, got %v", notes)
	}
	if len(out) != len(in) {
		t.Errorf("no-op must not change addon count: %d -> %d", len(in), len(out))
	}
	for _, a := range out {
		if a.Type == "autonomy" {
			t.Error("no-op must not synthesize an autonomy addon")
		}
	}
}

func TestMergeAutonomy_TurnOverTurnStable(t *testing.T) {
	raw := []Addon{
		addon(t, map[string]any{"type": "schedule", "interval": "24h", "goal": "produce the daily digest"}),
		addon(t, map[string]any{"type": "rag_source", "source": "https://a.example", "cadence": "5m"}),
	}
	first, _, cerr := mergeAutonomyAddons(raw, BaseAutonomy{})
	if cerr != nil {
		t.Fatalf("first merge: %v", cerr)
	}
	// Simulate the LLM re-proposing the SAME raw shape next turn; the
	// merge must produce byte-identical output (synthesize from
	// primitives, never re-parse a synthesized goal).
	second, _, cerr := mergeAutonomyAddons(raw, BaseAutonomy{})
	if cerr != nil {
		t.Fatalf("second merge: %v", cerr)
	}
	if autonomyOf(t, first).Goal != autonomyOf(t, second).Goal {
		t.Error("merge not deterministic across turns")
	}
	// And re-merging the ALREADY-merged output is a no-op (1 autonomy addon).
	third, notes, cerr := mergeAutonomyAddons(first, BaseAutonomy{})
	if cerr != nil {
		t.Fatalf("re-merge: %v", cerr)
	}
	if len(notes) != 0 || len(third) != len(first) {
		t.Errorf("re-merging normalized output must be a no-op; notes=%v", notes)
	}
}

func TestMergeAutonomy_CronBaseRejected(t *testing.T) {
	in := []Addon{
		addon(t, map[string]any{"type": "schedule", "interval": "24h", "goal": "digest"}),
		addon(t, map[string]any{"type": "rag_source", "source": "https://a.example", "cadence": "5m"}),
	}
	_, _, cerr := mergeAutonomyAddons(in, BaseAutonomy{Enabled: true, Mode: "cron"})
	if cerr == nil {
		t.Fatal("want reject: llm multi-cadence merge onto a cron-enabled base")
	}
}

func pv(vals ...string) ParamValue { return ParamValue(vals) }

func specDigest() []TemplateParam {
	return []TemplateParam{
		{Name: "projectId", Required: true},
		{Name: "displayName", Required: true},
		{Name: "llmModel", Required: true, Default: "zai.glm-5"},
	}
}

func TestRepairParams_DropsUndeclared(t *testing.T) {
	params := map[string]ParamValue{
		"displayName": pv("Daily Digest"),
		"llmModel":    pv("zai.glm-5"),
		"projectId":   pv("daily-digest"),
		"topic":       pv("email + slack"),
	}
	notes, q := repairTemplateParams(params, specDigest(), "chat-default")
	if q != "" {
		t.Fatalf("unexpected question: %q", q)
	}
	if _, ok := params["topic"]; ok {
		t.Error("undeclared 'topic' must be dropped")
	}
	var noted bool
	for _, n := range notes {
		if strings.Contains(string(n), "topic") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("dropping topic must be surfaced: %v", notes)
	}
}

func TestRepairParams_DerivesProjectId(t *testing.T) {
	params := map[string]ParamValue{
		"displayName": pv("Daily Digest Assistant"),
		"llmModel":    pv("zai.glm-5"),
	}
	if _, q := repairTemplateParams(params, specDigest(), "chat-default"); q != "" {
		t.Fatalf("unexpected question: %q", q)
	}
	if got := params["projectId"]; len(got) != 1 || got[0] != "daily-digest-assistant" {
		t.Errorf("projectId = %v, want [daily-digest-assistant]", got)
	}
}

func TestRepairParams_DefaultsLLMModel(t *testing.T) {
	// missing llmModel with a declared default -> use the default
	params := map[string]ParamValue{"displayName": pv("D"), "projectId": pv("d")}
	repairTemplateParams(params, specDigest(), "chat-default")
	if got := params["llmModel"]; len(got) != 1 || got[0] != "zai.glm-5" {
		t.Errorf("llmModel = %v, want template default [zai.glm-5]", got)
	}
	// no declared default -> fall back to the daemon model
	spec := []TemplateParam{{Name: "llmModel", Required: true}}
	params2 := map[string]ParamValue{}
	repairTemplateParams(params2, spec, "bedrock-fallback")
	if got := params2["llmModel"]; len(got) != 1 || got[0] != "bedrock-fallback" {
		t.Errorf("llmModel = %v, want fallback [bedrock-fallback]", got)
	}
}

func TestRepairParams_UnderspecifiedRequiredAsksQuestion(t *testing.T) {
	spec := []TemplateParam{{Name: "region", Required: true}} // no default, no derivation
	params := map[string]ParamValue{}
	_, q := repairTemplateParams(params, spec, "chat-default")
	if q == "" {
		t.Fatal("a required param with no default/derivation must surface a question, not a hard stop")
	}
	if strings.Contains(strings.ToLower(q), "region") == false {
		t.Errorf("question should name the missing param: %q", q)
	}
}

func TestRepairParams_EmptySpecDerivesProjectIdOnly(t *testing.T) {
	params := map[string]ParamValue{"displayName": pv("My Thing"), "extra": pv("x")}
	repairTemplateParams(params, nil, "chat-default")
	if got := params["projectId"]; len(got) != 1 || got[0] != "my-thing" {
		t.Errorf("projectId = %v, want derived [my-thing]", got)
	}
}

// metaLookup is a test TemplateMetaLookup: personal-assistant-shaped
// declared params + a disabled (mode:llm) base.
func metaLookup(_ string) ([]TemplateParam, BaseAutonomy, bool) {
	return []TemplateParam{
		{Name: "projectId", Required: true},
		{Name: "displayName", Required: true},
		{Name: "llmModel", Required: true},
	}, BaseAutonomy{Enabled: false, Mode: "llm"}, true
}

// TestWizardConvergence_20260716Incident feeds the exact failing shape
// from the incident (schedule daily-digest + rag_source 5m ingest, an
// undeclared `topic` param, and no projectId) through normalization +
// Compose and asserts it now converges to one llm-mode autonomy block.
func TestWizardConvergence_20260716Incident(t *testing.T) {
	c := &Composition{
		Template: "personal-assistant",
		Params: map[string]ParamValue{
			"displayName": pv("Daily Digest Assistant"),
			"llmModel":    pv("zai.glm-5"),
			"topic":       pv("email + slack + calendar"),
		},
		Addons: []Addon{
			addon(t, map[string]any{"type": "schedule", "interval": "24h", "goal": "produce the daily digest, prioritise, track commitments"}),
			addon(t, map[string]any{"type": "rag_source", "source": "imap://mail", "cadence": "5m"}),
		},
	}
	notes, question, nerr := normalizeComposition(c, metaLookup, "zai.glm-5")
	if nerr != nil {
		t.Fatalf("normalize should converge, got %v", nerr)
	}
	if question != "" {
		t.Fatalf("all required params derivable; unexpected question %q", question)
	}
	if len(notes) == 0 {
		t.Fatal("expected repair notes")
	}
	// topic dropped, projectId derived.
	if _, ok := c.Params["topic"]; ok {
		t.Error("undeclared topic must be dropped")
	}
	if got := c.Params["projectId"]; len(got) != 1 || got[0] != "daily-digest-assistant" {
		t.Errorf("projectId = %v, want derived", got)
	}
	// exactly one autonomy addon, llm mode.
	a := autonomyOf(t, c.Addons)
	if a.Mode != "llm" || a.PollInterval != "5m" {
		t.Errorf("autonomy = %+v, want llm/5m", a)
	}

	// And Compose now succeeds, writing a single llm autonomy block.
	files, _, err := Compose(ComposeInput{
		TemplateSlug: c.Template,
		Params:       c.ParamsMulti(),
		Addons:       c.Addons,
	}, ComposeDeps{Templates: fakeMat{files: baseFiles()}})
	if err != nil {
		t.Fatalf("Compose after normalize should succeed, got %v", err)
	}
	proj := files["projects/pricing-watch.yaml"]
	if !strings.Contains(proj, "mode: llm") {
		t.Errorf("composed project missing llm autonomy block:\n%s", proj)
	}
}

// TestNormalizeComposition_NilMetaPreservesParams: when no TemplateMeta is
// wired (spec unavailable), param repair must be SKIPPED — params are left
// untouched rather than dropped as "undeclared" (§4.2). Regression for the
// wizard-adapter converse test that a composition's params survive when the
// wizard runs without a template catalog.
func TestNormalizeComposition_NilMetaPreservesParams(t *testing.T) {
	c := &Composition{
		Template: "python-scraper",
		Params:   map[string]ParamValue{"schedule": pv("daily")},
		Addons: []Addon{
			addon(t, map[string]any{"type": "schedule", "interval": "24h", "goal": "scrape"}),
		},
	}
	notes, question, cerr := normalizeComposition(c, nil, "")
	if cerr != nil {
		t.Fatalf("unexpected error: %v", cerr)
	}
	if question != "" {
		t.Errorf("unexpected question: %q", question)
	}
	if len(notes) != 0 {
		t.Errorf("no repairs when spec unavailable, got %v", notes)
	}
	if got := c.Params["schedule"]; len(got) != 1 || got[0] != "daily" {
		t.Errorf("params must survive when spec unknown: schedule=%v", got)
	}
	if len(c.Addons) != 1 {
		t.Errorf("single autonomy addon is a merge no-op, got %d addons", len(c.Addons))
	}
}
