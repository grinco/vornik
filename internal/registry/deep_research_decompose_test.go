package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeepResearchDecomposeShape locks in the deep-research + research-subtask
// shape (2026-07-11): a decomposed, long-running research path for the
// assistant, mirroring issue-fix. decompose (lead) delegates a SEQUENTIAL chain
// of research-subtask leaves, each writing a findings file to the shared
// workspace; on resume a writer synthesizes and a publisher shares. Guards
// against a regression to a monolithic single-researcher shape or a broken
// delegation pin.
func TestDeepResearchDecomposeShape(t *testing.T) {
	root := repoRootFromRegistryTest(t)

	parse := func(name string) *Workflow {
		t.Helper()
		path := filepath.Join(root, "configs", "workflows", name)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		wf, err := ParseWorkflowMarkdown(content, path)
		if err != nil {
			t.Fatalf("ParseWorkflowMarkdown(%s): %v", name, err)
		}
		return wf
	}

	deep := parse("deep-research.md")
	if deep.Entrypoint != "decompose" {
		t.Errorf("deep-research entrypoint = %q; want decompose", deep.Entrypoint)
	}
	if !deep.ResumeAfterChildren {
		t.Errorf("deep-research resume_after_children = false; want true (else it won't resume after the subtask chain)")
	}
	dec, ok := deep.Steps["decompose"]
	if !ok {
		t.Fatalf("deep-research missing decompose step")
	}
	if dec.Role != "lead" || dec.OnSuccess != "synthesize" {
		t.Errorf("decompose role/on_success = %q/%q; want lead/synthesize", dec.Role, dec.OnSuccess)
	}
	if dec.DelegatedWorkflow != "research-subtask" {
		t.Errorf("decompose delegated_workflow = %q; want research-subtask (else subtasks fall back to the project default)", dec.DelegatedWorkflow)
	}
	syn, ok := deep.Steps["synthesize"]
	if !ok || syn.Role != "writer" {
		t.Fatalf("deep-research synthesize step missing or wrong role: %+v", syn)
	}
	if syn.OnSuccess != "publish" {
		t.Errorf("synthesize on_success = %q; want publish", syn.OnSuccess)
	}
	pub, ok := deep.Steps["publish"]
	if !ok || pub.Role != "publisher" {
		t.Fatalf("deep-research publish step missing or wrong role: %+v", pub)
	}

	// The leaf: a single bounded researcher pass, no decomposition.
	sub := parse("research-subtask.md")
	if sub.Entrypoint != "research" {
		t.Errorf("research-subtask entrypoint = %q; want research", sub.Entrypoint)
	}
	r, ok := sub.Steps["research"]
	if !ok || r.Role != "researcher" {
		t.Fatalf("research-subtask research step missing or wrong role: %+v", r)
	}
	if r.OnFail != "failed" {
		t.Errorf("research-subtask research on_fail = %q; want failed (a failed subtask fails the chain)", r.OnFail)
	}
}
