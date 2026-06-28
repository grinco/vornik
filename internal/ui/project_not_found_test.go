package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectDetail_NotFoundRendersNav pins the regression where the
// project_not_found.html data struct omitted CurrentPage, so the shared
// `nav` partial blew up with "can't evaluate field CurrentPage" and the
// page never rendered its body (operator saw an Internal Server Error
// instead of the friendly not-found page). The not-found page must
// render the main-content copy, which only appears AFTER the nav — so a
// nav crash means this assertion fails.
func TestProjectDetail_NotFoundRendersNav(t *testing.T) {
	srv := NewServer() // no project registry → every lookup is "not found"

	req := httptest.NewRequest(http.MethodGet, "/projects/ghost", nil)
	rr := httptest.NewRecorder()
	srv.ProjectDetail(rr, req)

	require.Equal(t, http.StatusNotFound, rr.Code)
	body := rr.Body.String()
	assert.NotContains(t, body, "Internal server error",
		"nav must render so the not-found page completes instead of 500'ing mid-template")
	assert.Contains(t, body, "does not exist",
		"main-content copy renders only after the nav partial — its absence means nav crashed")
	assert.Contains(t, body, "ghost",
		"the not-found page echoes the missing project ID")
}
