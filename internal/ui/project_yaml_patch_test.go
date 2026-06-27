package ui

import (
	"strings"
	"testing"
)

// commentedYAML is a small fixture that mirrors the shape of the
// bundled assistant-project.yaml: explanatory comments above
// values, commented-out scaffolds, and nested sections. Patcher
// regressions show up here as comment loss.
const commentedYAML = `# Top-of-file project banner — preserved verbatim.
projectId: "demo"
displayName: "Demo"
# swarmId names the swarm definition this project uses.
swarmId: "demo-swarm"
defaultWorkflowId: "adaptive"

# autonomy controls the autonomous-loop scheduler.
autonomy:
  enabled: false
  # Goal is the high-level objective the loop works toward.
  goal: |
    First-line goal text.
    Second-line goal text.
  mode: llm

# permissions caps what tools the swarm can call.
permissions:
  secrets: []
  allowedTools:
    - "current_time"
    - "file_read"

# budget:
#   daily_hard_usd: 20.0
`

// TestApplyYAMLPatches_UpdatesExistingScalar — single-line scalar
// update preserves the head comment and surrounding structure.
func TestApplyYAMLPatches_UpdatesExistingScalar(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"displayName"}, Value: "Demo Renamed"},
	}
	out, err := applyYAMLPatches([]byte(commentedYAML), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `displayName: "Demo Renamed"`) {
		t.Errorf("displayName not updated. got:\n%s", got)
	}
	if !strings.Contains(got, "# Top-of-file project banner") {
		t.Errorf("top banner comment lost. got:\n%s", got)
	}
	if !strings.Contains(got, "# swarmId names the swarm definition") {
		t.Errorf("swarmId head comment lost. got:\n%s", got)
	}
	if !strings.Contains(got, "# budget:") {
		t.Errorf("commented-out budget scaffold lost. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_UpdatesNestedScalar — patching a nested
// path (autonomy.mode) leaves sibling autonomy.* comments intact.
func TestApplyYAMLPatches_UpdatesNestedScalar(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"autonomy", "mode"}, Value: "cron"},
	}
	out, err := applyYAMLPatches([]byte(commentedYAML), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `mode: "cron"`) {
		t.Errorf("autonomy.mode not updated to cron. got:\n%s", got)
	}
	if !strings.Contains(got, "# Goal is the high-level objective") {
		t.Errorf("sibling autonomy.goal comment lost. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_MultilineStringLiteralStyle — multi-line
// strings (autonomy.goal) round-trip as a YAML literal block,
// not as escaped \n inside a quoted scalar.
func TestApplyYAMLPatches_MultilineStringLiteralStyle(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"autonomy", "goal"}, Value: "Line one.\nLine two.\nLine three."},
	}
	out, err := applyYAMLPatches([]byte(commentedYAML), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "goal: |") {
		t.Errorf("multi-line goal didn't use literal style. got:\n%s", got)
	}
	if !strings.Contains(got, "Line one.") || !strings.Contains(got, "Line three.") {
		t.Errorf("multi-line goal contents lost. got:\n%s", got)
	}
	if strings.Contains(got, `\n`) {
		t.Errorf("multi-line goal serialised with backslash-n escapes. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_RemoveIfEmpty — setting a field to its
// zero value with RemoveIfEmpty deletes the key entirely rather
// than writing `key: ""`. Keeps the rendered file clean.
func TestApplyYAMLPatches_RemoveIfEmpty(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"displayName"}, Value: "", RemoveIfEmpty: true},
	}
	out, err := applyYAMLPatches([]byte(commentedYAML), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "displayName") {
		t.Errorf("displayName not removed. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_RemoveIfEmptyAbsentKey_NoOp — removing a
// key that doesn't exist is a no-op (doesn't error and doesn't
// synthesise an empty intermediate map).
func TestApplyYAMLPatches_RemoveIfEmptyAbsentKey_NoOp(t *testing.T) {
	yaml := `projectId: "x"
`
	patches := []yamlPatch{
		{Path: []string{"chat", "system_prefix"}, Value: "", RemoveIfEmpty: true},
	}
	out, err := applyYAMLPatches([]byte(yaml), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "chat:") {
		t.Errorf("chat: intermediate map synthesised for a no-op remove. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_CreatesIntermediateMap — a patch targeting
// a path whose intermediate mapping doesn't yet exist synthesises
// the intermediate. Phase 1B form needs this when adding e.g.
// budget.daily_hard_usd to a project that didn't have a budget
// section at all.
func TestApplyYAMLPatches_CreatesIntermediateMap(t *testing.T) {
	yaml := `projectId: "x"
`
	patches := []yamlPatch{
		{Path: []string{"chat", "system_prefix"}, Value: "house rule"},
	}
	out, err := applyYAMLPatches([]byte(yaml), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "chat:") {
		t.Errorf("chat: intermediate not synthesised. got:\n%s", got)
	}
	if !strings.Contains(got, `system_prefix: "house rule"`) {
		t.Errorf("nested value not written. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_Float64 — budget amounts are floats.
// The patcher renders them as YAML scalars without quotes so
// downstream consumers see a number, not a string. Integral
// floats keep their decimal point to signal "this is a float
// field" (and to match the bundled YAMLs' `daily_hard_usd: 20.0`
// style — emitting `20` would diff-noise the file).
func TestApplyYAMLPatches_Float64(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"budget", "daily_hard_usd"}, Value: 20.0},
		{Path: []string{"budget", "monthly_hard_usd"}, Value: 199.95},
	}
	out, err := applyYAMLPatches([]byte(commentedYAML), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "daily_hard_usd: 20") {
		t.Errorf("integral float not rendered. got:\n%s", got)
	}
	if !strings.Contains(got, "monthly_hard_usd: 199.95") {
		t.Errorf("fractional float not rendered with two decimals. got:\n%s", got)
	}
	if strings.Contains(got, `"20"`) || strings.Contains(got, `"199.95"`) {
		t.Errorf("float rendered as quoted string. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_Float64RemoveIfEmpty — zero floats with
// RemoveIfEmpty delete the key entirely (consistent with
// int/string semantics) so the operator clearing a budget field
// in the form removes it from disk rather than writing `0.0`.
// Uses a focused fixture without the commented-out budget block
// so we can assert key absence without the comment confusing us.
func TestApplyYAMLPatches_Float64RemoveIfEmpty(t *testing.T) {
	yaml := `projectId: "x"
budget:
  daily_hard_usd: 20.0
  monthly_hard_usd: 200.0
`
	patches := []yamlPatch{
		{Path: []string{"budget", "daily_hard_usd"}, Value: float64(0), RemoveIfEmpty: true},
	}
	out, err := applyYAMLPatches([]byte(yaml), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "daily_hard_usd") {
		t.Errorf("zero float with RemoveIfEmpty should delete key. got:\n%s", got)
	}
	if !strings.Contains(got, "monthly_hard_usd: 200") {
		t.Errorf("sibling float key should survive the remove. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_Int64 — GitHub App identifiers (AppID,
// InstallationID) and the Telegram chat id are int64 in the
// project schema. The patcher must accept them as unquoted YAML
// numbers so the loader's typed Unmarshal sees them as numbers,
// not strings.
func TestApplyYAMLPatches_Int64(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"github_app", "app_id"}, Value: int64(123456789)},
		{Path: []string{"trading", "notify_fills_chat_id"}, Value: int64(-100123456)},
	}
	out, err := applyYAMLPatches([]byte(""), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "app_id: 123456789") {
		t.Errorf("int64 not rendered. got:\n%s", got)
	}
	if !strings.Contains(got, "notify_fills_chat_id: -100123456") {
		t.Errorf("negative int64 not rendered. got:\n%s", got)
	}
	if strings.Contains(got, `"123456789"`) {
		t.Errorf("int64 rendered as quoted string. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_Int64RemoveIfEmpty — zero int64 with
// RemoveIfEmpty deletes the key (consistent with int / float
// behaviour).
func TestApplyYAMLPatches_Int64RemoveIfEmpty(t *testing.T) {
	yaml := `github_app:
  app_id: 555
`
	patches := []yamlPatch{
		{Path: []string{"github_app", "app_id"}, Value: int64(0), RemoveIfEmpty: true},
	}
	out, err := applyYAMLPatches([]byte(yaml), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	if strings.Contains(string(out), "app_id") {
		t.Errorf("zero int64 with RemoveIfEmpty should delete key. got:\n%s", out)
	}
}

// TestApplyYAMLPatches_BoolAndInt — typed values land as YAML
// scalars without quotes. A renderer that quotes "true" or "10"
// would break consumers that expect typed YAML.
func TestApplyYAMLPatches_BoolAndInt(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"autonomy", "enabled"}, Value: true},
		{Path: []string{"autonomy", "maxTasksPerHour"}, Value: 25},
	}
	out, err := applyYAMLPatches([]byte(commentedYAML), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "enabled: true") {
		t.Errorf("bool not rendered unquoted. got:\n%s", got)
	}
	if !strings.Contains(got, "maxTasksPerHour: 25") {
		t.Errorf("int not rendered unquoted. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_StringSequence — []string lands as a
// block-style sequence (one item per line). Flow style
// (`[a, b, c]`) would diff-noisily across edits.
func TestApplyYAMLPatches_StringSequence(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"permissions", "allowedTools"}, Value: []string{"grep", "file_write", "current_time"}},
	}
	out, err := applyYAMLPatches([]byte(commentedYAML), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "allowedTools:") {
		t.Errorf("allowedTools key missing. got:\n%s", got)
	}
	if !strings.Contains(got, `- "grep"`) || !strings.Contains(got, `- "file_write"`) {
		t.Errorf("sequence items not rendered block-style. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_RejectsNonMappingTraversal — if the path
// walks through a key that exists but isn't a mapping (e.g. the
// caller passes ["autonomy", "goal", "x"] when autonomy.goal is
// a string), we refuse rather than silently overwriting.
func TestApplyYAMLPatches_RejectsNonMappingTraversal(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"autonomy", "goal", "x"}, Value: "y"},
	}
	_, err := applyYAMLPatches([]byte(commentedYAML), patches)
	if err == nil || !strings.Contains(err.Error(), "not a mapping") {
		t.Errorf("err = %v, want non-mapping rejection", err)
	}
}

// TestApplyYAMLPatches_EmptyContent — an empty file is patchable
// (form-creation path); the patcher synthesises a document +
// root mapping rather than erroring.
func TestApplyYAMLPatches_EmptyContent(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"projectId"}, Value: "fresh"},
	}
	out, err := applyYAMLPatches([]byte(""), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches on empty: %v", err)
	}
	if !strings.Contains(string(out), `projectId: "fresh"`) {
		t.Errorf("empty-file patch not written. got:\n%s", out)
	}
}

// TestApplySequencePatches_UpdatesElementScalar — single role
// in a swarm-like sequence gets its description rewritten while
// every other role + every sibling key + every comment survives.
// This is the foundation for the swarm editor's per-role
// frontmatter editing path.
func TestApplySequencePatches_UpdatesElementScalar(t *testing.T) {
	yamlBody := `# top comment
swarmId: "demo"
roles:
  - name: "lead"
    description: "Plans"
    model: "lead-model"
  - name: "coder"
    description: "Codes"
    model: "coder-model"
`
	patches := []yamlPatch{
		{Path: []string{"description"}, Value: "Plans with care"},
	}
	out, err := applyYAMLSequenceElementPatches([]byte(yamlBody), "roles", "name", "lead", patches)
	if err != nil {
		t.Fatalf("applyYAMLSequenceElementPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `description: "Plans with care"`) {
		t.Errorf("lead description not updated. got:\n%s", got)
	}
	if !strings.Contains(got, `description: "Codes"`) {
		t.Errorf("coder description should be unchanged. got:\n%s", got)
	}
	if !strings.Contains(got, "# top comment") {
		t.Errorf("top comment lost. got:\n%s", got)
	}
}

// TestApplySequencePatches_AddsKeyToElement — patching a key
// that doesn't yet exist on the target element adds it without
// disturbing siblings.
func TestApplySequencePatches_AddsKeyToElement(t *testing.T) {
	yamlBody := `roles:
  - name: "lead"
    description: "Plans"
`
	patches := []yamlPatch{
		{Path: []string{"model"}, Value: "new-model"},
	}
	out, err := applyYAMLSequenceElementPatches([]byte(yamlBody), "roles", "name", "lead", patches)
	if err != nil {
		t.Fatalf("applyYAMLSequenceElementPatches: %v", err)
	}
	if !strings.Contains(string(out), `model: "new-model"`) {
		t.Errorf("new model key not added. got:\n%s", string(out))
	}
}

// TestApplySequencePatches_RemoveIfEmpty — clearing a field on
// one role deletes the key from that role only.
func TestApplySequencePatches_RemoveIfEmpty(t *testing.T) {
	yamlBody := `roles:
  - name: "lead"
    description: "Plans"
    model: "lead-model"
  - name: "coder"
    model: "coder-model"
`
	patches := []yamlPatch{
		{Path: []string{"model"}, Value: "", RemoveIfEmpty: true},
	}
	out, err := applyYAMLSequenceElementPatches([]byte(yamlBody), "roles", "name", "lead", patches)
	if err != nil {
		t.Fatalf("applyYAMLSequenceElementPatches: %v", err)
	}
	got := string(out)
	// lead.model removed; coder.model survives.
	if strings.Contains(got, "lead-model") {
		t.Errorf("lead.model should be removed. got:\n%s", got)
	}
	if !strings.Contains(got, `model: "coder-model"`) {
		t.Errorf("coder.model should survive. got:\n%s", got)
	}
}

// TestApplySequencePatches_StringSequenceValue — patching a
// list-typed field (e.g. role.permissions.allowedTools nested
// inside a role element). Uses a nested path inside the element.
func TestApplySequencePatches_StringSequenceValue(t *testing.T) {
	yamlBody := `roles:
  - name: "lead"
    permissions:
      allowedTools: ["a", "b"]
`
	patches := []yamlPatch{
		{Path: []string{"permissions", "allowedTools"}, Value: []string{"c", "d", "e"}},
	}
	out, err := applyYAMLSequenceElementPatches([]byte(yamlBody), "roles", "name", "lead", patches)
	if err != nil {
		t.Fatalf("applyYAMLSequenceElementPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `- "c"`) || !strings.Contains(got, `- "d"`) || !strings.Contains(got, `- "e"`) {
		t.Errorf("nested allowedTools not rewritten. got:\n%s", got)
	}
}

// TestApplySequencePatches_ElementNotFound — id-value that
// doesn't match any element is operator error; the patcher
// returns an error rather than silently doing nothing.
func TestApplySequencePatches_ElementNotFound(t *testing.T) {
	yamlBody := `roles:
  - name: "lead"
    description: "Plans"
`
	_, err := applyYAMLSequenceElementPatches([]byte(yamlBody), "roles", "name", "ghost", []yamlPatch{
		{Path: []string{"description"}, Value: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("err = %v, want element-not-found rejection", err)
	}
}

// TestApplySequencePatches_SequenceMissing — top-level key
// doesn't exist or isn't a sequence; surface the structural
// mismatch.
func TestApplySequencePatches_SequenceMissing(t *testing.T) {
	_, err := applyYAMLSequenceElementPatches([]byte(`swarmId: "x"`+"\n"), "roles", "name", "lead", []yamlPatch{
		{Path: []string{"description"}, Value: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "roles") {
		t.Errorf("err = %v, want sequence-missing rejection", err)
	}
}

// TestApplySequencePatches_EmptyPatches_NoOp — zero patches
// returns the input verbatim, no rewriting.
func TestApplySequencePatches_EmptyPatches_NoOp(t *testing.T) {
	yamlBody := `roles:
  - name: "lead"
    description: "Plans"
`
	out, err := applyYAMLSequenceElementPatches([]byte(yamlBody), "roles", "name", "lead", nil)
	if err != nil {
		t.Fatalf("applyYAMLSequenceElementPatches: %v", err)
	}
	if string(out) != yamlBody {
		t.Errorf("empty patches should return verbatim input")
	}
}

// TestApplyYAMLPatches_MultiplePatches_OrderPreserved — patches
// apply in slice order; the second update wins for the same
// target so callers can compose deletes-then-creates.
func TestApplyYAMLPatches_MultiplePatches_OrderPreserved(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"displayName"}, Value: "first"},
		{Path: []string{"displayName"}, Value: "second"},
	}
	out, err := applyYAMLPatches([]byte(commentedYAML), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `displayName: "second"`) {
		t.Errorf("second patch did not win. got:\n%s", got)
	}
	if strings.Contains(got, `displayName: "first"`) {
		t.Errorf("first patch still present. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_MapSequence — array-of-mappings (used by
// the MCP servers form) lands as a block-style sequence of
// mappings with one mapping per entry. Each entry honours the
// "_order" sentinel so emit order is stable across saves.
func TestApplyYAMLPatches_MapSequence(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"mcp", "servers"}, Value: []map[string]any{
			{
				"name":          "scraper",
				"transport":     "sse",
				"url":           "http://x/sse",
				"allowed_tools": []string{"web_fetch"},
				"_order":        []string{"name", "transport", "url", "allowed_tools"},
			},
			{
				"name":      "secret",
				"transport": "stdio",
				"_order":    []string{"name", "transport", "url", "allowed_tools"},
			},
		}},
	}
	out, err := applyYAMLPatches([]byte(""), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "mcp:") {
		t.Errorf("mcp intermediate not synthesised. got:\n%s", got)
	}
	if !strings.Contains(got, "servers:") {
		t.Errorf("servers sequence not emitted. got:\n%s", got)
	}
	if !strings.Contains(got, `name: "scraper"`) {
		t.Errorf("first entry's name not emitted. got:\n%s", got)
	}
	if !strings.Contains(got, `name: "secret"`) {
		t.Errorf("second entry's name not emitted. got:\n%s", got)
	}
	// _order sentinel must not leak into the rendered YAML.
	if strings.Contains(got, "_order:") {
		t.Errorf("_order sentinel leaked into rendered YAML. got:\n%s", got)
	}
	// Empty string fields (e.g. the second entry has no url) must
	// not surface as `url: ""` litter.
	idxSecret := strings.Index(got, `name: "secret"`)
	if idxSecret < 0 {
		t.Fatalf("second entry missing entirely")
	}
	// Next ~80 chars after the second entry's name shouldn't have
	// a url: line for that entry.
	window := got[idxSecret:]
	if i := strings.Index(window, "- name:"); i > 0 {
		window = window[:i]
	}
	if strings.Contains(window, `url:`) {
		t.Errorf("empty url leaked into rendered second entry. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_MapSequenceRemoveIfEmpty — passing
// []map[string]any{} with RemoveIfEmpty deletes the parent's
// sequence key. Used by the MCP form when the operator
// unsubscribes from every server.
func TestApplyYAMLPatches_MapSequenceRemoveIfEmpty(t *testing.T) {
	yaml := `mcp:
  servers:
    - name: stale
      transport: sse
`
	patches := []yamlPatch{
		{Path: []string{"mcp", "servers"}, Value: []map[string]any{}, RemoveIfEmpty: true},
	}
	out, err := applyYAMLPatches([]byte(yaml), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	if strings.Contains(string(out), "servers:") {
		t.Errorf("empty []map should remove the sequence key. got:\n%s", out)
	}
}

// TestApplyYAMLPatches_MapSequenceAlphabeticalFallback — keys
// without an _order sentinel emit alphabetically so the rendered
// YAML stays diff-stable even though Go map iteration is random.
func TestApplyYAMLPatches_MapSequenceAlphabeticalFallback(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"mcp", "servers"}, Value: []map[string]any{{
			"zebra":  "z",
			"alpha":  "a",
			"mango":  "m",
			"banana": "b",
		}}},
	}
	out, err := applyYAMLPatches([]byte(""), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	idxAlpha := strings.Index(got, "alpha:")
	idxBanana := strings.Index(got, "banana:")
	idxMango := strings.Index(got, "mango:")
	idxZebra := strings.Index(got, "zebra:")
	if idxAlpha >= idxBanana || idxBanana >= idxMango || idxMango >= idxZebra {
		t.Errorf("alphabetical fallback order not honoured. got:\n%s", got)
	}
}

// TestApplyYAMLPatches_MapSequenceNilMap — nil map element is
// safe (empty mapping emitted). Defensive — no caller should pass
// nil but the patcher must not panic.
func TestApplyYAMLPatches_MapSequenceNilMap(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"mcp", "servers"}, Value: []map[string]any{nil}},
	}
	out, err := applyYAMLPatches([]byte(""), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	if !strings.Contains(string(out), "servers:") {
		t.Errorf("nil map element should produce an empty entry, not crash. got:\n%s", out)
	}
}

// TestApplyYAMLPatches_MapSequenceStableOrder — _order sentinel
// keeps keys-within-an-entry emit order stable across saves so
// the diff stays clean even though Go map iteration is random.
func TestApplyYAMLPatches_MapSequenceStableOrder(t *testing.T) {
	patches := []yamlPatch{
		{Path: []string{"mcp", "servers"}, Value: []map[string]any{{
			"name":          "scraper",
			"transport":     "sse",
			"url":           "http://x/sse",
			"allowed_tools": []string{"web_fetch"},
			"_order":        []string{"name", "transport", "url", "allowed_tools"},
		}}},
	}
	out, err := applyYAMLPatches([]byte(""), patches)
	if err != nil {
		t.Fatalf("applyYAMLPatches: %v", err)
	}
	got := string(out)
	idxName := strings.Index(got, "name:")
	idxTransport := strings.Index(got, "transport:")
	idxURL := strings.Index(got, "url:")
	idxAllowed := strings.Index(got, "allowed_tools:")
	if idxName >= idxTransport || idxTransport >= idxURL || idxURL >= idxAllowed {
		t.Errorf("emit order not honoured. got:\n%s", got)
	}
}
