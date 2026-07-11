package ui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProjectsNewWizard_RendersComposerPreviewTabs guards the NL
// Automation Composer's preview surface (task 1.2a, design §5.5): the
// Plan/Graph/YAML tab markup + the JS contract that drives them from
// env.bundle on a tier-3 turn. This is a server-rendered static
// template — the JS itself isn't executed here — so the assertions
// pin the markup/JS-contract shape, mirroring
// TestProjectsNewWizard_RendersCompositionSummaryPanel's approach for
// the v2 composition panel.
func TestProjectsNewWizard_RendersComposerPreviewTabs(t *testing.T) {
	srv := NewServer(WithWizardSessionLister(&stubWizardLister{}))
	rec := httptest.NewRecorder()
	srv.ProjectsNewWizard(rec, resumeReq("", "op_1"))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		// Tab strip + panels.
		`id="composer-preview"`,
		`id="composer-tab-plan"`,
		`id="composer-tab-graph"`,
		`id="composer-tab-yaml"`,
		`id="composer-panel-plan"`,
		`id="composer-panel-graph"`,
		`id="composer-panel-yaml"`,
		// Plan tab content: steps, ApprovalsBypassed (prominent),
		// Approvals, CostBand labelled an estimate.
		`id="composer-plan-steps"`,
		`id="composer-plan-approvals-bypassed"`,
		"Will proceed WITHOUT asking",
		`id="composer-plan-approvals"`,
		`id="composer-plan-costband"`,
		"estimate only",
		// Schedule-confirmation chip.
		`id="composer-schedule-chip"`,
		`id="composer-schedule-confirm-btn"`,
		"window.confirmComposerSchedule = async function",
		"/confirm-schedule",
		// Graph tab: fetches rendered SVG(s) from the server.
		`id="composer-graph-container"`,
		"/ui/projects/new/wizard/graph-preview",
		"function renderComposerGraphs",
		// YAML tab: read-only + the "open in editor after commit" note.
		`id="composer-yaml-content"`,
		"Read-only preview",
		"Open in editor after commit",
		// JS call sites wiring env.bundle -> the composer preview, and
		// the ready_to_commit gate applying on every turn shape.
		"function renderComposerPreview(bundle, ready)",
		"if (env.bundle) {",
		"renderComposerPreview(env.bundle, env.ready_to_commit);",
		"els.commit.disabled = !env.ready_to_commit;",
		"function hideComposerPreview()",
		// The commit button posts to the real endpoint for every turn
		// shape, including a bundle turn — task 1.2b's journaled commit
		// path lands on the server side; lastBundlePending only changes
		// the in-flight status copy client-side (never gates the POST).
		"lastBundlePending",
		"window.submitWizardCommit = async function",
		"/commit'",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered wizard page missing composer-preview wiring %q", want)
		}
	}

	// Plan is the default active tab (design §5.5): its button starts
	// aria-selected=true, Graph/YAML start false and their panels hidden.
	if !strings.Contains(body, `id="composer-tab-plan" role="tab" aria-selected="true"`) {
		t.Error("Plan tab must be the default active tab")
	}
	if !strings.Contains(body, `id="composer-panel-graph" role="tabpanel" aria-labelledby="composer-tab-graph" class="hidden"`) {
		t.Error("Graph panel must start hidden")
	}
	if !strings.Contains(body, `id="composer-panel-yaml" role="tabpanel" aria-labelledby="composer-tab-yaml" class="hidden"`) {
		t.Error("YAML panel must start hidden")
	}

	// The whole composer-preview panel starts hidden — an ordinary
	// tier-1/2 turn (no bundle) must never show it.
	if !strings.Contains(body, `id="composer-preview" class="hidden`) {
		t.Error("composer-preview panel must start hidden until a tier-3 bundle turn arrives")
	}

	// The YAML panel must not contain any editable input (textarea /
	// contenteditable) — it's read-only by construction (a <pre>). Slice
	// out just the YAML panel's own markup (there IS a <textarea>
	// elsewhere on the page — #wizard-input — so a whole-page check
	// would be meaningless).
	yamlPanelStart := strings.Index(body, `id="composer-panel-yaml"`)
	if yamlPanelStart < 0 {
		t.Fatal("composer-panel-yaml not found")
	}
	yamlPanelEnd := yamlPanelStart + strings.Index(body[yamlPanelStart:], `</div>`+"\n"+`                        </div>`)
	if yamlPanelEnd <= yamlPanelStart {
		yamlPanelEnd = len(body)
	}
	yamlPanel := body[yamlPanelStart:yamlPanelEnd]
	if strings.Contains(yamlPanel, "<textarea") || strings.Contains(yamlPanel, "contenteditable") {
		t.Error("YAML panel must be read-only: no <textarea>/contenteditable inside it")
	}
	if !strings.Contains(yamlPanel, "<pre") {
		t.Error("YAML panel must render its content in a <pre> (read-only by construction)")
	}

	// Composer preview must sit before the commit/cancel button row in
	// DOM order (same convention as the v2 composition panel).
	composerIdx := strings.Index(body, `id="composer-preview"`)
	commitIdx := strings.Index(body, `id="wizard-commit"`)
	if composerIdx < 0 || commitIdx < 0 || composerIdx > commitIdx {
		t.Error("composer preview must appear before the commit button in DOM order")
	}
}

// TestProjectsNewWizard_ExistingTierOneTwoWiringUnchanged is a
// regression guard: the legacy raw-proposal preview + v2 composition
// summary wiring (pre-composer) must still be present verbatim so a
// tier-1/2 turn renders exactly as before task 1.2a.
func TestProjectsNewWizard_ExistingTierOneTwoWiringUnchanged(t *testing.T) {
	srv := NewServer(WithWizardSessionLister(&stubWizardLister{}))
	rec := httptest.NewRecorder()
	srv.ProjectsNewWizard(rec, resumeReq("", "op_1"))
	body := rec.Body.String()

	for _, want := range []string{
		`id="wizard-preview"`,
		`id="wizard-composition"`,
		"function renderPreview(proposal, ready)",
		"function renderComposition(composition)",
		"renderComposition(env.composition);",
		"function hideComposerPreview()",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered wizard page missing pre-existing wiring %q", want)
		}
	}
}
