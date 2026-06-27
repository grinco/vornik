package service

// Regression test for the applyOptions-before-early-init ordering invariant.
//
// Background (CE/EE Phase 1b breadth):
//
//	NewContainer was refactored to call c.applyOptions(opts) immediately after
//	struct allocation so that every downstream init site — initLogship (~617),
//	initScheduler (~746), initHTTPServer (~906) — sees c.providers already
//	set. Without that move, providers.Logship (and other Group B flags) were
//	false at the time initLogship ran, so Enterprise log-forwarding was silently
//	disabled even when the caller passed WithProviders(enterprise.Providers()).
//
// Why existing tests are insufficient:
//
//	TestApplyOptions_SetsProvidersBeforeAnyInit and the Group B gate tests all
//	call applyOptions / initLogship / etc. DIRECTLY on hand-built Containers.
//	They would still pass even if someone moved applyOptions back to after the
//	init sites — they never exercise the real NewContainer construction path.
//
// What this test locks:
//
//	It calls the real NewContainer with WithProviders(Logship:true) + a config
//	that has Logging.Forward.Enabled=true + a valid syslog sink. If applyOptions
//	were ordered AFTER initLogship, c.providers.Logship would be false when
//	initLogship runs (~line 617) and the router would not be built. The
//	post-construction assertion c.logshipForwarder != nil then fails — exposing
//	the regression.
//
// Phase 2c note:
//
//	Log forwarding now lives behind contracts.LogForwarder +
//	ProviderSet.LogForwarderFactory (the real adapter is EE, stripped from CE).
//	service must not import internal/enterprise, so this test installs an
//	in-package fake factory on the provider set — exercising the SAME ordering
//	invariant (providers set before initLogship) through the seam.
//
// Construction prerequisites for a DB-free test:
//
//	- Database.Driver="sqlite", Database.Path=":memory:" — no Postgres needed
//	- Node.Profile="ui" — ServeUI only; worker machinery (podman/scheduler)
//	  skips via skipNonWorker; the artifact store still needs Storage.ArtifactsPath
//	  (set to t.TempDir())
//	- No ConfigPath — pricing.Load returns Empty() on missing file (non-fatal);
//	  initRegistry warns and continues; no other path read fails
//	- initHTTPServer builds the http.Server struct only; ListenAndServe is
//	  called in Run(), which this test does not invoke

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/config"
)

// newContainerForOrderTest builds the minimal *Config that lets NewContainer
// complete without a live Postgres instance or a running HTTP listener.
//
//   - SQLite in-memory: no Postgres, migrations run in-process
//   - "ui" profile: worker machinery (podman, scheduler, watchdog, effective-cost
//     monitor, reminders) all short-circuit via skipNonWorker; artifact store is
//     created (before the skip) so Storage.ArtifactsPath must exist
//   - Logging.Forward.Enabled=true + syslog sink: sufficient for initLogship to
//     build the router when c.providers.Logship is true
func newContainerForOrderTest(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Database.Driver = "sqlite"
	cfg.Database.Path = ":memory:"
	cfg.Node.Profile = "ui"
	cfg.Storage.ArtifactsPath = t.TempDir()         // artifact store mkdir runs before skipNonWorker
	cfg.Logging.Forward = enabledSyslogForwardCfg() // Enabled=true + syslog sink
	return cfg
}

// enterpriseLikeProviders returns an in-package sentinel ProviderSet with all
// Group B flags true — identical to what enterprise.Providers() would supply —
// without importing internal/enterprise (import law: service must NOT import
// enterprise). Group A providers use the community no-ops because the
// subsystem.Build() path is not exercised here.
func enterpriseLikeProviders() ProviderSet {
	return ProviderSet{
		BlackBox:       communityBlackBox{},
		Instinct:       communityInstinct{},
		Trading:        communityTrading{},
		Clustering:     stubClustering{},
		Admin:          true,
		MemoryFirewall: true,
		OIDC:           true,
		Logship:        true,
		// An in-package fake factory stands in for the EE
		// internal/enterprise/logship.Factory (service must not import EE).
		// It builds a forwarder when forwarding is enabled in config — so the
		// ordering regression still manifests as a nil forwarder if applyOptions
		// runs after initLogship.
		LogForwarderFactory: fakeFactory(&fakeForwarder{}, nil),
	}
}

// TestNewContainer_ApplyOptionsBeforeInitLogship_OrderingRegression is the
// regression lock for the applyOptions-before-early-init ordering invariant
// introduced in CE/EE Phase 1b breadth.
//
// Contract asserted:
//
//	NewContainer(cfg, "", WithProviders(p)) with p.Logship=true AND
//	cfg.Logging.Forward.Enabled=true MUST produce a container whose
//	logshipForwarder is non-nil — proving that c.providers was already set when
//	initLogship ran.
//
// The test FAILS if applyOptions is moved to after initLogship (the
// mis-ordering it guards against) because c.providers.Logship would be false
// at call time and the router would not be built.
func TestNewContainer_ApplyOptionsBeforeInitLogship_OrderingRegression(t *testing.T) {
	cfg := newContainerForOrderTest(t)

	c, err := NewContainer(cfg, "", WithProviders(enterpriseLikeProviders()))
	if err != nil {
		t.Fatalf("NewContainer: unexpected error: %v", err)
	}

	// Core invariant: log-forwarding seam must be wired (non-nil forwarder).
	//
	// If applyOptions ran AFTER initLogship the edition gate inside initLogship
	// would see providers.Logship=false (and a nil factory) and return early
	// without building the forwarder — making this assertion fail and exposing
	// the ordering regression.
	if c.logshipForwarder == nil {
		t.Fatal("c.logshipForwarder is nil: applyOptions must run BEFORE initLogship " +
			"(ordering regression — c.providers was not set when initLogship ran)")
	}

	// Cleanup: drain the forwarder so its background goroutines stop.
	c.drainLogship(context.Background())
}
