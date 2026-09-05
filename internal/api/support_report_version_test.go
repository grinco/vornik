package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/version"
)

// TestBundleVersion_PrefersTheRealBuild — the bundle reported the literal
// string "unstamped" until 2026-09-04, because it used the version.Default
// CONSTANT instead of the daemon's build version. api.go documents the same
// constant biting GetCapabilities for the same reason.
func TestBundleVersion_PrefersTheRealBuild(t *testing.T) {
	require.Equal(t, "2026.9.1-71-gabc", bundleVersion("2026.9.1-71-gabc"))
	require.Equal(t, "2026.9.1-71-gabc", bundleVersion("  2026.9.1-71-gabc  "))
	require.Equal(t, version.Default, bundleVersion(""),
		"an unwired build version must still produce a non-empty field")
}
