package projectwizard

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func newCP() *composedProject {
	return &composedProject{
		Project: &registry.Project{ID: "p", SwarmID: "p-swarm", DefaultWorkflowID: "adaptive"},
		Swarm:   &registry.Swarm{ID: "p-swarm", Roles: []registry.SwarmRole{{Name: "lead"}}},
	}
}

func TestMCPServerApplier(t *testing.T) {
	a := mcpServerApplier{known: map[string]bool{"slack": true, "github": true}}
	cp := newCP()
	if err := a.Apply(cp, []byte(`{"type":"mcp_server","name":"slack","allowed_tools":["send_message"]}`)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if len(cp.Project.MCP.Servers) != 1 || cp.Project.MCP.Servers[0].Name != "slack" ||
		len(cp.Project.MCP.Servers[0].AllowedTools) != 1 {
		t.Fatalf("server not appended correctly: %+v", cp.Project.MCP.Servers)
	}
	// Unknown server → error.
	if err := a.Apply(cp, []byte(`{"name":"jira"}`)); err == nil {
		t.Fatal("unknown server must error")
	}
	// Duplicate name → error.
	if err := a.Apply(cp, []byte(`{"name":"slack"}`)); err == nil {
		t.Fatal("duplicate server must error")
	}
	// Empty name → error.
	if err := a.Apply(cp, []byte(`{"name":""}`)); err == nil {
		t.Fatal("empty name must error")
	}
}

func TestChatToolsApplier(t *testing.T) {
	a := chatToolsApplier{}
	cp := newCP()
	cp.Project.Permissions.AllowedTools = []string{"file_read"}
	if err := a.Apply(cp, []byte(`{"allowed_tools":["memory_search","file_read","grep"]}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Extends, de-dupes (file_read already present), preserves order of new ones.
	got := cp.Project.Permissions.AllowedTools
	want := []string{"file_read", "memory_search", "grep"}
	if len(got) != len(want) {
		t.Fatalf("allowedTools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allowedTools = %v, want %v", got, want)
		}
	}
	// Empty list → error (nothing to add).
	if err := a.Apply(cp, []byte(`{"allowed_tools":[]}`)); err == nil {
		t.Fatal("empty allowed_tools must error")
	}
}

func TestScheduleApplier(t *testing.T) {
	a := scheduleApplier{}
	cp := newCP()
	err := a.Apply(cp, []byte(`{"interval":"168h","goal":"weekly digest","task_type":"report"}`))
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	au := cp.Project.Autonomy
	if !au.Enabled || au.Mode != registry.AutonomyModeCron || au.Goal != "weekly digest" ||
		au.PollInterval != "168h" || au.CronTaskType != "report" {
		t.Fatalf("autonomy not set: %+v", au)
	}
	// Unparseable interval → error.
	if err := a.Apply(newCP(), []byte(`{"interval":"weekly","goal":"g"}`)); err == nil {
		t.Fatal("bad interval must error")
	}
	// Missing goal → error (cron fires the goal verbatim).
	if err := a.Apply(newCP(), []byte(`{"interval":"24h"}`)); err == nil {
		t.Fatal("missing goal must error")
	}
	// Default task_type when omitted.
	cp2 := newCP()
	if err := a.Apply(cp2, []byte(`{"interval":"24h","goal":"g"}`)); err != nil {
		t.Fatal(err)
	}
	if cp2.Project.Autonomy.CronTaskType == "" {
		t.Fatal("task_type should default to a non-empty value")
	}
}

func TestSecretRequirementApplier_NeverFails(t *testing.T) {
	a := secretRequirementApplier{}
	cp := newCP()
	if err := a.Apply(cp, []byte(`{"name":"SLACK_TOKEN","label":"Slack bot token"}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Added to declared secrets AND to the doctor to-do list.
	if len(cp.Project.Permissions.Secrets) != 1 || cp.Project.Permissions.Secrets[0] != "SLACK_TOKEN" {
		t.Fatalf("declared secrets: %v", cp.Project.Permissions.Secrets)
	}
	if len(cp.Secrets) != 1 || cp.Secrets[0].Name != "SLACK_TOKEN" || cp.Secrets[0].Label != "Slack bot token" {
		t.Fatalf("doctor secrets: %+v", cp.Secrets)
	}
	// Duplicate name is idempotent (no dup entry, no error).
	if err := a.Apply(cp, []byte(`{"name":"SLACK_TOKEN"}`)); err != nil {
		t.Fatalf("dup: %v", err)
	}
	if len(cp.Project.Permissions.Secrets) != 1 {
		t.Fatalf("dup secret should be idempotent: %v", cp.Project.Permissions.Secrets)
	}
	// Empty name is the only error case.
	if err := a.Apply(cp, []byte(`{"name":""}`)); err == nil {
		t.Fatal("empty secret name must error")
	}
}

func TestRolePromptAppendApplier(t *testing.T) {
	a := rolePromptAppendApplier{}
	cp := newCP()
	cp.Swarm.Roles[0].SystemPrompt = "You are the lead."
	if err := a.Apply(cp, []byte(`{"role":"lead","text":"Always cite sources."}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if cp.Swarm.Roles[0].SystemPrompt != "You are the lead.\n\nAlways cite sources." {
		t.Fatalf("prompt not appended: %q", cp.Swarm.Roles[0].SystemPrompt)
	}
	// Accumulates in order.
	if err := a.Apply(cp, []byte(`{"role":"lead","text":"Be concise."}`)); err != nil {
		t.Fatal(err)
	}
	if cp.Swarm.Roles[0].SystemPrompt != "You are the lead.\n\nAlways cite sources.\n\nBe concise." {
		t.Fatalf("second append wrong: %q", cp.Swarm.Roles[0].SystemPrompt)
	}
	// Unknown role → error (appliers never create roles).
	if err := a.Apply(cp, []byte(`{"role":"ghost","text":"x"}`)); err == nil {
		t.Fatal("unknown role must error")
	}
	// Empty text → error.
	if err := a.Apply(cp, []byte(`{"role":"lead","text":""}`)); err == nil {
		t.Fatal("empty text must error")
	}
	// Nil swarm → error (not a panic).
	cp2 := &composedProject{Project: &registry.Project{ID: "p"}}
	if err := a.Apply(cp2, []byte(`{"role":"lead","text":"x"}`)); err == nil {
		t.Fatal("nil swarm must error, not panic")
	}
}

func TestRagSourceApplier(t *testing.T) {
	a := ragSourceApplier{}
	cp := newCP()
	if err := a.Apply(cp, []byte(`{"source":"https://docs.example.com","cadence":"24h"}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	au := cp.Project.Autonomy
	// Enables llm-mode freshness tracking + records the source in the goal.
	if !au.Enabled || au.Mode != registry.AutonomyModeLLM {
		t.Fatalf("rag_source should enable llm-mode autonomy: %+v", au)
	}
	if !contains(au.Goal, "https://docs.example.com") {
		t.Fatalf("goal should mention the source: %q", au.Goal)
	}
	if au.PollInterval != "24h" {
		t.Fatalf("cadence not applied: %q", au.PollInterval)
	}
	// Second source accumulates into the same goal. rag_source owns the llm
	// autonomy mode and is additive: two rag_source addons tracking two doc
	// sites are both valid (same mode), so both sources land in the goal.
	if err := a.Apply(cp, []byte(`{"source":"https://api.example.com","cadence":"24h"}`)); err != nil {
		t.Fatal(err)
	}
	if !contains(cp.Project.Autonomy.Goal, "https://api.example.com") ||
		!contains(cp.Project.Autonomy.Goal, "https://docs.example.com") {
		t.Fatalf("both sources should accumulate in the goal: %q", cp.Project.Autonomy.Goal)
	}
	// Bad cadence → error.
	if err := a.Apply(newCP(), []byte(`{"source":"x","cadence":"soon"}`)); err == nil {
		t.Fatal("bad cadence must error")
	}
	// Zero/negative cadence → error (parity with scheduleApplier: a
	// non-positive interval is a tight-loop risk downstream).
	if err := a.Apply(newCP(), []byte(`{"source":"x","cadence":"0s"}`)); err == nil {
		t.Fatal("zero cadence must error")
	}
	if err := a.Apply(newCP(), []byte(`{"source":"x","cadence":"-5m"}`)); err == nil {
		t.Fatal("negative cadence must error")
	}
	// Empty source → error.
	if err := a.Apply(newCP(), []byte(`{"source":"","cadence":"24h"}`)); err == nil {
		t.Fatal("empty source must error")
	}
	// Source containing a newline → error (it's concatenated into the
	// autonomy goal text as a bullet line; a newline would let it inject
	// an extra line).
	if err := a.Apply(newCP(), []byte(`{"source":"https://docs.example.com\nEVIL: line","cadence":"24h"}`)); err == nil {
		t.Fatal("source with newline must error")
	}
}

// TestRagSourceApplier_RejectsDifferingCadence guards the silent-clobber
// footgun: a second rag_source addon on the same (llm-mode) project with
// a DIFFERENT cadence must not silently overwrite Autonomy.PollInterval
// while both sources still land in the goal claiming to be tracked. Only
// a matching cadence may accumulate.
func TestRagSourceApplier_RejectsDifferingCadence(t *testing.T) {
	a := ragSourceApplier{}
	cp := newCP()
	if err := a.Apply(cp, []byte(`{"source":"https://docs.example.com","cadence":"24h"}`)); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	err := a.Apply(cp, []byte(`{"source":"https://api.example.com","cadence":"12h"}`))
	if err == nil {
		t.Fatal("second rag_source with a differing cadence must error")
	}
	// No mutation from the rejected addon: cadence and goal stay as the first left them.
	if cp.Project.Autonomy.PollInterval != "24h" {
		t.Fatalf("cadence must not be clobbered by a rejected addon: %q", cp.Project.Autonomy.PollInterval)
	}
	if contains(cp.Project.Autonomy.Goal, "https://api.example.com") {
		t.Fatalf("rejected addon's source must not land in the goal: %q", cp.Project.Autonomy.Goal)
	}
}

// TestScheduleApplier_RejectsWhenAutonomyAlreadyEnabled guards the
// cross-applier footgun: a base template that already schedules, or an
// earlier autonomy-writing addon in the same composition, must not be
// silently overwritten by a later schedule addon. Exactly one addon (or
// the base template) may own the Autonomy block.
func TestScheduleApplier_RejectsWhenAutonomyAlreadyEnabled(t *testing.T) {
	a := scheduleApplier{}
	cp := newCP()
	cp.Project.Autonomy.Enabled = true
	cp.Project.Autonomy.Mode = registry.AutonomyModeLLM
	cp.Project.Autonomy.Goal = "existing goal"
	cp.Project.Autonomy.PollInterval = "24h"

	err := a.Apply(cp, []byte(`{"interval":"168h","goal":"weekly digest","task_type":"report"}`))
	if err == nil {
		t.Fatal("schedule addon on an already-autonomous project must error")
	}
	// No mutation: the pre-existing autonomy block must be untouched.
	if cp.Project.Autonomy.Mode != registry.AutonomyModeLLM ||
		cp.Project.Autonomy.Goal != "existing goal" ||
		cp.Project.Autonomy.PollInterval != "24h" {
		t.Fatalf("autonomy block must not be mutated on rejection: %+v", cp.Project.Autonomy)
	}
}

// TestRagSourceApplier_RejectsDifferentModeConflict guards the MODE
// conflict: rag_source owns llm mode and accumulates, so it may layer onto
// an existing llm-mode block, but a cron (or other non-llm) block already
// owning autonomy is an incoherent last-wins conflict and must be rejected.
func TestRagSourceApplier_RejectsDifferentModeConflict(t *testing.T) {
	a := ragSourceApplier{}
	cp := newCP()
	cp.Project.Autonomy.Enabled = true
	cp.Project.Autonomy.Mode = registry.AutonomyModeCron
	cp.Project.Autonomy.Goal = "existing goal"
	cp.Project.Autonomy.PollInterval = "24h"
	cp.Project.Autonomy.CronTaskType = "report"

	err := a.Apply(cp, []byte(`{"source":"https://docs.example.com","cadence":"12h"}`))
	if err == nil {
		t.Fatal("rag_source addon on a cron-mode autonomy block must error (mode conflict)")
	}
	// No mutation: the pre-existing autonomy block must be untouched.
	if cp.Project.Autonomy.Mode != registry.AutonomyModeCron ||
		cp.Project.Autonomy.Goal != "existing goal" ||
		cp.Project.Autonomy.PollInterval != "24h" ||
		cp.Project.Autonomy.CronTaskType != "report" {
		t.Fatalf("autonomy block must not be mutated on rejection: %+v", cp.Project.Autonomy)
	}
}

var _ = json.Marshal // keep import if unused after edits
