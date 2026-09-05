package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/version"
)

// The support bundle's local driver reads the daemon's edition from here.
// Without it the CLI could only report the DAEMON's version beside its OWN
// edition — a mixed answer to the field a support engineer trusts first
// (support-bundle-in-CE design §4.1), and the two genuinely differ: a
// Community daemon is exactly the deployment whose operator runs the local
// path.
func TestCapabilities_ReportsEdition(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adminSurf   bool
		wantEdition string
	}{
		{"enterprise daemon", true, version.EditionEnterprise},
		{"community daemon", false, version.EditionCommunity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer()
			s.adminSurfacePresent = tc.adminSurf

			rec := httptest.NewRecorder()
			s.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			var got CapabilitiesResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Edition != tc.wantEdition {
				t.Errorf("edition = %q, want %q", got.Edition, tc.wantEdition)
			}
		})
	}
}
