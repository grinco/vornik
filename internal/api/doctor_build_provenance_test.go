package api

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/version"
)

// The check exists because three unstamped builds shipped without anything in
// the product saying so (2026-07-30, 2026-08-03, 2026-08-15). Each status below
// is a distinct operator situation, and conflating them is what made the
// earlier occurrences slow to diagnose.
func TestCheckBuildProvenance(t *testing.T) {
	cases := []struct {
		name       string
		version    string
		wantStatus string
		wantIn     string
	}{
		{
			name:       "release build",
			version:    "2026.8.4",
			wantStatus: "OK",
			wantIn:     "2026.8.4",
		},
		{
			name:       "git describe output is still a release",
			version:    "2026.8.4-3-gabcdef1",
			wantStatus: "OK",
			wantIn:     "2026.8.4-3-gabcdef1",
		},
		{
			name:       "VCS fallback names the commit",
			version:    "dev+geae1a72aa6b5",
			wantStatus: "WARNING",
			wantIn:     "dev+geae1a72aa6b5",
		},
		{
			name:       "dirty tree",
			version:    "dev+geae1a72aa6b5.dirty",
			wantStatus: "WARNING",
			wantIn:     "unstamped build",
		},
		{
			name:       "nothing identifiable at all",
			version:    version.Default,
			wantStatus: "WARNING",
			wantIn:     "no build version",
		},
		{
			name:       "unwired server",
			version:    "",
			wantStatus: "WARNING",
			wantIn:     "no build version",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{server: &Server{buildVersion: c.version}}
			got := h.checkBuildProvenance()
			if got.Name != "build_provenance" {
				t.Errorf("Name = %q", got.Name)
			}
			if got.Status != c.wantStatus {
				t.Errorf("Status = %q, want %q (message: %s)", got.Status, c.wantStatus, got.Message)
			}
			if !strings.Contains(got.Message, c.wantIn) {
				t.Errorf("Message = %q, want it to mention %q", got.Message, c.wantIn)
			}
		})
	}
}

// A nil server must not panic the whole doctor report.
func TestCheckBuildProvenance_NilServer(t *testing.T) {
	h := &DoctorHandlers{}
	if got := h.checkBuildProvenance(); got.Status != "WARNING" {
		t.Errorf("Status = %q, want WARNING", got.Status)
	}
}
