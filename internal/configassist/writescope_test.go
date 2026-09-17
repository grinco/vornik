package configassist

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// Regression: audit 2026-09-15 CA-08 — "Read Scope Is Not an Enforced Write
// Scope". The snapshot authorizer admits shared pricing.yaml, role-library/
// and project-templates/ as READ-ONLY context every project may ground on.
// Nothing then enforced that distinction on the way out: the only write
// allowlist was the model's own declared PLAN, so a project-scoped request
// could file an edit to shared content — and blastRadius labelled it
// "project", understating what the write would affect.
func TestPropose_SharedReadOnlyContextIsNotWritable(t *testing.T) {
	f := newFixture(t)
	shared := filepath.Join(f.root, "pricing.yaml")
	if err := os.WriteFile(shared, []byte("models:\n  - name: glm-5.2\n    description: the default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.scriptEdit("pricing.yaml", "description: the default", "description: the default model for everything")
	f.scriptJudge("pass", "description: the default model for everything")

	res, err := f.engine.Propose(context.Background(), operatorReq("update the pricing description"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal == nil {
		t.Fatalf("a project-scoped request must not be able to write shared read-only context; got proposal %+v", res.Proposal)
	}
	if res.Refusal.Code != RefuseDiffScope {
		t.Fatalf("refusal code = %s, want %s", res.Refusal.Code, RefuseDiffScope)
	}
	if res.Proposal != nil {
		t.Fatal("nothing may be filed when the write scope is refused")
	}
	// The deployed file must be untouched.
	after, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "the default model for everything") {
		t.Fatal("the shared file was modified")
	}
}

// The project's own files stay writable — the gate narrows the shared
// context, it does not break ordinary editing.
func TestPropose_OwnProjectFilesRemainWritable(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.scriptJudge("pass", "cadence: 2h")
	res, err := f.engine.Propose(context.Background(), operatorReq("slow the feed down"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		t.Fatalf("the project's own file must stay writable: %+v", res.Refusal)
	}
}

// Regression: audit 2026-09-15 CA-08, second half — blastRadius recognised
// only swarms/ as shared, so a workflow or shared-library edit was announced
// to the operator as project scope. The label is what a reviewer reads before
// approving; it must not be narrower than the write.
func TestBlastRadius_SharedPathsAreNotProjectScope(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"configs/projects/assistant.yaml", persistence.ProposalScopeProject},
		{"configs/swarms/dev.md", persistence.ProposalScopeSwarm},
		{"configs/workflows/digest.md", persistence.ProposalScopeSwarm},
		{"configs/role-library/reviewer.md", persistence.ProposalScopeSwarm},
		{"configs/project-templates/base.yaml", persistence.ProposalScopeSwarm},
		{"configs/pricing.yaml", persistence.ProposalScopeSwarm},
	}
	for _, tc := range cases {
		got := blastRadius([]Op{{Op: "replace", Path: tc.path}})
		if got != tc.want {
			t.Errorf("blastRadius(%s) = %s, want %s — a shared edit must not be announced as project scope", tc.path, got, tc.want)
		}
	}
}

// Regression: audit 2026-09-15 CA-05, second half — "Read dependencies are
// incomplete as well: readDependencies adds only swarm and workflow files."
// A file the assistant actually READ during the loop shaped the proposal, so
// it belongs in the read set; otherwise a concurrent edit to it goes
// undetected at apply time.
func TestPropose_ReadSetIncludesFilesTheAssistantRead(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "pricing.yaml"), []byte("models:\n  - name: glm-5.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["projects/assistant.yaml"]`,
			call("c1", "file_read", map[string]any{"path": "pricing.yaml"})),
		toolResp("glm-5.2:cloud", "now editing",
			call("c2", "file_edit", map[string]any{"path": "projects/assistant.yaml", "old_string": "cadence: 1h", "new_string": "cadence: 2h"})),
		textResp("glm-5.2:cloud", "Consulted pricing and slowed the feed."),
	}
	f.scriptJudge("pass", "cadence: 2h")
	res, err := f.engine.Propose(context.Background(), operatorReq("slow the feed, mind the pricing"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		t.Fatalf("unexpected refusal: %+v", res.Refusal)
	}
	var ev EvidenceRecord
	if err := json.Unmarshal([]byte(res.Proposal.Evidence), &ev); err != nil {
		t.Fatal(err)
	}
	if _, ok := ev.ReadSet["configs/pricing.yaml"]; !ok {
		t.Fatalf("a file the assistant read must enter the read set: %+v", ev.ReadSet)
	}
}
