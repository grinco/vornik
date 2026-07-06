package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/projectdoctor"
	"vornik.io/vornik/internal/registry"
)

// --- tiny local fakes for the exported projectdoctor interfaces.
// Mirrors the fake shapes used by internal/projectdoctor's own unit
// tests (fakeResolver, fakePinger, fakeSnap), redefined here because
// those are unexported to that package.

type doctorFakeResolver struct {
	proj *registry.Project
	err  error
}

func (f doctorFakeResolver) ResolveProjectConfig(_ string) (*registry.Project, *registry.Swarm, *registry.Workflow, error) {
	return f.proj, nil, nil, f.err
}

type doctorFakePinger struct{ err error }

func (f doctorFakePinger) Ping(_ context.Context) error { return f.err }

type doctorFakeSnap []mcp.ServerSnapshot

func (f doctorFakeSnap) Snapshot(_ context.Context) []mcp.ServerSnapshot { return f }

// memSecrets is a shared-map fake SecretReader/Writer pair — Set on
// the writer flips Has on the reader because both wrap the same map.
type memSecrets struct{ m map[string]bool }

func (s memSecrets) Has(name string) bool { return s.m[name] }
func (s memSecrets) Set(name, value string) error {
	s.m[name] = value != ""
	return nil
}

// doctorRig builds an admin-enabled Server wired with a
// *projectdoctor.Doctor over a resolvable project "p1" that declares
// one secret (TOK), initially present or missing per tokPresent.
// Returns the server + the shared secrets map so tests can assert on
// persisted state.
func doctorRig(t *testing.T, tokPresent bool) (*Server, *memSecrets) {
	t.Helper()
	proj := &registry.Project{
		ID:          "p1",
		Permissions: registry.ProjectPermissions{Secrets: []string{"TOK"}},
	}
	secrets := memSecrets{m: map[string]bool{"TOK": tokPresent}}
	d := projectdoctor.New(projectdoctor.Deps{
		Registry:     doctorFakeResolver{proj: proj},
		Secrets:      secrets,
		SecretWriter: secrets,
		Model:        doctorFakePinger{},
		MCP:          doctorFakeSnap{},
	})
	srv := &Server{
		projectDoctor: d,
		adminConfig:   config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}},
	}
	return srv, &secrets
}

func TestProjectDoctor_GetReport_RequiresAdmin(t *testing.T) {
	srv, _ := doctorRig(t, true)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/doctor", nil)
	rec := httptest.NewRecorder()
	srv.ProjectDoctor(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "body=%s", rec.Body.String())
}

func TestProjectDoctor_GetReport_HappyPath(t *testing.T) {
	srv, _ := doctorRig(t, true)
	req := templateAdminReq(httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/doctor", nil))
	rec := httptest.NewRecorder()
	srv.ProjectDoctor(rec, req)
	rawBody := rec.Body.String()
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rawBody)

	var rep projectdoctor.Report
	require.NoError(t, json.Unmarshal([]byte(rawBody), &rep))
	assert.Equal(t, "p1", rep.ProjectID)
	require.Len(t, rep.Checks, 6)
	assert.True(t, rep.Complete, "all declared secrets present, model/mcp healthy => complete")

	// Sanity: raw JSON carries both keys the brief calls out.
	assert.Contains(t, rawBody, `"checks"`)
	assert.Contains(t, rawBody, `"complete"`)
}

func TestProjectDoctor_RunOne_UnknownKey(t *testing.T) {
	srv, _ := doctorRig(t, true)
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/doctor/checks/bogus/run", nil))
	rec := httptest.NewRecorder()
	srv.ProjectDoctor(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "CHECK_FAILED")
}

func TestProjectDoctor_SetSecret_WritesAndReturnsRefreshedCheck(t *testing.T) {
	srv, secrets := doctorRig(t, false)

	// Before: TOK is missing, so a full report shows it red/incomplete.
	getReq := templateAdminReq(httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/doctor", nil))
	getRec := httptest.NewRecorder()
	srv.ProjectDoctor(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code)
	var before projectdoctor.Report
	require.NoError(t, json.NewDecoder(getRec.Body).Decode(&before))
	assert.False(t, before.Complete, "TOK not yet set => incomplete")

	// Set it via the secrets endpoint.
	body := `{"name":"TOK","value":"shh"}`
	postReq := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/doctor/secrets", strings.NewReader(body)))
	postRec := httptest.NewRecorder()
	srv.ProjectDoctor(postRec, postReq)
	require.Equal(t, http.StatusOK, postRec.Code, "body=%s", postRec.Body.String())

	var refreshed projectdoctor.CheckResult
	require.NoError(t, json.NewDecoder(postRec.Body).Decode(&refreshed))
	assert.Equal(t, "secrets", refreshed.Key)
	assert.Equal(t, projectdoctor.StatusGreen, refreshed.Status)
	assert.True(t, secrets.Has("TOK"), "shared-map fake must reflect the write")

	// Follow-up GET shows the secret green / report complete.
	getReq2 := templateAdminReq(httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/doctor", nil))
	getRec2 := httptest.NewRecorder()
	srv.ProjectDoctor(getRec2, getReq2)
	require.Equal(t, http.StatusOK, getRec2.Code)
	var after projectdoctor.Report
	require.NoError(t, json.NewDecoder(getRec2.Body).Decode(&after))
	assert.True(t, after.Complete, "TOK now present => complete")
}

// TestProjectDoctor_SetSecret_RejectsUndeclaredName is the API-layer
// regression for the companion review finding (2026-07-04): an
// undeclared secret name must not silently write through the writer —
// it must come back as a 400 VALIDATION_ERROR, and the shared-map
// fake must show the write never happened.
func TestProjectDoctor_SetSecret_RejectsUndeclaredName(t *testing.T) {
	srv, secrets := doctorRig(t, true)
	body := `{"name":"EVIL","value":"shh"}`
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/doctor/secrets", strings.NewReader(body)))
	rec := httptest.NewRecorder()
	srv.ProjectDoctor(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
	assert.False(t, secrets.Has("EVIL"), "undeclared secret must never reach the writer")
}

func TestProjectDoctor_MethodGuard(t *testing.T) {
	srv, _ := doctorRig(t, true)
	req := templateAdminReq(httptest.NewRequest(http.MethodPut, "/api/v1/projects/p1/doctor", nil))
	rec := httptest.NewRecorder()
	srv.ProjectDoctor(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
