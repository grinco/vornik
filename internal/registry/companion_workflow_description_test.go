package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shippedWorkflowsDir is the repo copy of the workflow library, read
// relative to this package.
const shippedWorkflowsDir = "../../configs/workflows"

// loadShippedWorkflow parses one file out of configs/workflows/.
func loadShippedWorkflow(t *testing.T, name string) *Workflow {
	t.Helper()
	path := filepath.Join(shippedWorkflowsDir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	wf, err := ParseWorkflowMarkdown(raw, name)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// TestShippedResearchGather_DescriptionStatesNoNetwork pins the
// 2026-09-11 incident (companion unreachable-work guard design §4.1).
//
// companion-research-gather's description used to read "…without
// spending its own tokens browsing". That text is what catalog()
// surfaces to the host LLM as the basis for choosing a workflow, and a
// host read it, delegated a "load https://… into RAG" job, and
// reported success. The analyst role holds no fetch tool of any kind;
// nothing was loaded.
//
// The assertion is deliberately POSITIVE as well as negative: checking
// only that the old phrase is gone would pass against an empty
// description, which states nothing and helps no chooser. The
// description must say what the workflow draws on AND that it has no
// network access.
func TestShippedResearchGather_DescriptionStatesNoNetwork(t *testing.T) {
	wf := loadShippedWorkflow(t, "companion-research-gather.md")
	desc := wf.Description
	if strings.TrimSpace(desc) == "" {
		t.Fatal("companion-research-gather has no description at all")
	}
	lower := strings.ToLower(desc)

	// Positive: the constraint a chooser needs, stated.
	if !strings.Contains(lower, "no network access") {
		t.Errorf("description must state the no-network constraint verbatim "+
			"(\"No network access\"); got %q", desc)
	}
	// Positive: what it DOES draw on, so the entry is still useful.
	if !strings.Contains(lower, "memory") {
		t.Errorf("description must say it synthesises from project memory; got %q", desc)
	}
	if !strings.Contains(lower, "artifact") {
		t.Errorf("description must say it reads staged input artifacts; got %q", desc)
	}
	// Positive: the blessed alternative path, named.
	if !strings.Contains(lower, "companion-rag-ingest") {
		t.Errorf("description must point at companion-rag-ingest for external pages; got %q", desc)
	}

	// Negative: the exact sentence that caused the incident.
	if strings.Contains(lower, "without spending its own tokens browsing") {
		t.Errorf("the browsing promise is back in the description: %q", desc)
	}
}

// TestShippedCompanionWorkflows_DerivedNetworkAccessPinned is the
// ONGOING protection §4.1 names, in place of the free-text lint that
// design considered and rejected. Layer 1 is a one-time correction;
// what stops a description re-acquiring a browsing promise is that
// catalog() publishes the DERIVED capability directly beneath it, so
// the promise is visibly contradicted.
//
// This pins what the derivation says for every shipped companion-*
// workflow — a decidable fact about tool grants, not a judgement about
// prose. The one "possible" entry is real and is the derivation being
// honest rather than convenient: companion-rag-ingest's rag-ingester
// role holds run_shell, which is not on the inert allow-list, so the
// guard leaves that workflow alone. That is the fail-safe direction
// §3 chose; it under-fires, it does not over-fire.
//
// A new companion workflow, or a role gaining/losing a tool, fails this
// test. That is the point: the person making the change decides
// deliberately what the catalogue should advertise.
func TestShippedCompanionWorkflows_DerivedNetworkAccessPinned(t *testing.T) {
	want := map[string]bool{ // workflow id -> network-INCAPABLE
		"companion-architectural-review": true,
		"companion-data-validation":      true,
		"companion-doc-review":           true,
		"companion-rag-ingest":           false, // rag-ingester holds run_shell
		"companion-report-summarize":     true,
		"companion-research-gather":      true, // the 2026-09-11 incident pair
		"companion-test-coverage-audit":  true,
	}

	reg := New()
	if err := reg.Load("../../configs"); err != nil {
		t.Fatalf("load shipped configs: %v", err)
	}
	swarm := reg.GetSwarm("companion-example-swarm")
	if swarm == nil {
		t.Skip("companion swarm template not shipped in this edition")
	}

	entries, err := os.ReadDir(shippedWorkflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", shippedWorkflowsDir, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "companion-") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".md")
		seen[id] = true
		wf := reg.GetWorkflow(id)
		if wf == nil {
			t.Errorf("%s did not load into the registry", e.Name())
			continue
		}
		expected, known := want[id]
		if !known {
			t.Errorf("new companion workflow %q is unpinned — decide what network_access "+
				"catalog() should publish for it and add it to this table", id)
			continue
		}
		if got := WorkflowIsNetworkIncapable(wf, swarm); got != expected {
			t.Errorf("%s: network-incapable = %v, want %v — a role's tool grants changed; "+
				"confirm the new catalogue value is true before re-pinning", id, got, expected)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("pinned workflow %q is no longer shipped — drop it from the table", id)
		}
	}
}
