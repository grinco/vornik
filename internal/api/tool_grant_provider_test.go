package api

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

// grant_step_tools on the agent MCP surface (registry design §10.1–§10.4).

type fakeGrantStore struct {
	recorded    []*persistence.ExecutionToolGrant
	current     *persistence.ExecutionToolGrant
	escalations int
	recordErr   error
}

func (f *fakeGrantStore) Record(_ context.Context, g *persistence.ExecutionToolGrant) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, g)
	return nil
}

func (f *fakeGrantStore) Current(context.Context, string, string) (*persistence.ExecutionToolGrant, error) {
	return f.current, nil
}

func (f *fakeGrantStore) EscalationCount(context.Context, string, string) (int, error) {
	return f.escalations, nil
}

type fakeExecLookup struct{ exec *persistence.Execution }

func (f *fakeExecLookup) GetByTaskID(context.Context, string) (*persistence.Execution, error) {
	return f.exec, nil
}

func grantProvider(t *testing.T, store *fakeGrantStore, ceiling []string) (*ToolGrantProvider, context.Context) {
	t.Helper()
	step := "research"
	p := &ToolGrantProvider{
		Grants:     store,
		Executions: &fakeExecLookup{exec: &persistence.Execution{ID: "exec1", CurrentStepID: &step}},
		Ceiling:    func(context.Context, string) []string { return ceiling },
	}
	ctx := context.WithValue(context.Background(), mcp.TaskIDHeaderKey{}, "task1")
	return p, ctx
}

const grantTool = "mcp__vornik__grant_step_tools"

func TestGrantProvider_AdvertisesOneSmallTool(t *testing.T) {
	p, _ := grantProvider(t, &fakeGrantStore{}, nil)
	got := p.Tools("p1")
	if len(got) != 1 {
		t.Fatalf("advertised %d tools, want exactly 1 — this feature exists to SHRINK the "+
			"advertised surface, so adding much to it is self-defeating", len(got))
	}
	if got[0].Function.Name != grantTool {
		t.Errorf("tool name = %q, want %q", got[0].Function.Name, grantTool)
	}
}

func TestGrantProvider_NilStoreAdvertisesNothing(t *testing.T) {
	var p *ToolGrantProvider
	if len(p.Tools("p1")) != 0 {
		t.Error("a nil provider advertised a tool")
	}
	if (&ToolGrantProvider{}).Owns(grantTool) {
		t.Error("an unwired provider claimed ownership; Execute would then fail instead of " +
			"the tool simply being absent")
	}
}

func TestGrantProvider_RecordsAcceptedGrant(t *testing.T) {
	store := &fakeGrantStore{}
	p, ctx := grantProvider(t, store, []string{"mcp__scraper__web_fetch", "mcp__broker__quote"})

	out, err := p.Execute(ctx, "p1", grantTool, `{"tools":["mcp__scraper__web_fetch"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(store.recorded) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(store.recorded))
	}
	row := store.recorded[0]
	if !row.Accepted || row.StepID != "research" || row.ExecutionID != "exec1" {
		t.Errorf("row = %+v, want accepted and scoped to exec1/research", row)
	}
	if row.CeilingHash == "" {
		t.Error("no ceiling_hash recorded; an audit reader cannot then tell 'never in the " +
			"ceiling' from 'the ceiling was tightened after the grant'")
	}
	if !strings.Contains(out, "research") {
		t.Errorf("result %q should name the step it scoped", out)
	}
}

// TestGrantProvider_RefusalIsRecordedButNotItemisedToTheAgent is the probing defence
// plus the audit requirement, together: the operator gets names, the agent does not.
func TestGrantProvider_RefusalIsRecordedButNotItemisedToTheAgent(t *testing.T) {
	store := &fakeGrantStore{}
	p, ctx := grantProvider(t, store, []string{"mcp__scraper__web_fetch"})

	_, err := p.Execute(ctx, "p1", grantTool, `{"tools":["mcp__broker__place_order"]}`)
	if err == nil {
		t.Fatal("a grant naming a tool outside the ceiling was accepted")
	}
	if strings.Contains(err.Error(), "place_order") || strings.Contains(err.Error(), "broker") {
		t.Errorf("agent-visible error %q names the refused tool — an injected prompt can then "+
			"enumerate the ceiling by probing", err)
	}
	if len(store.recorded) != 1 {
		t.Fatalf("a refused request must still be recorded — a rejected privilege request is "+
			"exactly what an audit trail is for; got %d rows", len(store.recorded))
	}
	row := store.recorded[0]
	if row.Accepted {
		t.Error("refused row marked accepted")
	}
	if len(row.RefusedTools) != 1 || row.RefusedTools[0] != "mcp__broker__place_order" {
		t.Errorf("audit row refused_tools = %v, want the offending name for the operator",
			row.RefusedTools)
	}
}

func TestGrantProvider_EscalationLimitEnforced(t *testing.T) {
	store := &fakeGrantStore{escalations: maxEscalationsPerStep}
	p, ctx := grantProvider(t, store, []string{"a"})

	_, err := p.Execute(ctx, "p1", grantTool, `{"tools":["a"],"escalation":true}`)
	if err == nil {
		t.Fatal("escalation past the limit was allowed; an injected prompt could force " +
			"unbounded audited cycles")
	}
	if len(store.recorded) != 0 {
		t.Error("a refused-at-the-limit escalation should not append another row — that would " +
			"be the very audit spam the limit prevents")
	}
	// A non-escalation grant is unaffected by the escalation budget.
	if _, err := p.Execute(ctx, "p1", grantTool, `{"tools":["a"]}`); err != nil {
		t.Errorf("a plain grant was blocked by the escalation limit: %v", err)
	}
}

// TestGrantProvider_UnrecordableGrantDoesNotSucceed: the advertise path reads the
// store, so a grant that failed to persist must not report success — the lead would
// otherwise believe it scoped a step that is still wide open.
func TestGrantProvider_UnrecordableGrantDoesNotSucceed(t *testing.T) {
	store := &fakeGrantStore{recordErr: context.DeadlineExceeded}
	p, ctx := grantProvider(t, store, []string{"a"})

	if _, err := p.Execute(ctx, "p1", grantTool, `{"tools":["a"]}`); err == nil {
		t.Error("reported success for a grant that was never recorded")
	}
}

func TestGrantProvider_RejectsEmptyAndUnscopedRequests(t *testing.T) {
	p, ctx := grantProvider(t, &fakeGrantStore{}, []string{"a"})

	if _, err := p.Execute(ctx, "p1", grantTool, `{"tools":[]}`); err == nil {
		t.Error("an empty grant was accepted; it would leave the step with no tools")
	}
	// No task context — an operator or UI caller is not a step.
	if _, err := p.Execute(context.Background(), "p1", grantTool, `{"tools":["a"]}`); err == nil {
		t.Error("a grant with no task context was accepted")
	}
}
