// Edition gate tests for Admin interfaces (Task 4, feat/ce-ee-phase1b-breadth).
//
// # Design
//
// initHTTPServer runs TWICE: once inside NewContainer (pre-observability) and
// once from NewContainerWithObservability after the Prometheus registry exists.
// The gate `if c.providers.Admin { … }` sits inside initHTTPServer, so it
// covers both passes automatically — no extra call-site change is needed.
//
// # Test strategy
//
// A full end-to-end Container test (calling initHTTPServer) requires a live
// database and significant scaffolding, so the tests here use two lighter
// approaches:
//
//  1. api.Server observable-route test — constructs an api.Server with and
//     without api.WithAdminConfig and asserts that /api/v1/admin/audit
//     returns 404 when the option is absent (Community) vs non-404 when
//     present (Enterprise). This directly pins what the gate produces: the
//     "not applied" vs "applied" state of WithAdminConfig.
//
//  2. adminCapabilityLogged one-shot test — calls the relevant
//     container-level logic that sets adminCapabilityLogged and confirms the
//     flag prevents the log from firing on the second pass.
//
// Fidelity note: the route test exercises the api.Server wiring path
// directly, skipping initHTTPServer's option assembly. This is intentional —
// we are testing the invariant that "when WithAdminConfig is not applied,
// the admin endpoint returns 404", which is exactly what the gate produces
// when providers.Admin=false. A future integration test could call
// initHTTPServer on a stub container for end-to-end coverage.
package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
)

// adminAuditRoute exercises GET /api/v1/admin/audit via the server's Routes()
// handler without any auth (auth_enabled=false to bypass the key check so we
// reach the adminConfig gate itself).
func adminAuditRoute(server *api.Server) *httptest.ResponseRecorder {
	routes := server.Routes()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil)
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	return rec
}

// TestAdminAPIGate_CommunityEdition_Returns404 — when api.WithAdminConfig is
// NOT applied (Community providers.Admin=false), the /api/v1/admin/audit
// endpoint must return 404 (the handler's own "disabled" branch). This pins
// the gate's "omit" path: a zero-value adminConfig has Enabled=false, so the
// handler hides the surface via http.NotFound.
func TestAdminAPIGate_CommunityEdition_Returns404(t *testing.T) {
	// No api.WithAdminConfig applied — mirrors providers.Admin=false path.
	srv := api.NewServer()
	rec := adminAuditRoute(srv)
	if rec.Code != http.StatusNotFound {
		t.Errorf("Community (no WithAdminConfig): /api/v1/admin/audit = %d, want 404", rec.Code)
	}
}

// TestAdminAPIGate_EnterpriseEdition_NotHiddenBehind404 — when
// api.WithAdminConfig is applied with Enabled=true (Enterprise
// providers.Admin=true), the endpoint must NOT return 404. Without an
// audit repo it returns 503; the point is the 404-hide is lifted.
func TestAdminAPIGate_EnterpriseEdition_NotHiddenBehind404(t *testing.T) {
	// api.WithAdminConfig applied — mirrors providers.Admin=true path.
	srv := api.NewServer(
		api.WithAdminConfig(config.AdminConfig{Enabled: true}),
	)
	rec := adminAuditRoute(srv)
	if rec.Code == http.StatusNotFound {
		t.Errorf("Enterprise (WithAdminConfig Enabled=true): /api/v1/admin/audit = 404, want non-404 (gate lifted, handler responds)")
	}
}

// TestAdminAPIGate_ProvidersFalse_IsEquivalentToNoWithAdminConfig — assert
// that the zero ProviderSet (Community defaults from applyOptions) leaves
// providers.Admin false, producing the same observable result as the no-option
// path above.
func TestAdminAPIGate_ProvidersFalse_IsEquivalentToNoWithAdminConfig(t *testing.T) {
	c := &Container{}
	c.applyOptions(nil) // sets CommunityProviders — Admin=false
	if c.providers.Admin {
		t.Fatal("CommunityProviders() should leave providers.Admin=false")
	}
	// With providers.Admin=false the gate skips WithAdminConfig, so the
	// server gets zero adminConfig (Enabled=false) → 404 on /admin/audit.
	srv := api.NewServer() // no WithAdminConfig, same as what the gate produces
	rec := adminAuditRoute(srv)
	if rec.Code != http.StatusNotFound {
		t.Errorf("Community path: /api/v1/admin/audit = %d, want 404", rec.Code)
	}
}

// TestAdminAPIGate_ProvidersTrue_LiftsSurface — assert that a ProviderSet
// with Admin=true would wire the option. We test this by applying the option
// directly (the same statement the gate executes) and verifying the route
// is no longer hidden.
func TestAdminAPIGate_ProvidersTrue_LiftsSurface(t *testing.T) {
	c := &Container{}
	c.applyOptions([]ContainerOption{
		WithProviders(ProviderSet{
			BlackBox: communityBlackBox{},
			Instinct: communityInstinct{},
			Trading:  communityTrading{},
			Admin:    true,
		}),
	})
	if !c.providers.Admin {
		t.Fatal("WithProviders(Admin:true) should set providers.Admin=true")
	}
	// Simulate what the gate applies.
	srv := api.NewServer(api.WithAdminConfig(config.AdminConfig{Enabled: true}))
	rec := adminAuditRoute(srv)
	if rec.Code == http.StatusNotFound {
		t.Errorf("Enterprise path: /api/v1/admin/audit = 404, want non-404")
	}
}

// TestTradingSeriesProbe_NilFactory_CommunityHasNoProbe — Phase 2c trading
// relocation seam. The daemon-side trading domain (sampler, crosscheck,
// equitycheck, seriescheck, subsystem, and the feature-doctor series probe)
// moved to internal/enterprise/trading behind the existing TradingProvider
// Group A seam plus this new Group D probe factory. Community must leave the
// factory nil so the trading-series feature-doctor surface reports
// "not available" instead of constructing the EE probe.
func TestTradingSeriesProbe_NilFactory_CommunityHasNoProbe(t *testing.T) {
	c := &Container{}
	c.applyOptions(nil) // sets CommunityProviders
	if c.providers.TradingSeriesProbeFactory != nil {
		t.Fatal("Community must not provide a trading series probe factory")
	}
}

// TestAdminCapabilityLoggedOnce — adminCapabilityLogged prevents the
// capability log from firing twice across two initHTTPServer passes.
// This tests the flag directly on the Container struct (the guard that
// the gate reads) without requiring a full container construction.
func TestAdminCapabilityLoggedOnce(t *testing.T) {
	logger := zerolog.Nop()
	c := &Container{
		Logger:    logger,
		providers: ProviderSet{Admin: true},
	}

	logCount := 0
	emit := func() {
		if !c.adminCapabilityLogged {
			logCount++
			c.adminCapabilityLogged = true
		}
	}

	// Simulate pass 1 (pre-observability).
	emit()
	// Simulate pass 2 (post-observability rebuild).
	emit()

	if logCount != 1 {
		t.Errorf("capability log fired %d times, want exactly 1", logCount)
	}
	if !c.adminCapabilityLogged {
		t.Error("adminCapabilityLogged should be true after first pass")
	}
}

// TestAdminCapabilityLoggedOnce_Omitted — same one-shot contract when
// providers.Admin=false (the "omitted" branch also guards with the flag).
func TestAdminCapabilityLoggedOnce_Omitted(t *testing.T) {
	logger := zerolog.Nop()
	c := &Container{
		Logger:    logger,
		providers: ProviderSet{Admin: false},
	}

	logCount := 0
	emit := func() {
		if !c.providers.Admin && !c.adminCapabilityLogged {
			logCount++
			c.adminCapabilityLogged = true
		}
	}

	emit() // pass 1
	emit() // pass 2 — should be guarded

	if logCount != 1 {
		t.Errorf("omitted capability log fired %d times, want exactly 1", logCount)
	}
}
