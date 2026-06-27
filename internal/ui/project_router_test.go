package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// projectRouter is a fairly dense switch — testing each suffix as a
// dispatch test ensures a new route doesn't silently shadow an
// existing one.

func TestProjectRouter_BriefGET(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/brief", nil)
	rec := httptest.NewRecorder()
	srv.projectRouter(rec, req)
	// Brief edit handler should respond OK.
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestProjectRouter_ConfigFormGET(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/config/form", nil)
	rec := httptest.NewRecorder()
	srv.projectRouter(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestProjectRouter_ConfigGET(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/config", nil)
	rec := httptest.NewRecorder()
	srv.projectRouter(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestProjectRouter_KeysGETReturns503WithoutRepo(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/keys", nil)
	rec := httptest.NewRecorder()
	srv.projectRouter(rec, req)
	// Without API-key repo wired, the page returns 503.
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestProjectRouter_TasksNewGET(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/tasks/new", nil)
	rec := httptest.NewRecorder()
	srv.projectRouter(rec, req)
	// Form renders even when the creator is unwired (legacy form
	// shape ships without it).
	assert.NotEqual(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestProjectRouter_WizardPOSTOnlyMethodGuard(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/wizard", nil)
	rec := httptest.NewRecorder()
	srv.projectRouter(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"wizard accepts POST only")
}

func TestProjectRouter_NewPostReturns503WithoutCatalog(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/projects/new",
		strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.projectRouter(rec, req)
	// Without a templates catalog wired, POST /projects/new returns 503.
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
