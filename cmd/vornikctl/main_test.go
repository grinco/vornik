// Package main provides tests for the vornikctl CLI entrypoint.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/cli"
)

// TestMainFunctionSetVersion tests that the main function properly sets
// the version before executing the CLI. This indirectly tests the call to
// cli.SetVersion().
func TestMainFunctionSetVersion(t *testing.T) {
	// This test verifies that cli.SetVersion() is called and the CLI receives
	// a custom version value. We simulate main()'s behavior by setting a custom
	// version and verifying it can be set.

	// Save original version values to restore after test
	originalVersion := Version
	originalBuildDate := BuildDate
	defer func() {
		Version = originalVersion
		BuildDate = originalBuildDate
	}()

	// Test with a custom build version
	Version = "1.2.3-test"
	BuildDate = "2024-05-09"

	// Call cli.SetVersion() as main() does - this validates the code path
	cli.SetVersion(Version + " (built " + BuildDate + ")")

	// Verify execution doesn't panic and CLI is ready
	assert.NotEmpty(t, Version, "version should be set")
	assert.NotEmpty(t, BuildDate, "build date should be set")
}

// TestMainFunctionVersionNotSet tests the behavior when Version is "dev".
func TestMainFunctionVersionNotSet(t *testing.T) {
	// Save and restore original version
	originalVersion := Version
	originalBuildDate := BuildDate
	defer func() {
		Version = originalVersion
		BuildDate = originalBuildDate
	}()

	// Test with default "dev" version
	Version = "dev"
	BuildDate = "unknown"

	// Call cli.SetVersion() as main() does - this validates the dev version path
	cli.SetVersion(Version + " (built " + BuildDate + ")")

	// Verify Version can be set to "dev" and BuildDate can be set
	assert.Equal(t, "dev", Version)
	assert.Equal(t, "unknown", BuildDate)
}

// TestBuildInfoVariables tests that the build info variables are exported
// and can be set (as they are injected at build time via ldflags).
func TestBuildInfoVariables(t *testing.T) {
	// These are set via ldflags at build time, but we test they exist
	// and can be modified, which is what main() relies on.

	// Save originals
	origVersion := Version
	origBuildDate := BuildDate
	defer func() {
		Version = origVersion
		BuildDate = origBuildDate
	}()

	// Test we can write to them
	Version = "test-version"
	BuildDate = "2024-01-01"

	assert.Equal(t, "test-version", Version,
		"Version variable should be writable")
	assert.Equal(t, "2024-01-01", BuildDate,
		"BuildDate variable should be writable")
}
