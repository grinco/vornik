package ui

// Regression tests for the 2026-06 security audit finding:
// AssistantSuggest had no project-scope check, so a request
// scoped to project A could ground the assistant on project B's
// private brief / sibling prompts and charge B's budget by
// supplying projectId=B in the form.
//
// Contract (mirrors idor_regression_test.go):
//   - A scoped key for a FOREIGN project must get the SAME 404 as
//     a non-existent project (existence not leaked), and the LLM
//     must NOT be called (no budget spend / no context disclosure).
//   - A key scoped to the project itself still works (gate isn't
//     always-fail).

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"vornik.io/vornik/internal/api"
)

// auditPostAssistant builds a scoped POST to the assistant handler
// with the given form body and project scope.
func auditPostAssistant(form url.Values, scopes ...string) *http.Request {
	req := postAssistant(form.Encode())
	ctx := api.ContextWithScopeForTesting(req.Context(), scopes...)
	return req.WithContext(ctx)
}

func auditAssistantForm() url.Values {
	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "")
	return form
}

// TestAssistantSuggest_ScopedKeyCannotGroundForeignProject: a key
// scoped to project "other" requests the assistant for project
// "demo". The handler must 404 before any LLM call so neither
// demo's private brief/sibling prompts leak nor its budget is
// charged.
func TestAssistantSuggest_ScopedKeyCannotGroundForeignProject(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "should never be produced"}
	server := newAssistantServer(t, root, llm)

	req := auditPostAssistant(auditAssistantForm(), "other")
	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped assistant IDOR: status=%d, want 404 (existence-not-leaked)", rec.Code)
	}
	if llm.Calls != 0 {
		t.Errorf("scoped assistant IDOR: LLM called %d times against foreign project — context disclosed / budget charged", llm.Calls)
	}
}

// TestAssistantSuggest_SameProjectScopeStillWorks: the gate isn't
// always-fail — a key scoped to "demo" still reaches the LLM.
func TestAssistantSuggest_SameProjectScopeStillWorks(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "Here is a draft."}
	server := newAssistantServer(t, root, llm)

	req := auditPostAssistant(auditAssistantForm(), "demo")
	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("same-project scope: status=%d, want 200; scope gate is over-blocking", rec.Code)
	}
	if llm.Calls != 1 {
		t.Errorf("same-project scope: LLM called %d times, want 1", llm.Calls)
	}
}
