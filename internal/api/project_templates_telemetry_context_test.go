package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/telemetryclient"
)

// Emission runs after the response is written, so the client may already be
// gone. Deriving the emit context from r.Context() meant a client that
// disconnected promptly cancelled the in-flight telemetry request and the event
// was silently dropped — undercounting exactly the operation that succeeded.
// The user-visible result is unaffected either way; the count is not.
func TestCreateProjectFromTemplate_EmitsEvenWhenClientDisconnects(t *testing.T) {
	srv, _, _, _ := templateRig(t)

	var calls int
	var sawContextErr error
	srv.telemetryVersion = "2026.7.4"
	srv.lifecycleTelemetry = telemetryclient.Client{
		Endpoint: "https://telemetry.example.test/v1/collect.json",
		Enabled:  true,
		HTTP: &http.Client{Transport: apiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			sawContextErr = req.Context().Err()
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Body:       io.NopCloser(strings.NewReader(`{"accepted":true}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	body, _ := json.Marshal(map[string]any{
		"slug":       "demo",
		"parameters": map[string]string{"projectId": "my-project", "greeting": "hi"},
	})
	req := templateAdminReq(httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/from-template", strings.NewReader(string(body))))
	req.Header.Set("Content-Type", "application/json")

	// The client hangs up the moment it has its response.
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	cancel()

	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, 1, calls, "the event must still be emitted after a disconnect")
	require.NoError(t, sawContextErr,
		"emit must not inherit the request's cancellation, or successful creations go uncounted")
}
