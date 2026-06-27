package service

import "testing"

// TestContainer_Version verifies the version field accessor pair added for the
// cluster-heartbeat slice (Task 3 of 2026-06-12-cluster-slice-c1-fleet-observability).
func TestContainer_Version(t *testing.T) {
	c := &Container{}
	c.SetVersion("2026.6.0-test")
	if got := c.Version(); got != "2026.6.0-test" {
		t.Fatalf("Version() = %q; want %q", got, "2026.6.0-test")
	}
}
