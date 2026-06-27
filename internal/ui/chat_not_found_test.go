package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChatPostMessage_ProjectNotInRegistryReturns404 — the
// dispatcher is wired but the project ID isn't loaded (operator
// hit a stale link). The handler should return 404 rather than
// dispatch a turn that hits an unknown project's chat config.
func TestChatPostMessage_ProjectNotInRegistryReturns404(t *testing.T) {
	server, _ := chatTestServer(t, "should not fire")
	form := url.Values{}
	form.Set("prompt", "hi")
	req := httptest.NewRequest(http.MethodPost, "/projects/ghost/chat/messages",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ChatPostMessage(rec, req, "ghost")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Project not found")
}
