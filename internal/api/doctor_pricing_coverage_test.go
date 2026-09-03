package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkPricingCoverage walked role.Model and nothing else, then reported
// "all N models have pricing entries". Verified verbatim on the live daemon
// 2026-09-02: `OK pricing_coverage — all 11 models have pricing entries`, while
// the deployment references 28 ids. An unpriced model does not fail — it
// silently inherits default{1.00, 3.00} — so the check that exists to catch a
// fabricated rate was reporting OK.
//
// Design https://docs.vornik.io

// pricingFixture writes a config tree plus a pricing table and returns a doctor
// wired to both. `priced` lists the ids that HAVE entries.
func pricingFixture(t *testing.T, priced []string, files map[string]string, refs map[string]string) *DoctorHandlers {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	var b strings.Builder
	b.WriteString("models:\n")
	for _, m := range priced {
		b.WriteString("  \"" + m + "\": { input: 0.10, output: 0.30 }\n")
	}
	b.WriteString("default: { input: 1.00, output: 3.00 }\n")
	pricingPath := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(pricingPath, []byte(b.String()), 0o600))

	return &DoctorHandlers{configDir: dir, pricingPath: pricingPath, modelRefs: refs}
}

// swarmWith builds a swarm fixture with an explicit model and optional fallback.
func swarmWith(model, fallback string) map[string]string {
	role := "  - name: \"tester\"\n    runtime: { image: \"vornik-agent:latest\" }\n    model: \"" + model + "\"\n"
	if fallback != "" {
		role += "    modelFallback: \"" + fallback + "\"\n"
	}
	return map[string]string{
		"swarms/s.md":     "---\nswarmId: \"s\"\nroles:\n" + role + "---\n",
		"workflows/w.md":  "---\nworkflowId: \"w\"\nentrypoint: \"test\"\nsteps:\n  test:\n    type: \"agent\"\n    role: \"tester\"\n    prompt: \"x\"\nterminals:\n  done: { status: \"COMPLETED\" }\n---\n",
		"projects/p.yaml": "projectId: \"p\"\nswarmId: \"s\"\ndefaultWorkflowId: \"w\"\n",
	}
}

// TestPricingCoverage_ChecksModelFallback — design test 1.
//
// A model referenced ONLY as role.ModelFallback was never examined. Fails
// before the fix: the field is not read, so the check reports OK.
func TestPricingCoverage_ChecksModelFallback(t *testing.T) {
	h := pricingFixture(t, []string{"primary-model"}, swarmWith("primary-model", "unpriced-fallback"), nil)
	got := h.checkPricingCoverage()

	assert.Equal(t, "WARNING", got.Status,
		"a model referenced only as a fallback still bills at the default rate when unpriced")
	assert.Contains(t, strings.Join(got.Items, " "), "unpriced-fallback")
}

// TestPricingCoverage_ChecksDaemonScopeSurfaces — design test 2.
//
// Daemon-scope ids are unreachable from configDir: the handler holds a snapshot
// of named fields, never the live *config.Config, because config.Load() calls
// flag.Parse() and panics at request time. Each surface is driven through the
// snapshot map in turn.
func TestPricingCoverage_ChecksDaemonScopeSurfaces(t *testing.T) {
	for _, surface := range []string{
		"chat.model",
		"memory.embedding_model",
		"chat.router.http.model",
		"narrator.model",
	} {
		t.Run(surface, func(t *testing.T) {
			h := pricingFixture(t, []string{"primary-model"},
				swarmWith("primary-model", ""),
				map[string]string{surface: "unpriced-daemon-model"})
			got := h.checkPricingCoverage()

			assert.Equal(t, "WARNING", got.Status)
			joined := strings.Join(got.Items, " ")
			assert.Contains(t, joined, "unpriced-daemon-model")
			assert.Contains(t, joined, surface,
				"the finding must name the SURFACE, or the operator has to go hunting for where the id came from")
		})
	}
}

// TestPricingCoverage_ChecksProjectJudgeModel — the project-scope surface,
// reachable via reg.ListProjects() which returns whole *Project structs.
func TestPricingCoverage_ChecksProjectJudgeModel(t *testing.T) {
	files := swarmWith("primary-model", "")
	files["projects/p.yaml"] = "projectId: \"p\"\nswarmId: \"s\"\ndefaultWorkflowId: \"w\"\n" +
		"hallucinationJudge:\n  model: \"unpriced-judge\"\n"

	h := pricingFixture(t, []string{"primary-model"}, files, nil)
	got := h.checkPricingCoverage()

	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, strings.Join(got.Items, " "), "unpriced-judge")
}

// TestPricingCoverage_MessageStatesItsScope — design test 3, and THE test that
// would have caught the original defect.
//
// The old message said "all N models have pricing entries" while N counted only
// role.Model. A message may not claim more scope than the check examined.
func TestPricingCoverage_MessageStatesItsScope(t *testing.T) {
	h := pricingFixture(t, []string{"primary-model", "the-fallback", "the-chat-model"},
		swarmWith("primary-model", "the-fallback"),
		map[string]string{"chat.model": "the-chat-model"})
	got := h.checkPricingCoverage()

	require.Equal(t, "OK", got.Status, "everything is priced: %s / %v", got.Message, got.Items)
	assert.NotContains(t, got.Message, "all models have",
		"an unqualified 'all models' is the claim that made the original check dishonest")
	assert.Contains(t, got.Message, "3",
		"the count must reflect every surface examined, not just role models")
	// C1: the message names what it looked at.
	for _, want := range []string{"role", "fallback"} {
		assert.Contains(t, strings.ToLower(got.Message), want,
			"the message must state the surfaces it covered")
	}
}

// TestPricingCoverage_FullyPricedIsOK — C5. A widened check that cries wolf gets
// muted, which returns us to a false assurance by another route.
func TestPricingCoverage_FullyPricedIsOK(t *testing.T) {
	h := pricingFixture(t,
		[]string{"primary-model", "the-fallback", "the-chat-model", "the-embed-model", "the-judge"},
		func() map[string]string {
			f := swarmWith("primary-model", "the-fallback")
			f["projects/p.yaml"] = "projectId: \"p\"\nswarmId: \"s\"\ndefaultWorkflowId: \"w\"\n" +
				"hallucinationJudge:\n  model: \"the-judge\"\n"
			return f
		}(),
		map[string]string{"chat.model": "the-chat-model", "memory.embedding_model": "the-embed-model"})

	got := h.checkPricingCoverage()
	assert.Equal(t, "OK", got.Status, "no false positives: %s / %v", got.Message, got.Items)
	assert.Empty(t, got.Items)
}

// TestPricingCoverage_DuplicateIdsCountOnce — design test 6. A model used as
// both a role model and a daemon default is one gap, not two.
func TestPricingCoverage_DuplicateIdsCountOnce(t *testing.T) {
	h := pricingFixture(t, nil,
		swarmWith("shared-model", ""),
		map[string]string{"chat.model": "shared-model"})
	got := h.checkPricingCoverage()

	assert.Equal(t, "WARNING", got.Status)
	assert.Len(t, got.Items, 1, "one unpriced id referenced twice is one finding: %v", got.Items)
	assert.Contains(t, got.Items[0], "shared-model")
}

// TestPricingCoverage_EmptyModelRefsAreSkipped — an unset optional surface is
// not a finding. Without this the check warns on every deployment that has not
// configured a narrator.
func TestPricingCoverage_EmptyModelRefsAreSkipped(t *testing.T) {
	h := pricingFixture(t, []string{"primary-model"},
		swarmWith("primary-model", ""),
		map[string]string{"narrator.model": "", "instinct.model": "   "})
	got := h.checkPricingCoverage()
	assert.Equal(t, "OK", got.Status, "unset surfaces are not gaps: %s / %v", got.Message, got.Items)
}
