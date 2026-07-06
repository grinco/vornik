package projectwizard

import (
	"encoding/json"
	"errors"
	"testing"

	"vornik.io/vornik/internal/templates"
)

func TestAddon_UnmarshalCapturesTypeAndArgs(t *testing.T) {
	var a Addon
	raw := `{"type":"mcp_server","name":"slack","allowed_tools":["send_message"]}`
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Type != "mcp_server" {
		t.Fatalf("type = %q", a.Type)
	}
	// Args must be the full object so the applier can decode name/allowed_tools.
	var probe struct {
		Name         string   `json:"name"`
		AllowedTools []string `json:"allowed_tools"`
	}
	if err := json.Unmarshal(a.Args, &probe); err != nil {
		t.Fatalf("args re-decode: %v", err)
	}
	if probe.Name != "slack" || len(probe.AllowedTools) != 1 {
		t.Fatalf("args = %+v", probe)
	}
}

// TestAddon_MarshalJSON_RoundTripsThroughComposition regression-tests
// the bug found while wiring Commit (T6) to decode a persisted
// session.Composition: re-marshaling a Composition whose Addons had
// already been unmarshaled once (Converse's persistence step) used
// to nest the addon args a level too deep — Addon had no MarshalJSON,
// so the default struct marshaler emitted {"Type":...,"Args":{...}}
// instead of the flat {"type":...,"interval":...} shape appliers
// expect. Commit's decode-then-Compose then fed scheduleApplier a
// mis-shapen args blob, and it failed on a missing "interval" that
// was actually there, just nested wrong.
func TestAddon_MarshalJSON_RoundTripsThroughComposition(t *testing.T) {
	var comp Composition
	raw := `{"template":"custom-base","params":{"projectId":"pricing-watch"},"addons":[{"type":"schedule","interval":"168h","goal":"weekly digest","task_type":"report"}]}`
	if err := json.Unmarshal([]byte(raw), &comp); err != nil {
		t.Fatalf("unmarshal composition: %v", err)
	}

	// Simulate Converse's persistence step: marshal the already-decoded
	// composition back to bytes for storage on session.Composition.
	persisted, err := json.Marshal(&comp)
	if err != nil {
		t.Fatalf("marshal composition: %v", err)
	}

	// Simulate Commit's decode of the persisted bytes.
	var reloaded Composition
	if err := json.Unmarshal(persisted, &reloaded); err != nil {
		t.Fatalf("unmarshal persisted composition: %v", err)
	}
	if len(reloaded.Addons) != 1 {
		t.Fatalf("expected 1 addon after round-trip, got %d: %s", len(reloaded.Addons), persisted)
	}
	var args struct {
		Interval string `json:"interval"`
		Goal     string `json:"goal"`
	}
	if err := json.Unmarshal(reloaded.Addons[0].Args, &args); err != nil {
		t.Fatalf("decode round-tripped addon args: %v", err)
	}
	if args.Interval != "168h" || args.Goal != "weekly digest" {
		t.Fatalf("addon args lost fields across round-trip: %+v (raw persisted: %s)", args, persisted)
	}
}

func TestNewApplierRegistry_HasAllSixTypes(t *testing.T) {
	reg := newApplierRegistry(ComposeDeps{KnownMCP: map[string]bool{}})
	for _, want := range []string{
		"mcp_server", "schedule", "rag_source",
		"chat_tools", "role_prompt_append", "secret_requirement",
	} {
		if _, ok := reg[want]; !ok {
			t.Errorf("registry missing applier %q", want)
		}
	}
}

func TestComposeError_Message(t *testing.T) {
	e := &ComposeError{AddonIndex: 2, AddonType: "schedule", Field: "interval", Message: "not a duration"}
	got := e.Error()
	for _, sub := range []string{"schedule", "interval", "not a duration"} {
		if !contains(got, sub) {
			t.Fatalf("error %q missing %q", got, sub)
		}
	}
}

func contains(hay, needle string) bool {
	return len(needle) == 0 || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0)
}
func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// fakeMat returns fixed template files regardless of params (params
// validation is templates.MaterialiseFilesMulti's job, tested there).
type fakeMat struct {
	files map[string]string
	err   error
}

func (f fakeMat) MaterialiseMulti(_ string, _ map[string][]string, _ templates.OptionsResolver) (map[string]string, error) {
	return f.files, f.err
}

const baseProjectYAML = `projectId: "pricing-watch"
displayName: "Pricing Watch"
swarmId: "pricing-watch-swarm"
defaultWorkflowId: "adaptive"
permissions:
  secrets: []
  allowedTools: ["file_read"]
`

const baseSwarmMD = `---
swarmId: "pricing-watch-swarm"
displayName: "Pricing Watch swarm"
roles:
  - name: "lead"
    description: "Leads."
    runtime:
      image: "vornik/agent:latest"
---

# Pricing Watch swarm

## Role prompts

### lead

You are the lead.
`

func baseFiles() map[string]string {
	return map[string]string{
		"projects/pricing-watch.yaml":   baseProjectYAML,
		"swarms/pricing-watch-swarm.md": baseSwarmMD,
	}
}

func TestCompose_AppliesAddonsInOrderAndValidates(t *testing.T) {
	deps := ComposeDeps{
		Templates: fakeMat{files: baseFiles()},
		KnownMCP:  map[string]bool{"slack": true},
	}
	in := ComposeInput{
		TemplateSlug: "custom-base",
		Params:       map[string][]string{"projectId": {"pricing-watch"}},
		Addons: []Addon{
			mustAddon(`{"type":"mcp_server","name":"slack","allowed_tools":["send_message"]}`),
			mustAddon(`{"type":"schedule","interval":"168h","goal":"weekly pricing digest","task_type":"report"}`),
			mustAddon(`{"type":"secret_requirement","name":"SLACK_TOKEN","label":"Slack token"}`),
			mustAddon(`{"type":"role_prompt_append","role":"lead","text":"Track competitor pricing."}`),
		},
	}
	files, secrets, err := Compose(in, deps)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	proj := files["projects/pricing-watch.yaml"]
	if !containsStr(proj, "slack") || !containsStr(proj, "168h") || !containsStr(proj, "SLACK_TOKEN") {
		t.Fatalf("composed project missing addon mutations:\n%s", proj)
	}
	sw := files["swarms/pricing-watch-swarm.md"]
	if !containsStr(sw, "Track competitor pricing.") {
		t.Fatalf("composed swarm missing role prompt:\n%s", sw)
	}
	if len(secrets) != 1 || secrets[0].Name != "SLACK_TOKEN" {
		t.Fatalf("declared secrets: %+v", secrets)
	}
}

func TestCompose_ApplierErrorNamesIndex(t *testing.T) {
	deps := ComposeDeps{Templates: fakeMat{files: baseFiles()}, KnownMCP: map[string]bool{}}
	in := ComposeInput{
		TemplateSlug: "custom-base",
		Addons:       []Addon{mustAddon(`{"type":"mcp_server","name":"nonexistent"}`)},
	}
	_, _, err := Compose(in, deps)
	if err == nil {
		t.Fatal("expected error for unknown MCP server")
	}
	var ce *ComposeError
	if !asComposeErr(err, &ce) || ce.AddonIndex != 0 || ce.AddonType != "mcp_server" {
		t.Fatalf("want ComposeError at addon[0] mcp_server, got %v", err)
	}
}

// TestCompose_ApplierErrorNamesNonZeroIndex guards the ce.AddonIndex = i
// stamping specifically: the failing addon is at index 1, which differs
// from ComposeError's zero-value AddonIndex, so the test fails if the
// stamping line is dropped. Phase 3b feeds this index back to the LLM to
// name which addon to fix, so index-stamping regressions must be caught.
func TestCompose_ApplierErrorNamesNonZeroIndex(t *testing.T) {
	deps := ComposeDeps{Templates: fakeMat{files: baseFiles()}, KnownMCP: map[string]bool{}}
	in := ComposeInput{
		TemplateSlug: "custom-base",
		Addons: []Addon{
			// [0] succeeds
			mustAddon(`{"type":"schedule","interval":"168h","goal":"weekly digest","task_type":"report"}`),
			// [1] fails: unknown MCP server (KnownMCP empty)
			mustAddon(`{"type":"mcp_server","name":"nonexistent"}`),
		},
	}
	_, _, err := Compose(in, deps)
	if err == nil {
		t.Fatal("expected error for unknown MCP server at addon[1]")
	}
	var ce *ComposeError
	if !asComposeErr(err, &ce) || ce.AddonIndex != 1 || ce.AddonType != "mcp_server" {
		t.Fatalf("want ComposeError at addon[1] mcp_server, got %+v (%v)", ce, err)
	}
}

// TestCompose_ProjectOnlyNoSwarm covers a template that emits only a
// project YAML (no swarms/*.md). Compose must succeed, apply the addon,
// return the mutated project, and not error on the absent swarm.
func TestCompose_ProjectOnlyNoSwarm(t *testing.T) {
	deps := ComposeDeps{
		Templates: fakeMat{files: map[string]string{
			"projects/pricing-watch.yaml": baseProjectYAML,
		}},
		KnownMCP: map[string]bool{},
	}
	in := ComposeInput{
		TemplateSlug: "custom-base",
		Addons: []Addon{
			mustAddon(`{"type":"secret_requirement","name":"API_TOKEN","label":"API token"}`),
		},
	}
	files, secrets, err := Compose(in, deps)
	if err != nil {
		t.Fatalf("compose (project-only): %v", err)
	}
	proj, ok := files["projects/pricing-watch.yaml"]
	if !ok {
		t.Fatal("mutated project file missing from result")
	}
	if !containsStr(proj, "API_TOKEN") {
		t.Fatalf("composed project missing secret mutation:\n%s", proj)
	}
	if _, hasSwarm := files["swarms/pricing-watch-swarm.md"]; hasSwarm {
		t.Fatal("no swarm should have been produced")
	}
	if len(secrets) != 1 || secrets[0].Name != "API_TOKEN" {
		t.Fatalf("declared secrets: %+v", secrets)
	}
}

func TestCompose_MaterialiseFailurePropagates(t *testing.T) {
	deps := ComposeDeps{Templates: fakeMat{err: errString("boom")}}
	_, _, err := Compose(ComposeInput{TemplateSlug: "x"}, deps)
	if err == nil {
		t.Fatal("materialise failure must propagate")
	}
}

func TestCompose_UnknownAddonType(t *testing.T) {
	deps := ComposeDeps{Templates: fakeMat{files: baseFiles()}, KnownMCP: map[string]bool{}}
	in := ComposeInput{TemplateSlug: "custom-base", Addons: []Addon{mustAddon(`{"type":"teleport"}`)}}
	_, _, err := Compose(in, deps)
	if err == nil {
		t.Fatal("unknown addon type must error")
	}
}

// TestCompose_RejectsConflictingAutonomyAddons is the cross-task
// regression guard for the last-wins autonomy footgun: two
// autonomy-writing addons in DIFFERENT modes (schedule=cron then
// rag_source=llm, or two exclusive cron schedules) must not both mutate
// registry.Project.Autonomy. Compose.Validate does not catch this
// (Autonomy.Mode coherence is only checked by the daemon's
// registry.Load), so the second applier's mode-aware guard must fail
// composition instead of silently mangling the first addon's block.
func TestCompose_RejectsConflictingAutonomyAddons(t *testing.T) {
	cases := []struct {
		name      string
		addons    []Addon
		wantType  string
		wantIndex int
	}{
		{
			// schedule sets cron; rag_source (llm) then hits a mode conflict.
			name: "schedule then rag_source",
			addons: []Addon{
				mustAddon(`{"type":"schedule","interval":"168h","goal":"weekly digest","task_type":"report"}`),
				mustAddon(`{"type":"rag_source","source":"https://docs.example.com","cadence":"24h"}`),
			},
			wantType:  "rag_source",
			wantIndex: 1,
		},
		{
			// cron is exclusive: a second schedule on any existing autonomy conflicts.
			name: "schedule then schedule",
			addons: []Addon{
				mustAddon(`{"type":"schedule","interval":"168h","goal":"weekly digest","task_type":"report"}`),
				mustAddon(`{"type":"schedule","interval":"24h","goal":"daily check","task_type":"task"}`),
			},
			wantType:  "schedule",
			wantIndex: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := ComposeDeps{Templates: fakeMat{files: baseFiles()}, KnownMCP: map[string]bool{}}
			in := ComposeInput{TemplateSlug: "custom-base", Addons: tc.addons}
			_, _, err := Compose(in, deps)
			if err == nil {
				t.Fatal("conflicting autonomy addons must error")
			}
			var ce *ComposeError
			if !asComposeErr(err, &ce) || ce.AddonIndex != tc.wantIndex || ce.AddonType != tc.wantType {
				t.Fatalf("want ComposeError at addon[%d] %s, got %+v (%v)", tc.wantIndex, tc.wantType, ce, err)
			}
		})
	}
}

// TestCompose_TwoRagSourcesAccumulate is the positive counterpart: two
// rag_source addons are the SAME (llm) mode AND the SAME cadence, so they
// must both apply and accumulate their sources into one autonomy goal —
// Compose must succeed, not treat the second as a conflict.
func TestCompose_TwoRagSourcesAccumulate(t *testing.T) {
	deps := ComposeDeps{Templates: fakeMat{files: baseFiles()}, KnownMCP: map[string]bool{}}
	in := ComposeInput{
		TemplateSlug: "custom-base",
		Addons: []Addon{
			mustAddon(`{"type":"rag_source","source":"https://docs.example.com","cadence":"24h"}`),
			mustAddon(`{"type":"rag_source","source":"https://api.example.com","cadence":"24h"}`),
		},
	}
	files, _, err := Compose(in, deps)
	if err != nil {
		t.Fatalf("two llm-mode rag_source addons must compose without error: %v", err)
	}
	proj := files["projects/pricing-watch.yaml"]
	if !containsStr(proj, "https://docs.example.com") || !containsStr(proj, "https://api.example.com") {
		t.Fatalf("both rag sources should accumulate in the composed project goal:\n%s", proj)
	}
}

// TestCompose_TwoRagSourcesDifferentCadenceRejected guards the
// silent-clobber footgun: a second rag_source with a DIFFERENT cadence
// would otherwise overwrite Autonomy.PollInterval (last-wins) while both
// sources still claim to be tracked at their own cadence. Compose must
// reject the second addon instead.
func TestCompose_TwoRagSourcesDifferentCadenceRejected(t *testing.T) {
	deps := ComposeDeps{Templates: fakeMat{files: baseFiles()}, KnownMCP: map[string]bool{}}
	in := ComposeInput{
		TemplateSlug: "custom-base",
		Addons: []Addon{
			mustAddon(`{"type":"rag_source","source":"https://docs.example.com","cadence":"24h"}`),
			mustAddon(`{"type":"rag_source","source":"https://api.example.com","cadence":"12h"}`),
		},
	}
	_, _, err := Compose(in, deps)
	if err == nil {
		t.Fatal("two rag_source addons with different cadences must error")
	}
	var ce *ComposeError
	if !asComposeErr(err, &ce) || ce.AddonIndex != 1 || ce.AddonType != "rag_source" || ce.Field != "cadence" {
		t.Fatalf("want ComposeError at addon[1] rag_source field cadence, got %+v (%v)", ce, err)
	}
}

// TestCompose_TemplateEmitsMultipleProjectFiles guards findBySuffix's
// determinism: a template that emits two projects/*.yaml files must fail
// composition clearly instead of picking one arbitrarily via map
// iteration order.
func TestCompose_TemplateEmitsMultipleProjectFiles(t *testing.T) {
	deps := ComposeDeps{
		Templates: fakeMat{files: map[string]string{
			"projects/pricing-watch.yaml":   baseProjectYAML,
			"projects/pricing-watch-2.yaml": baseProjectYAML,
		}},
		KnownMCP: map[string]bool{},
	}
	in := ComposeInput{TemplateSlug: "custom-base"}
	_, _, err := Compose(in, deps)
	if err == nil {
		t.Fatal("template emitting two project YAML files must error")
	}
	var ce *ComposeError
	if !asComposeErr(err, &ce) || ce.AddonIndex != -1 {
		t.Fatalf("want ComposeError with AddonIndex -1, got %+v (%v)", ce, err)
	}
}

// TestCompose_TemplateEmitsMultipleSwarmFiles is the swarms/*.md
// counterpart of TestCompose_TemplateEmitsMultipleProjectFiles.
func TestCompose_TemplateEmitsMultipleSwarmFiles(t *testing.T) {
	deps := ComposeDeps{
		Templates: fakeMat{files: map[string]string{
			"projects/pricing-watch.yaml":     baseProjectYAML,
			"swarms/pricing-watch-swarm.md":   baseSwarmMD,
			"swarms/pricing-watch-swarm-2.md": baseSwarmMD,
		}},
		KnownMCP: map[string]bool{},
	}
	in := ComposeInput{TemplateSlug: "custom-base"}
	_, _, err := Compose(in, deps)
	if err == nil {
		t.Fatal("template emitting two swarm markdown files must error")
	}
	var ce *ComposeError
	if !asComposeErr(err, &ce) || ce.AddonIndex != -1 {
		t.Fatalf("want ComposeError with AddonIndex -1, got %+v (%v)", ce, err)
	}
}

// helpers
func mustAddon(raw string) Addon {
	var a Addon
	if err := jsonUnmarshal(raw, &a); err != nil {
		panic(err)
	}
	return a
}

// containsStr reuses the byte-substring search already defined by
// contains/indexOf above (Task 1's compose_test.go); kept as a
// distinctly-named wrapper per the brief so it reads standalone at
// each call site.
func containsStr(hay, needle string) bool {
	return contains(hay, needle)
}

// jsonUnmarshal is a thin wrapper so test helpers don't need to
// import encoding/json a second time under a different alias.
func jsonUnmarshal(raw string, v any) error {
	return json.Unmarshal([]byte(raw), v)
}

// errString is a minimal error value for tests that only care that
// MaterialiseMulti returned *an* error.
type errString string

func (e errString) Error() string { return string(e) }

// asComposeErr wraps errors.As so the Compose tests can assert the
// dynamic type without importing errors directly in every test.
func asComposeErr(err error, target **ComposeError) bool {
	return errors.As(err, target)
}
