package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExpandSafe locks in the rule that expandSafe must refuse to substitute
// any variable whose name starts with VORNIK_. Those names hold daemon
// secrets (database password, API keys) and must never reach an MCP
// subprocess via project-supplied config.
func TestExpandSafe(t *testing.T) {
	// Use env vars that nothing else in the test suite is likely to read.
	t.Setenv("VORNIK_DATABASE_PASSWORD", "supersecret")
	t.Setenv("VORNIK_API_KEY", "keykeykey")
	t.Setenv("MCP_PROJECT_TOKEN", "project-token")
	t.Setenv("NON_VORNIK", "visible")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "vornik prefix refused",
			in:   "--password=${VORNIK_DATABASE_PASSWORD}",
			want: "--password=",
		},
		{
			name: "vornik api key refused",
			in:   "${VORNIK_API_KEY}",
			want: "",
		},
		{
			name: "non-vornik project var expanded",
			in:   "--token=${MCP_PROJECT_TOKEN}",
			want: "--token=project-token",
		},
		{
			name: "short form also refused",
			in:   "$VORNIK_DATABASE_PASSWORD",
			want: "",
		},
		{
			name: "unset var becomes empty",
			in:   "${DOES_NOT_EXIST_XYZ}",
			want: "",
		},
		{
			name: "literal text unchanged",
			in:   "/usr/bin/mcp-server",
			want: "/usr/bin/mcp-server",
		},
		{
			name: "mixed",
			in:   "${NON_VORNIK}:${VORNIK_API_KEY}",
			want: "visible:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandSafe(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestBaseMCPEnvUsesMinimalAllowlist(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/vornik")
	t.Setenv("VORNIK_API_KEY", "secret")
	t.Setenv("DATABASE_URL", "postgres://secret")

	env := baseMCPEnv()
	require.Contains(t, env, "PATH=/usr/bin")
	require.Contains(t, env, "HOME=/home/vornik")

	for _, item := range env {
		require.NotContains(t, item, "VORNIK_API_KEY=")
		require.NotContains(t, item, "DATABASE_URL=")
		require.NotContains(t, item, "secret")
	}
}
