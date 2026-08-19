package telemetryclient

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestProjectRequestHasNoIdentifierOrQuery(t *testing.T) {
	req, err := BuildRequest(DefaultEndpoint, ProjectEvent("2026.8.7", SourceCLITemplate, "personal-assistant", true, true))
	require.NoError(t, err)
	require.Empty(t, req.URL.RawQuery)
}
