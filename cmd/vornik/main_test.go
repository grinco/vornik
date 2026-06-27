package main

import (
	"testing"

	"vornik.io/vornik/internal/version"
)

// The community main is a thin entrypoint; its existence and EE-freeness are
// pinned structurally by internal/architecture (TestCommunityMainIsEEFree).
// This test pins the package-level build-info defaults so a botched ldflags
// wiring (wrong var name) is caught at unit level.
func TestBuildInfoDefaults(t *testing.T) {
	if Version != version.Default {
		t.Errorf("Version default = %q, want %q", Version, version.Default)
	}
	if BuildDate != version.UnknownBuildDate {
		t.Errorf("BuildDate default = %q, want %q", BuildDate, version.UnknownBuildDate)
	}
}
