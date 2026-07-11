package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/registry"
)

// --- fakes ---

// fakeIntegrationProber returns a canned ProbeResult regardless of the
// candidate, mirroring integrations.fakeProber (unexported in that
// package) so ui handler tests never dial a real network.
type fakeIntegrationProber struct {
	result integrations.ProbeResult
	calls  int
	last   integrations.CandidateConfig
}

func (p *fakeIntegrationProber) Kind() string { return "" }
func (p *fakeIntegrationProber) Probe(_ context.Context, cand integrations.CandidateConfig) integrations.ProbeResult {
	p.calls++
	p.last = cand
	return p.result
}

// fakeRegistryWithProber substitutes kindID's Prober on the REAL catalog
// (integrations.Registry) — tests get the real Fields/SaveTarget shape
// (so Save's field-split/write path is genuinely exercised) with a
// network-free probe.
func fakeRegistryWithProber(kindID string, prober integrations.Prober) func(integrations.DialGuard) []integrations.IntegrationKind {
	return func(guard integrations.DialGuard) []integrations.IntegrationKind {
		kinds := integrations.Registry(guard)
		for i := range kinds {
			if kinds[i].ID == kindID {
				kinds[i].Prober = prober
			}
		}
		return kinds
	}
}

// withFakeIntegrationsRegistry substitutes the package-level registry seam
// for the duration of the test.
func withFakeIntegrationsRegistry(t *testing.T, fn func(integrations.DialGuard) []integrations.IntegrationKind) {
	t.Helper()
	orig := integrationsRegistryFunc
	integrationsRegistryFunc = fn
	t.Cleanup(func() { integrationsRegistryFunc = orig })
}

// fakeAssistantLLM is a canned AssistantLLM (mirrors the assistant.go
// interface) for the doc-helper tests.
type fakeAssistantLLM struct {
	result *AssistantResult
	err    error
	calls  int
}

func (f *fakeAssistantLLM) Complete(_ context.Context, _, _, _ string) (*AssistantResult, error) {
	f.calls++
	return f.result, f.err
}

// withProjectScope stamps req with a project-scoped context (mirrors what
// AuthMiddleware stamps for a project-scoped session/API key).
func withProjectScope(req *http.Request, projects ...string) *http.Request {
	return req.WithContext(api.ContextWithProjectScope(req.Context(), projects...))
}

func formRequest(path string, values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// integrationsProjectRegistry builds a *registry.Registry over the
// writeFormFixture project fixture ("form-demo") — reused from
// project_config_form_test.go so the email/slack/github_app save tests
// exercise the real transactional write+validate path.
func integrationsProjectRegistry(t *testing.T) (*registry.Registry, string) {
	t.Helper()
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg, root
}

// --- integrationsCaller (the shared scope-resolution helper, §6) ---

func TestIntegrationsCaller_UnscopedIsAdmin(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ui/integrations", nil)
	c := integrationsCaller(req)
	assert.True(t, c.IsAdmin)
	assert.Empty(t, c.ScopedProjectIDs)
}

func TestIntegrationsCaller_ScopedIsNotAdmin(t *testing.T) {
	req := withProjectScope(httptest.NewRequest(http.MethodGet, "/ui/integrations", nil), "form-demo")
	c := integrationsCaller(req)
	assert.False(t, c.IsAdmin)
	assert.Equal(t, []string{"form-demo"}, c.ScopedProjectIDs)
}

// --- GET /integrations (catalog scope-filtering) ---

func TestIntegrationsCatalog_AdminSeesDaemonAndProjectTiles(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	s := NewServer(WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/ui/integrations", nil)
	rec := httptest.NewRecorder()
	s.IntegrationsCatalog(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Telegram", "admin must see the daemon-scope Telegram tile")
	assert.Contains(t, body, "Email", "admin must see project-scope tiles too")
	assert.Contains(t, body, "form-demo", "project-scope tiles carry a project badge")
	// Regression (2026-07-10 MCP-kind removal): the catalog must not grow an
	// MCP tile back — the control-plane hub's MCP tab is the management
	// surface (see integrations.Registry's doc).
	assert.NotContains(t, body, "MCP tool server", "MCP management belongs to the control-plane hub, not the Integrations Hub")
}

func TestIntegrationsCatalog_ScopedCallerHidesDaemonTiles(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	s := NewServer(WithProjectRegistry(reg))

	req := withProjectScope(httptest.NewRequest(http.MethodGet, "/ui/integrations", nil), "form-demo")
	rec := httptest.NewRecorder()
	s.IntegrationsCatalog(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "Telegram", "daemon-scope kinds must be hidden entirely, not merely disabled")
	assert.Contains(t, body, "Email", "the caller's own project-scope kinds must still render")
}

func TestIntegrationsCatalog_RejectsNonGET(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/ui/integrations", nil)
	rec := httptest.NewRecorder()
	s.IntegrationsCatalog(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// --- GET /integrations/{kind} (guided form + scope gates) ---

func TestIntegrationForm_DaemonKindForbiddenForScopedCaller(t *testing.T) {
	s := NewServer()
	req := withProjectScope(httptest.NewRequest(http.MethodGet, "/ui/integrations/telegram", nil), "form-demo")
	rec := httptest.NewRecorder()
	s.IntegrationForm(rec, req, "telegram")
	assert.Equal(t, http.StatusForbidden, rec.Code, "daemon-scope kind route must 403 for a project-scoped caller")
}

func TestIntegrationForm_DaemonKindOKForAdmin(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/integrations/telegram", nil)
	rec := httptest.NewRecorder()
	s.IntegrationForm(rec, req, "telegram")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Bot token")
}

func TestIntegrationForm_UnknownKind404s(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/integrations/nope", nil)
	rec := httptest.NewRecorder()
	s.IntegrationForm(rec, req, "nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestIntegrationForm_ProjectKindScopeGate(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	s := NewServer(WithProjectRegistry(reg))

	// Scoped to form-demo, but targeting a DIFFERENT project explicitly: 403.
	other := withProjectScope(httptest.NewRequest(http.MethodGet, "/ui/integrations/email?project=someone-elses-project", nil), "form-demo")
	rec := httptest.NewRecorder()
	s.IntegrationForm(rec, other, "email")
	assert.Equal(t, http.StatusForbidden, rec.Code, "a scoped caller must not reach another project's guided form")

	// Scoped to form-demo, targeting form-demo explicitly: 200.
	own := withProjectScope(httptest.NewRequest(http.MethodGet, "/ui/integrations/email?project=form-demo", nil), "form-demo")
	rec2 := httptest.NewRecorder()
	s.IntegrationForm(rec2, own, "email")
	require.Equal(t, http.StatusOK, rec2.Code, "body=%s", rec2.Body.String())

	// Scoped to form-demo, no ?project= — defaults to the sole scoped project.
	implicit := withProjectScope(httptest.NewRequest(http.MethodGet, "/ui/integrations/email", nil), "form-demo")
	rec3 := httptest.NewRecorder()
	s.IntegrationForm(rec3, implicit, "email")
	require.Equal(t, http.StatusOK, rec3.Code, "body=%s", rec3.Body.String())
}

func TestIntegrationForm_AdminWithNoProjectSeesPicker(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	s := NewServer(WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/ui/integrations/email", nil)
	rec := httptest.NewRecorder()
	s.IntegrationForm(rec, req, "email")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "form-demo")
	assert.Contains(t, rec.Body.String(), "Choose a project")
}

// TestIntegrationForm_SecretNeverRoundTrips is the design §6 masked-secret
// guard: a configured daemon-scope secret (the live, loader-expanded
// value sitting in cfg.Telegram.BotToken) must never appear literally in
// the rendered HTML — only an "already set" indicator.
func TestIntegrationForm_SecretNeverRoundTrips(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "1234567890:AATopSecretTokenValueMustNeverAppear"
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))

	req := httptest.NewRequest(http.MethodGet, "/ui/integrations/telegram", nil)
	rec := httptest.NewRecorder()
	s.IntegrationForm(rec, req, "telegram")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "AATopSecretTokenValueMustNeverAppear", "the live secret literal must never round-trip to the browser")
	assert.Contains(t, body, "already set", "a configured secret must render the masked indicator")
}

// --- POST /integrations/{kind}/probe ---

func TestIntegrationProbe_RendersEachOutcome(t *testing.T) {
	cases := []struct {
		name    string
		outcome integrations.Outcome
		want    string
	}{
		{"ok", integrations.OutcomeOK, "Connected"},
		{"fail", integrations.OutcomeFail, "Invalid — the provider rejected this"},
		{"error", integrations.OutcomeError, "Couldn't reach the provider — try again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prober := &fakeIntegrationProber{result: integrations.ProbeResult{
				Outcome: tc.outcome, OK: tc.outcome == integrations.OutcomeOK, Summary: "probe-summary-" + tc.name,
			}}
			withFakeIntegrationsRegistry(t, fakeRegistryWithProber("telegram", prober))

			s := NewServer()
			req := formRequest("/ui/integrations/telegram/probe", url.Values{"bot_token": {"whatever-token"}})
			rec := httptest.NewRecorder()
			s.IntegrationProbe(rec, req, "telegram")

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.want)
			assert.Equal(t, 1, prober.calls, "probe must never write; it should call the prober exactly once")
		})
	}
}

func TestIntegrationProbe_NeverWrites(t *testing.T) {
	reg, root := integrationsProjectRegistry(t)
	before, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)

	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK, Summary: "ok"}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("email", prober))

	s := NewServer(WithProjectRegistry(reg))
	req := formRequest("/ui/integrations/email/probe", url.Values{
		"project": {"form-demo"}, "imap_host": {"imap.example.com"},
	})
	rec := httptest.NewRecorder()
	s.IntegrationProbe(rec, req, "email")

	require.Equal(t, http.StatusOK, rec.Code)
	after, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "POST /probe must never write config")
}

func TestIntegrationProbe_DaemonKindForbiddenForScopedCaller(t *testing.T) {
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("telegram", prober))

	s := NewServer()
	req := withProjectScope(formRequest("/ui/integrations/telegram/probe", url.Values{"bot_token": {"x"}}), "form-demo")
	rec := httptest.NewRecorder()
	s.IntegrationProbe(rec, req, "telegram")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Zero(t, prober.calls, "a forbidden caller must never reach the prober")
}

// --- POST /integrations/{kind}/save ---

func TestIntegrationSave_RefusesOnProbeFail(t *testing.T) {
	reg, root := integrationsProjectRegistry(t)
	before, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)

	prober := &fakeIntegrationProber{result: integrations.ProbeResult{Outcome: integrations.OutcomeFail, Summary: "bad credentials"}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("email", prober))

	s := NewServer(WithProjectRegistry(reg))
	form := url.Values{
		"project":   {"form-demo"},
		"imap_host": {"imap.example.com"}, "imap_username": {"a@example.com"}, "imap_password_env": {"imap-secret-value-1"},
		"smtp_host": {"smtp.example.com"}, "smtp_username": {"a@example.com"}, "smtp_password_env": {"smtp-secret-value-1"},
		"from_address": {"a@example.com"},
	}
	req := formRequest("/ui/integrations/email/save", form)
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "email")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Invalid")

	after, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a failing probe must refuse the write (design §5.4 step 1)")

	_, cached := s.cachedIntegrationProbe("email", "form-demo")
	assert.False(t, cached, "an unsaved (refused) probe must not populate the cached tile store")
}

func TestIntegrationSave_SucceedsWritesConfigAndCachesProbe(t *testing.T) {
	reg, root := integrationsProjectRegistry(t)

	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK, Summary: "mailbox ready"}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("email", prober))

	s := NewServer(WithProjectRegistry(reg))
	form := url.Values{
		"project":   {"form-demo"},
		"imap_host": {"imap.example.com"}, "imap_username": {"a@example.com"}, "imap_password_env": {"imap-secret-value-2"},
		"smtp_host": {"smtp.example.com"}, "smtp_username": {"a@example.com"}, "smtp_password_env": {"smtp-secret-value-2"},
		"from_address": {"a@example.com"},
	}
	req := formRequest("/ui/integrations/email/save", form)
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "email")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Saved")

	written, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(written), "imap.example.com")

	entry, ok := s.cachedIntegrationProbe("email", "form-demo")
	require.True(t, ok, "a successful save must populate the cached probe store")
	assert.Equal(t, integrations.OutcomeOK, entry.Result.Outcome)
	assert.Equal(t, 1, prober.calls)
}

func TestIntegrationSave_ForbiddenForOutOfScopeCaller(t *testing.T) {
	reg, root := integrationsProjectRegistry(t)
	before, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)

	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("email", prober))

	s := NewServer(WithProjectRegistry(reg))
	form := url.Values{"project": {"form-demo"}, "imap_host": {"imap.example.com"}}
	req := withProjectScope(formRequest("/ui/integrations/email/save", form), "some-other-project")
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "email")

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Zero(t, prober.calls, "an out-of-scope save must never reach Save/Probe at all")
	after, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// --- POST /integrations/{kind}/recheck ---

func TestIntegrationRecheck_SwapsTileAndCachesResult(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)

	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK, Summary: "reconnected-ok"}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("slack", prober))

	s := NewServer(WithProjectRegistry(reg))
	req := formRequest("/ui/integrations/slack/recheck", url.Values{"project": {"form-demo"}})
	rec := httptest.NewRecorder()
	s.IntegrationRecheck(rec, req, "slack")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "reconnected-ok")
	assert.Contains(t, body, "Connected")
	assert.Equal(t, 1, prober.calls)

	entry, ok := s.cachedIntegrationProbe("slack", "form-demo")
	require.True(t, ok)
	assert.Equal(t, integrations.OutcomeOK, entry.Result.Outcome)
}

func TestIntegrationRecheck_ForbiddenForScopedCallerOutOfProject(t *testing.T) {
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("slack", prober))

	s := NewServer()
	req := withProjectScope(formRequest("/ui/integrations/slack/recheck", url.Values{"project": {"not-mine"}}), "form-demo")
	rec := httptest.NewRecorder()
	s.IntegrationRecheck(rec, req, "slack")

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Zero(t, prober.calls)
}

// --- POST /integrations/{kind}/assist ---

func TestIntegrationAssist_FallsBackToDocHintWhenLLMNil(t *testing.T) {
	s := NewServer() // no WithAssistantLLM
	req := formRequest("/ui/integrations/telegram/assist", url.Values{"field": {"bot_token"}})
	rec := httptest.NewRecorder()
	s.IntegrationAssist(rec, req, "telegram")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "BotFather", "must fall back to the field's static DocHint")
}

func TestIntegrationAssist_FallsBackOnLLMError(t *testing.T) {
	fake := &fakeAssistantLLM{err: assertError("boom")}
	s := NewServer(WithAssistantLLM(fake))
	req := formRequest("/ui/integrations/telegram/assist", url.Values{"field": {"bot_token"}})
	rec := httptest.NewRecorder()
	s.IntegrationAssist(rec, req, "telegram")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "BotFather")
	assert.Equal(t, 1, fake.calls)
}

func TestIntegrationAssist_CallsLLMWhenWired(t *testing.T) {
	fake := &fakeAssistantLLM{result: &AssistantResult{Text: "Look under Slack app settings."}}
	s := NewServer(WithAssistantLLM(fake))
	req := formRequest("/ui/integrations/slack/assist", url.Values{"field": {"team_id"}})
	rec := httptest.NewRecorder()
	s.IntegrationAssist(rec, req, "slack")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Look under Slack app settings.")
	assert.Equal(t, 1, fake.calls)
}

func TestIntegrationAssist_UnknownFieldBadRequest(t *testing.T) {
	s := NewServer()
	req := formRequest("/ui/integrations/telegram/assist", url.Values{"field": {"nope"}})
	rec := httptest.NewRecorder()
	s.IntegrationAssist(rec, req, "telegram")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestIntegrationAssist_ForbiddenForOutOfScopeCaller: the doc-helper is a
// write-adjacent route too (it echoes DocURL/field text back through an
// LLM prompt) — a scoped caller must not reach it for a daemon-scope kind
// (design §6), same gate every other route enforces.
func TestIntegrationAssist_ForbiddenForOutOfScopeCaller(t *testing.T) {
	s := NewServer()
	req := withProjectScope(formRequest("/ui/integrations/telegram/assist", url.Values{"field": {"bot_token"}}), "form-demo")
	rec := httptest.NewRecorder()
	s.IntegrationAssist(rec, req, "telegram")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// --- integrationsRouter dispatch ---

func TestIntegrationsRouter_Dispatch(t *testing.T) {
	s := NewServer()

	// Bare kind -> form.
	req := httptest.NewRequest(http.MethodGet, "/integrations/telegram", nil)
	rec := httptest.NewRecorder()
	s.integrationsRouter(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Unknown action -> 404.
	req2 := httptest.NewRequest(http.MethodPost, "/integrations/telegram/nonsense", nil)
	rec2 := httptest.NewRecorder()
	s.integrationsRouter(rec2, req2)
	assert.Equal(t, http.StatusNotFound, rec2.Code)

	// Bare "/integrations/" (trailing slash root) -> catalog.
	req3 := httptest.NewRequest(http.MethodGet, "/integrations/", nil)
	rec3 := httptest.NewRecorder()
	s.integrationsRouter(rec3, req3)
	assert.Equal(t, http.StatusOK, rec3.Code)
}

// TestIntegrationsRouter_DispatchesEveryAction exercises the router's
// probe/save/recheck/assist switch arms directly (TestIntegrationsRouter_
// Dispatch above only covers the bare-kind, unknown-action, and root
// cases) — each action must reach its handler, not 404.
func TestIntegrationsRouter_DispatchesEveryAction(t *testing.T) {
	// Save (even of a daemon-scope kind like telegram, which writes
	// config.yaml rather than a project file) needs a non-empty
	// configDir(), which only a wired project registry provides
	// (s.configDir's doc) — hence WithProjectRegistry here even though
	// this dispatch test targets telegram, not a project-scope kind.
	reg, _ := integrationsProjectRegistry(t)
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("telegram", prober))
	s := NewServer(WithProjectRegistry(reg))

	probeReq := formRequest("/integrations/telegram/probe", url.Values{"bot_token": {"x"}})
	rec := httptest.NewRecorder()
	s.integrationsRouter(rec, probeReq)
	require.Equal(t, http.StatusOK, rec.Code, "probe dispatch: body=%s", rec.Body.String())

	saveReq := formRequest("/integrations/telegram/save", url.Values{"bot_token": {"x"}})
	rec2 := httptest.NewRecorder()
	s.integrationsRouter(rec2, saveReq)
	require.Equal(t, http.StatusOK, rec2.Code, "save dispatch: body=%s", rec2.Body.String())

	recheckReq := formRequest("/integrations/telegram/recheck", url.Values{})
	rec3 := httptest.NewRecorder()
	s.integrationsRouter(rec3, recheckReq)
	require.Equal(t, http.StatusOK, rec3.Code, "recheck dispatch: body=%s", rec3.Body.String())

	assistReq := formRequest("/integrations/telegram/assist", url.Values{"field": {"bot_token"}})
	rec4 := httptest.NewRecorder()
	s.integrationsRouter(rec4, assistReq)
	require.Equal(t, http.StatusOK, rec4.Code, "assist dispatch: body=%s", rec4.Body.String())
}

// --- shared guard-clause coverage: method/kind/form across every write
// handler (design: same pattern in IntegrationProbe/Save/Recheck/Assist) ---
//
// badFormRequest (malformed %-escape body -> r.ParseForm() error) is
// already defined in schema_config_save_test.go — reused here rather than
// redeclared.

func TestIntegrationProbe_MethodNotAllowed(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/integrations/telegram/probe", nil)
	rec := httptest.NewRecorder()
	s.IntegrationProbe(rec, req, "telegram")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestIntegrationProbe_UnknownKind404s(t *testing.T) {
	s := NewServer()
	req := formRequest("/ui/integrations/nope/probe", url.Values{})
	rec := httptest.NewRecorder()
	s.IntegrationProbe(rec, req, "nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestIntegrationProbe_BadFormRejected(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.IntegrationProbe(rec, badFormRequest("/ui/integrations/telegram/probe"), "telegram")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestIntegrationProbe_NoProberServiceUnavailable(t *testing.T) {
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("telegram", nil))
	s := NewServer()
	req := formRequest("/ui/integrations/telegram/probe", url.Values{"bot_token": {"x"}})
	rec := httptest.NewRecorder()
	s.IntegrationProbe(rec, req, "telegram")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestIntegrationSave_MethodNotAllowed(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/integrations/telegram/save", nil)
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "telegram")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestIntegrationSave_UnknownKind404s(t *testing.T) {
	s := NewServer()
	req := formRequest("/ui/integrations/nope/save", url.Values{})
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestIntegrationSave_UnwritableKindServiceUnavailable injects a fake kind
// whose ID has no integrations.SaveTargetForKind entry — every REAL
// catalog kind is wired (task 5.2/5.2b), so this branch can only be
// exercised with a synthetic kind, mirroring how the design calls out
// SaveTargetForKind's ok=false path as a genuine (if currently unreachable
// in production) guard.
func TestIntegrationSave_UnwritableKindServiceUnavailable(t *testing.T) {
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
	withFakeIntegrationsRegistry(t, func(guard integrations.DialGuard) []integrations.IntegrationKind {
		kinds := integrations.Registry(guard)
		kinds = append(kinds, integrations.IntegrationKind{
			ID: "not-wired-yet", DisplayName: "Not Wired", Scope: integrations.ScopeDaemon, Prober: prober,
		})
		return kinds
	})
	s := NewServer()
	req := formRequest("/ui/integrations/not-wired-yet/save", url.Values{})
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "not-wired-yet")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestIntegrationSave_BadFormRejected(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, badFormRequest("/ui/integrations/telegram/save"), "telegram")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestIntegrationSave_NoConfigDirServiceUnavailable covers the "registry
// config directory is not configured" 503 — s.configDir() is "" whenever
// no project registry is wired (design: the handler must refuse cleanly
// rather than let Save fail on an empty ConfigDir).
func TestIntegrationSave_NoConfigDirServiceUnavailable(t *testing.T) {
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("email", prober))
	s := NewServer() // no WithProjectRegistry -> configDir() == ""
	req := formRequest("/ui/integrations/email/save", url.Values{"project": {"form-demo"}})
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "email")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestIntegrationRecheck_MethodNotAllowed(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/integrations/telegram/recheck", nil)
	rec := httptest.NewRecorder()
	s.IntegrationRecheck(rec, req, "telegram")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestIntegrationRecheck_UnknownKind404s(t *testing.T) {
	s := NewServer()
	req := formRequest("/ui/integrations/nope/recheck", url.Values{})
	rec := httptest.NewRecorder()
	s.IntegrationRecheck(rec, req, "nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestIntegrationRecheck_BadFormRejected(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.IntegrationRecheck(rec, badFormRequest("/ui/integrations/telegram/recheck"), "telegram")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestIntegrationRecheck_NoProberServiceUnavailable(t *testing.T) {
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("telegram", nil))
	s := NewServer()
	req := formRequest("/ui/integrations/telegram/recheck", url.Values{})
	rec := httptest.NewRecorder()
	s.IntegrationRecheck(rec, req, "telegram")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestIntegrationRecheck_ReadsCurrentValuesPerKind exercises
// currentIntegrationValues' telegram/email/github_app branches (the
// existing recheck test only covers slack) so a re-check genuinely
// re-probes the SAVED server-side config, not an empty candidate.
func TestIntegrationRecheck_ReadsCurrentValuesPerKind(t *testing.T) {
	t.Run("telegram", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Telegram.BotToken = "the-real-bot-token"
		prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
		withFakeIntegrationsRegistry(t, fakeRegistryWithProber("telegram", prober))
		s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))

		req := formRequest("/ui/integrations/telegram/recheck", url.Values{})
		rec := httptest.NewRecorder()
		s.IntegrationRecheck(rec, req, "telegram")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "the-real-bot-token", prober.last.Values["bot_token"])
	})

	t.Run("email", func(t *testing.T) {
		reg, _ := integrationsProjectRegistry(t)
		prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
		withFakeIntegrationsRegistry(t, fakeRegistryWithProber("email", prober))
		s := NewServer(WithProjectRegistry(reg))

		req := formRequest("/ui/integrations/email/recheck", url.Values{"project": {"form-demo"}})
		rec := httptest.NewRecorder()
		s.IntegrationRecheck(rec, req, "email")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, prober.last.Values, "imap_host")
	})

	t.Run("github_app_with_private_key_file", func(t *testing.T) {
		root := writeFormFixture(t)
		server, _ := formServer(t, root)

		keyPath := filepath.Join(t.TempDir(), "gh-app-key.pem")
		require.NoError(t, os.WriteFile(keyPath, []byte("-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----\n"), 0o600))

		form := baselineFormValues()
		form.Set("githubApp_appID", "1")
		form.Set("githubApp_installationID", "2")
		form.Set("githubApp_privateKeyPath", keyPath)
		form.Set("githubApp_webhookSecretEnv", "GH_HMAC")
		form.Set("githubApp_repoAllowlist", "acme/widgets")
		saveRec := httptest.NewRecorder()
		server.ProjectConfigFormSave(saveRec, postForm(form), "form-demo")
		require.Equal(t, http.StatusOK, saveRec.Code, "fixture save: body=%s", saveRec.Body.String())

		prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
		withFakeIntegrationsRegistry(t, fakeRegistryWithProber("github_app", prober))

		req := formRequest("/ui/integrations/github_app/recheck", url.Values{"project": {"form-demo"}})
		rec := httptest.NewRecorder()
		server.IntegrationRecheck(rec, req, "github_app")
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		assert.Contains(t, prober.last.Values["private_key_path"], "BEGIN PRIVATE KEY")
	})
}

func TestIntegrationAssist_MethodNotAllowed(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/integrations/telegram/assist", nil)
	rec := httptest.NewRecorder()
	s.IntegrationAssist(rec, req, "telegram")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestIntegrationAssist_UnknownKind404s(t *testing.T) {
	s := NewServer()
	req := formRequest("/ui/integrations/nope/assist", url.Values{"field": {"x"}})
	rec := httptest.NewRecorder()
	s.IntegrationAssist(rec, req, "nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestIntegrationAssist_BadFormRejected(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.IntegrationAssist(rec, badFormRequest("/ui/integrations/telegram/assist"), "telegram")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestIntegrationForm_MethodNotAllowed(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/ui/integrations/telegram", nil)
	rec := httptest.NewRecorder()
	s.IntegrationForm(rec, req, "telegram")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// --- IntegrationForm per-kind field prefill (secretFieldConfigured /
// nonSecretFieldValue switch arms) ---

func TestIntegrationForm_SlackAndGitHubAppRenderTheirFields(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	s := NewServer(WithProjectRegistry(reg))

	for _, kind := range []string{"slack", "github_app"} {
		t.Run(kind, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ui/integrations/"+kind+"?project=form-demo", nil)
			rec := httptest.NewRecorder()
			s.IntegrationForm(rec, req, kind)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// Regression (2026-07-10 MCP-kind removal): /ui/integrations/mcp must 404 —
// MCP tool servers are managed on the control-plane hub's MCP tab, and the
// hub's former guided MCP form (an inferior duplicate of that tab) is gone.
func TestIntegrationForm_MCPKindIsGone(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/integrations/mcp", nil)
	rec := httptest.NewRecorder()
	s.IntegrationForm(rec, req, "mcp")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestIntegrationForm_AdminUnknownProjectStillRenders covers the
// proj==nil branches inside secretFieldConfigured/nonSecretFieldValue: an
// admin may open a project-scope kind's form for a project ID that isn't
// (yet) in the registry (e.g. a typo'd or since-archived project) — the
// form must still render with everything simply unconfigured, not 500.
func TestIntegrationForm_AdminUnknownProjectStillRenders(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	s := NewServer(WithProjectRegistry(reg))

	for _, kind := range []string{"email", "slack", "github_app"} {
		t.Run(kind, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ui/integrations/"+kind+"?project=no-such-project", nil)
			rec := httptest.NewRecorder()
			s.IntegrationForm(rec, req, kind)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// --- integrationConfigured / integrationTile edge cases ---

func TestIntegrationConfigured_UnknownKindDefaultsFalse(t *testing.T) {
	s := NewServer()
	assert.False(t, s.integrationConfigured("not-a-real-kind", ""))
}

// TestIntegrationsCatalog_ConfiguredButNeverProbedShowsUnknown covers
// integrationTile's "unknown" status branch: a kind that IS configured
// (daemon-scope telegram bot token set) but has no cached probe result yet
// (never saved/rechecked this process) must render as "not checked yet",
// not "unconfigured".
func TestIntegrationsCatalog_ConfiguredButNeverProbedShowsUnknown(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "already-configured-token"
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))

	req := httptest.NewRequest(http.MethodGet, "/ui/integrations", nil)
	rec := httptest.NewRecorder()
	s.IntegrationsCatalog(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Not checked yet")
}

// --- integrationsReloader / integrationsReloadStatus optional-capability
// wiring (design: mirrors config_reload.go's boundedReloader pattern) ---

func TestIntegrationsReloader_WiredReloaderCallsThrough(t *testing.T) {
	reloader := &mockConfigReloader{}
	s := NewServer(WithConfigReloader(reloader))

	got := s.integrationsReloader()
	require.NotNil(t, got)
	require.NoError(t, got.Reload(context.Background()))
	assert.Equal(t, 1, reloader.calls, "the adapter must call through to the wrapped ConfigReloader.Reload")
}

func TestIntegrationsReloader_NilWhenUnwired(t *testing.T) {
	s := NewServer()
	assert.Nil(t, s.integrationsReloader())
}

func TestIntegrationsReloadStatus_WiredWhenReloaderImplementsStatus(t *testing.T) {
	s := NewServer(WithConfigReloader(statusReloader{status: config.ReloadStatus{}}))
	assert.NotNil(t, s.integrationsReloadStatus())
}

func TestIntegrationsReloadStatus_NilWhenReloaderLacksStatus(t *testing.T) {
	s := NewServer(WithConfigReloader(&mockConfigReloader{}))
	assert.Nil(t, s.integrationsReloadStatus())
}

// assertError is a tiny error helper so tests don't need to import "errors"
// just for one canned error.
type assertError string

func (e assertError) Error() string { return string(e) }

// erroringConfigReloader is a ConfigReloader whose Reload always fails —
// used to drive Save's step-5 "reload" failure path (SaveError.Step ==
// "reload") for the vornik_integration_save_total reload_failed test.
type erroringConfigReloader struct{ err error }

func (e erroringConfigReloader) Reload() error { return e.err }

// --- M2: integrationFormHref escapes projectID (companion review
// review-20260710-05a2 finding M2) ---

func TestIntegrationFormHref_EscapesProjectID(t *testing.T) {
	got := integrationFormHref("email", "a project/needs?escaping&here")
	assert.Equal(t, "/ui/integrations/email?project="+url.QueryEscape("a project/needs?escaping&here"), got)
	assert.NotContains(t, got, " ", "the raw space must not survive unescaped in the href")
}

func TestIntegrationFormHref_EmptyProjectIDOmitsQuery(t *testing.T) {
	assert.Equal(t, "/ui/integrations/telegram", integrationFormHref("telegram", ""))
}

func TestIntegrationTile_ShowsRecheckWhenConfigured(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "configured-token"
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))

	vm := s.integrationTile(integrations.IntegrationKind{ID: "telegram", DisplayName: "Telegram", Scope: integrations.ScopeDaemon}, "")
	assert.True(t, vm.Configured)
	assert.True(t, vm.CanRecheck)
}

func TestIntegrationTile_UnconfiguredNeverShowsRecheck(t *testing.T) {
	s := NewServer()
	vm := s.integrationTile(integrations.IntegrationKind{ID: "telegram", DisplayName: "Telegram", Scope: integrations.ScopeDaemon}, "")
	assert.False(t, vm.Configured)
	assert.False(t, vm.CanRecheck)
}

// --- task 3.4: the "Help me fix this" tile button (fix-it-doctor-design.md
// §5.5/§7 — hidden entirely, never shown-then-404) ---

func TestIntegrationTile_FixItHref_ShownWhenRedAndDoctorWired(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "configured-token"
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}), WithFixItDoctor(&stubUIFixItDoctor{}))
	s.storeIntegrationProbe("telegram", "", integrations.ProbeResult{Outcome: integrations.OutcomeFail})

	vm := s.integrationTile(integrations.IntegrationKind{ID: "telegram", DisplayName: "Telegram", Scope: integrations.ScopeDaemon}, "")
	assert.Equal(t, "fail", vm.Status)
	assert.Equal(t, "/ui/fixit/red_integration/telegram", vm.FixItHref)
}

func TestIntegrationTile_FixItHref_ShownWhenErrorOutcome(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "configured-token"
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}), WithFixItDoctor(&stubUIFixItDoctor{}))
	s.storeIntegrationProbe("telegram", "", integrations.ProbeResult{Outcome: integrations.OutcomeError})

	vm := s.integrationTile(integrations.IntegrationKind{ID: "telegram", DisplayName: "Telegram", Scope: integrations.ScopeDaemon}, "")
	assert.Equal(t, "error", vm.Status)
	assert.NotEmpty(t, vm.FixItHref)
}

func TestIntegrationTile_FixItHref_ProjectScopedIncludesQueryParam(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	s := NewServer(WithProjectRegistry(reg), WithFixItDoctor(&stubUIFixItDoctor{}))
	s.storeIntegrationProbe("slack", "form-demo", integrations.ProbeResult{Outcome: integrations.OutcomeFail})

	vm := s.integrationTile(integrations.IntegrationKind{ID: "slack", DisplayName: "Slack", Scope: integrations.ScopeProject}, "form-demo")
	assert.Equal(t, "/ui/fixit/red_integration/slack?project=form-demo", vm.FixItHref)
}

// TestIntegrationTile_FixItHref_HiddenWhenDoctorNotWired is the §7 "not
// shown-then-404" requirement: even a genuinely red tile must not offer
// the link when the Fix-It Doctor surface isn't configured on this
// deployment.
func TestIntegrationTile_FixItHref_HiddenWhenDoctorNotWired(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "configured-token"
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg})) // no WithFixItDoctor
	s.storeIntegrationProbe("telegram", "", integrations.ProbeResult{Outcome: integrations.OutcomeFail})

	vm := s.integrationTile(integrations.IntegrationKind{ID: "telegram", DisplayName: "Telegram", Scope: integrations.ScopeDaemon}, "")
	assert.Equal(t, "fail", vm.Status, "precondition: the tile must actually be red")
	assert.Empty(t, vm.FixItHref, "must be hidden when the doctor isn't wired, never a dead link")
}

// TestIntegrationTile_FixItHref_HiddenWhenOutcomeOK covers the design's
// "appears only when Outcome != ok" — a healthy tile never shows it.
func TestIntegrationTile_FixItHref_HiddenWhenOutcomeOK(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "configured-token"
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}), WithFixItDoctor(&stubUIFixItDoctor{}))
	s.storeIntegrationProbe("telegram", "", integrations.ProbeResult{Outcome: integrations.OutcomeOK})

	vm := s.integrationTile(integrations.IntegrationKind{ID: "telegram", DisplayName: "Telegram", Scope: integrations.ScopeDaemon}, "")
	assert.Equal(t, "ok", vm.Status)
	assert.Empty(t, vm.FixItHref)
}

// TestIntegrationTile_FixItHref_HiddenWhenNeverProbed covers "unknown" /
// "unconfigured" — no cached probe means nothing to ground a repair
// session on, so the button must stay hidden rather than link to a
// red_integration session with no data.
func TestIntegrationTile_FixItHref_HiddenWhenNeverProbed(t *testing.T) {
	s := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	vm := s.integrationTile(integrations.IntegrationKind{ID: "telegram", DisplayName: "Telegram", Scope: integrations.ScopeDaemon}, "")
	assert.Equal(t, "unconfigured", vm.Status)
	assert.Empty(t, vm.FixItHref)
}

// Regression (2026-07-10 MCP-kind removal): even with mcp.servers populated
// in config, the catalog renders no MCP tile — the kind is gone from the
// registry, not merely unconfigured.
func TestIntegrationsCatalog_NoMCPTileEvenWhenServersConfigured(t *testing.T) {
	cfg := &config.Config{}
	cfg.MCP.Servers = []config.MCPServerConfig{{Name: "broker", Transport: "stdio"}}
	cfg.Telegram.BotToken = "configured-token"
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))

	req := httptest.NewRequest(http.MethodGet, "/ui/integrations", nil)
	rec := httptest.NewRecorder()
	s.IntegrationsCatalog(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `id="tile-mcp"`,
		"MCP management belongs to the control-plane hub, not the Integrations Hub")
}

// --- allowed_hosts config → integrationsDialGuard wiring ---

func TestIntegrationsDialGuard_ReadsAllowedHostsFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Integrations.AllowedHosts = []string{"imap.internal.example.com", "mcp.lan"}
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))

	guard := s.integrationsDialGuard()
	assert.Equal(t, []string{"imap.internal.example.com", "mcp.lan"}, guard.AllowedHosts)
}

func TestIntegrationsDialGuard_NilConfigDefaultsToEmptyAllowlist(t *testing.T) {
	s := NewServer()
	guard := s.integrationsDialGuard()
	assert.Empty(t, guard.AllowedHosts)
}

func TestIntegrationsDialGuard_EmptyConfigAllowlistStillBlocksPrivate(t *testing.T) {
	cfg := &config.Config{}
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))
	guard := s.integrationsDialGuard()
	require.Empty(t, guard.AllowedHosts)

	client := guard.HTTPClient(2 * time.Second)
	_, err := client.Get("http://127.0.0.1:1/anything")
	require.Error(t, err, "an empty allowlist must still refuse a private-range destination")
	assert.Contains(t, err.Error(), "dial guard")
}

// --- I1: bounded probe cache (companion review review-20260710-05a2
// finding I1 — storeIntegrationProbe had no eviction) ---

func TestStoreIntegrationProbe_SweepsEntriesOlderThanTTL(t *testing.T) {
	s := NewServer()
	s.integrationProbes = map[string]integrationProbeCacheEntry{
		integrationProbeCacheKey("email", "stale-project"): {
			Result: integrations.ProbeResult{Outcome: integrations.OutcomeOK},
			At:     time.Now().Add(-(integrationProbeCacheTTL + time.Hour)),
		},
	}

	s.storeIntegrationProbe("slack", "fresh-project", integrations.ProbeResult{Outcome: integrations.OutcomeOK})

	_, staleStillThere := s.cachedIntegrationProbe("email", "stale-project")
	assert.False(t, staleStillThere, "an entry older than integrationProbeCacheTTL must be swept on the next write")
	_, freshThere := s.cachedIntegrationProbe("slack", "fresh-project")
	assert.True(t, freshThere)
}

func TestStoreIntegrationProbe_CapsMaxEntriesEvictingOldestFirst(t *testing.T) {
	s := NewServer()
	now := time.Now()
	const seeded = integrationProbeCacheMaxEntries + 5
	s.integrationProbes = make(map[string]integrationProbeCacheEntry, seeded)
	for i := 0; i < seeded; i++ {
		key := integrationProbeCacheKey(fmt.Sprintf("kind-%d", i), "project")
		s.integrationProbes[key] = integrationProbeCacheEntry{
			Result: integrations.ProbeResult{Outcome: integrations.OutcomeOK},
			// All strictly BEFORE now (kind-0 furthest in the past, kind-
			// (seeded-1) closest to now) so the entry storeIntegrationProbe
			// adds below — timestamped with the real time.Now() at that
			// call, necessarily later than every seeded entry — is
			// unambiguously the newest of the lot.
			At: now.Add(-time.Duration(seeded-i) * time.Millisecond),
		}
	}

	s.storeIntegrationProbe("newest-kind", "project", integrations.ProbeResult{Outcome: integrations.OutcomeOK})

	assert.LessOrEqual(t, len(s.integrationProbes), integrationProbeCacheMaxEntries, "the map must never exceed integrationProbeCacheMaxEntries")
	_, oldestStillThere := s.cachedIntegrationProbe("kind-0", "project")
	assert.False(t, oldestStillThere, "the oldest entries must be evicted first once the cap is exceeded")
	_, newestThere := s.cachedIntegrationProbe("newest-kind", "project")
	assert.True(t, newestThere, "the just-written entry must survive its own eviction pass")
}

// --- Metrics: vornik_integration_probe_total / vornik_integration_save_total ---

func TestIntegrationProbe_RecordsProbeMetricPerOutcome(t *testing.T) {
	cases := []struct {
		name    string
		outcome integrations.Outcome
	}{
		{"ok", integrations.OutcomeOK},
		{"fail", integrations.OutcomeFail},
		{"error", integrations.OutcomeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prober := &fakeIntegrationProber{result: integrations.ProbeResult{Outcome: tc.outcome, OK: tc.outcome == integrations.OutcomeOK}}
			withFakeIntegrationsRegistry(t, fakeRegistryWithProber("telegram", prober))

			reg := prometheus.NewRegistry()
			s := NewServer(WithIntegrationsMetrics(NewIntegrationsMetrics(reg)))
			req := formRequest("/ui/integrations/telegram/probe", url.Values{"bot_token": {"x"}})
			rec := httptest.NewRecorder()
			s.IntegrationProbe(rec, req, "telegram")

			require.Equal(t, http.StatusOK, rec.Code)
			got := testutil.ToFloat64(s.integrationsMetrics.ProbeTotal.WithLabelValues("telegram", string(tc.outcome)))
			assert.Equal(t, float64(1), got)
		})
	}
}

func TestIntegrationRecheck_RecordsProbeMetric(t *testing.T) {
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("slack", prober))
	reg, _ := integrationsProjectRegistry(t)

	metricsReg := prometheus.NewRegistry()
	s := NewServer(WithProjectRegistry(reg), WithIntegrationsMetrics(NewIntegrationsMetrics(metricsReg)))
	req := formRequest("/ui/integrations/slack/recheck", url.Values{"project": {"form-demo"}})
	rec := httptest.NewRecorder()
	s.IntegrationRecheck(rec, req, "slack")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := testutil.ToFloat64(s.integrationsMetrics.ProbeTotal.WithLabelValues("slack", "ok"))
	assert.Equal(t, float64(1), got)
}

func TestIntegrationSave_RecordsOkMetrics(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK, Summary: "mailbox ready"}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("email", prober))

	metricsReg := prometheus.NewRegistry()
	s := NewServer(WithProjectRegistry(reg), WithIntegrationsMetrics(NewIntegrationsMetrics(metricsReg)))
	form := url.Values{
		"project":   {"form-demo"},
		"imap_host": {"imap.example.com"}, "imap_username": {"a@example.com"}, "imap_password_env": {"imap-secret-metrics-ok"},
		"smtp_host": {"smtp.example.com"}, "smtp_username": {"a@example.com"}, "smtp_password_env": {"smtp-secret-metrics-ok"},
		"from_address": {"a@example.com"},
	}
	req := formRequest("/ui/integrations/email/save", form)
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "email")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, float64(1), testutil.ToFloat64(s.integrationsMetrics.SaveTotal.WithLabelValues("email", "ok")))
	assert.Equal(t, float64(1), testutil.ToFloat64(s.integrationsMetrics.ProbeTotal.WithLabelValues("email", "ok")))
}

func TestIntegrationSave_RecordsProbeFailedMetric(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{Outcome: integrations.OutcomeFail, Summary: "bad credentials"}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("email", prober))

	metricsReg := prometheus.NewRegistry()
	s := NewServer(WithProjectRegistry(reg), WithIntegrationsMetrics(NewIntegrationsMetrics(metricsReg)))
	form := url.Values{
		"project":   {"form-demo"},
		"imap_host": {"imap.example.com"}, "imap_username": {"a@example.com"}, "imap_password_env": {"imap-secret-metrics-fail"},
		"smtp_host": {"smtp.example.com"}, "smtp_username": {"a@example.com"}, "smtp_password_env": {"smtp-secret-metrics-fail"},
		"from_address": {"a@example.com"},
	}
	req := formRequest("/ui/integrations/email/save", form)
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "email")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, float64(1), testutil.ToFloat64(s.integrationsMetrics.SaveTotal.WithLabelValues("email", "probe_failed")))
	assert.Equal(t, float64(1), testutil.ToFloat64(s.integrationsMetrics.ProbeTotal.WithLabelValues("email", "fail")))
}

// TestIntegrationSave_RecordsWriteFailedMetric drives a Save failure that
// occurs BEFORE step 1's probe (an admin caller — bypasses the UI-layer
// scope gate — submitting a malformed project id that
// validateProjectIDForPath rejects), so result.Probe.Outcome stays "" and
// only the save_total write_failed label is expected.
func TestIntegrationSave_RecordsWriteFailedMetric(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	metricsReg := prometheus.NewRegistry()
	s := NewServer(WithProjectRegistry(reg), WithIntegrationsMetrics(NewIntegrationsMetrics(metricsReg)))

	form := url.Values{"project": {"bad/id"}}
	req := formRequest("/ui/integrations/email/save", form)
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "email")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, float64(1), testutil.ToFloat64(s.integrationsMetrics.SaveTotal.WithLabelValues("email", "write_failed")))
	assert.Equal(t, float64(0), testutil.ToFloat64(s.integrationsMetrics.ProbeTotal.WithLabelValues("email", "ok")), "a save that never reached the probe must not record a probe metric")
}

func TestIntegrationSave_RecordsReloadFailedMetric(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{OK: true, Outcome: integrations.OutcomeOK, Summary: "mailbox ready"}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("email", prober))

	metricsReg := prometheus.NewRegistry()
	s := NewServer(
		WithProjectRegistry(reg),
		WithIntegrationsMetrics(NewIntegrationsMetrics(metricsReg)),
		WithConfigReloader(erroringConfigReloader{err: assertError("reload exploded")}),
	)
	form := url.Values{
		"project":   {"form-demo"},
		"imap_host": {"imap.example.com"}, "imap_username": {"a@example.com"}, "imap_password_env": {"imap-secret-metrics-reload"},
		"smtp_host": {"smtp.example.com"}, "smtp_username": {"a@example.com"}, "smtp_password_env": {"smtp-secret-metrics-reload"},
		"from_address": {"a@example.com"},
	}
	req := formRequest("/ui/integrations/email/save", form)
	rec := httptest.NewRecorder()
	s.IntegrationSave(rec, req, "email")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, float64(1), testutil.ToFloat64(s.integrationsMetrics.SaveTotal.WithLabelValues("email", "reload_failed")))
	assert.Equal(t, float64(1), testutil.ToFloat64(s.integrationsMetrics.ProbeTotal.WithLabelValues("email", "ok")), "step 1's re-probe still happened and succeeded before the reload step failed")
}

func TestIntegrationSaveResultLabel(t *testing.T) {
	assert.Equal(t, "ok", integrationSaveResultLabel(true, nil))
	assert.Equal(t, "probe_failed", integrationSaveResultLabel(false, nil))
	assert.Equal(t, "write_failed", integrationSaveResultLabel(false, assertError("boom")))
	assert.Equal(t, "write_failed", integrationSaveResultLabel(false, &integrations.SaveError{Step: "write", Cause: assertError("boom")}))
	assert.Equal(t, "reload_failed", integrationSaveResultLabel(false, &integrations.SaveError{Step: "reload", Cause: assertError("boom")}))
}
