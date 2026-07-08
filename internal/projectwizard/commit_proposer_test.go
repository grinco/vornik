package projectwizard

import (
	"context"
	"testing"
)

// fakeProposer records the files it was asked to propose and returns a
// canned proposal id + review URL.
type fakeProposer struct {
	calls      []proposeCall
	proposalID string
	url        string
	err        error
}

type proposeCall struct {
	projectID string
	files     map[string]string
}

func (f *fakeProposer) ProposeScaffold(_ context.Context, projectID string, files map[string]string) (string, string, error) {
	f.calls = append(f.calls, proposeCall{projectID: projectID, files: files})
	if f.err != nil {
		return "", "", f.err
	}
	return f.proposalID, f.url, nil
}

// TestCommit_RoutesThroughProposer is the core reroute test: when a
// ScaffoldProposer is wired, Commit files a proposal (does NOT write via the
// ProjectWriter) and returns the proposal id + review URL.
func TestCommit_RoutesThroughProposer(t *testing.T) {
	w, store, _ := newWizardForTest()
	writer := &capturingWriter{}
	prop := &fakeProposer{proposalID: "cpp_abc123", url: "/ui/admin/control-plane?section=proposals"}
	w.Writer = writer // present but must NOT be used
	w.Proposer = prop
	w.Validator = RegistryValidator{}

	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	result, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(prop.calls) != 1 {
		t.Fatalf("proposer called %d times, want 1", len(prop.calls))
	}
	if len(writer.calls) != 0 {
		t.Errorf("writer must NOT be used when a proposer is wired; got %d write(s)", len(writer.calls))
	}
	if result.ProposalID != "cpp_abc123" {
		t.Errorf("ProposalID = %q, want cpp_abc123", result.ProposalID)
	}
	if result.ProjectID != "test-project" {
		t.Errorf("ProjectID = %q, want test-project", result.ProjectID)
	}
	if result.URL != prop.url {
		t.Errorf("URL = %q, want %q (proposal review page)", result.URL, prop.url)
	}
	// The proposer received the project's YAML under its configs-relative key.
	files := prop.calls[0].files
	if _, ok := files[projectYAMLKey("test-project")]; !ok {
		t.Errorf("proposer files missing %q; got keys %v", projectYAMLKey("test-project"), keysOf(files))
	}
}

// TestCommit_ProposerTakesPrecedenceOverWriter pins that even the single-file
// (no-template) path goes through the proposer when one is wired.
func TestCommit_ProposerIdempotentReclickURL(t *testing.T) {
	w, store, _ := newWizardForTest()
	prop := &fakeProposer{proposalID: "cpp_x", url: "/ui/admin/control-plane?section=proposals&status=DRAFT"}
	w.Proposer = prop
	w.Validator = RegistryValidator{}

	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	if _, err := w.Commit(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	// Re-click: session is now committed. On the proposer path the project
	// doesn't exist yet, so the URL must point at the proposals list, not a
	// (404-ing) project setup page.
	res2, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("re-click commit: %v", err)
	}
	if res2.URL != controlPlaneProposalsURL {
		t.Errorf("re-click URL = %q, want proposals list %q", res2.URL, controlPlaneProposalsURL)
	}
	if len(prop.calls) != 1 {
		t.Errorf("re-click must be idempotent (no second proposal); proposer called %d times", len(prop.calls))
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
