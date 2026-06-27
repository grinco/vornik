package service

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

// BlackBoxProvider supplies the Black Box subsystem for the active edition,
// or nil when the edition does not include it. A pure factory — no side
// effects until the returned Subsystem's Build/Start run.
type BlackBoxProvider interface {
	BlackBoxSubsystem() Subsystem
}

// InstinctProvider supplies the Instinct subsystem for the active edition,
// or nil when the edition does not include it (Community no-op).
type InstinctProvider interface {
	InstinctSubsystem() Subsystem
}

// TradingProvider supplies the Trading subsystem for the active edition,
// or nil when the edition does not include it (Community no-op).
type TradingProvider interface {
	TradingSubsystem() Subsystem
}

// IdentityProvider builds the Enterprise browser-login surface (OIDC/SSO +
// the RBAC-resolving session backend). It is the Phase-2c seam that keeps
// internal/service free of the EE identity types: LoginWiring returns ONLY
// neutral CE types (auth.Backend, http.Handler/HandlerFunc) so the container
// never names *loginflow.Handler / *session.Store / authz.*.
//
// Nil in Community — when providers.Identity == nil the container disables
// browser login entirely (today's CE behaviour: no session backend joins the
// auth chain, no /auth/* routes, no login buttons). The real implementation
// lives in internal/enterprise/identity and is wired by enterprise.Providers().
type IdentityProvider interface {
	// LoginWiring builds the SSO/OIDC login surface from the CE-held deps.
	// It returns ok=false (and zero values) when login is configured-off for
	// this instance — no GitHub provider block, or the Postgres identity core
	// is absent (sqlite). The container treats ok=false exactly like a nil
	// provider: login disabled. The provider owns its own Info/Warn logging
	// for the omission cases (via deps.Logger).
	LoginWiring(deps IdentityDeps) (backend auth.Backend, login http.Handler, logout http.HandlerFunc, providerNames []string, ok bool)
}

// IdentityDeps carries the CE-held handles the EE login wiring needs to build
// the session store, RBAC resolver, OIDC providers, and login flow. Every
// field is a neutral CE type so internal/service can populate it without
// importing internal/enterprise.
type IdentityDeps struct {
	// Auth is the parsed auth config block (providers, session lifetime,
	// external base URL, bootstrap admins, org-member defaults).
	Auth config.AuthSettings
	// Identity is the identity-core repository (users/groups/bindings)
	// backing the RBAC resolver. Nil on a non-Postgres backend.
	Identity persistence.IdentityRepository
	// UISessions is the browser-login session repository. Nil on a
	// non-Postgres backend.
	UISessions persistence.UISessionRepository
	// Registry resolves project trading-mode for the login caps cookie.
	// May be nil; the wiring skips the trading-capability stamp then.
	Registry *registry.Registry
	// Logger is the component logger for the login flow + omission notices.
	Logger zerolog.Logger
	// SessionIDFromContext extracts the active session id from the request
	// context (api.SessionIDFromContext) so the login flow can mint/refresh
	// sessions. Passed in to avoid an EE→api coupling on a single function.
	SessionIDFromContext func(ctx context.Context) string
}

// ClusteringProvider builds the Enterprise clustering subsystems (cluster
// heartbeat, webhook heartbeat, node pruner, endpoint monitor). It is the
// Phase-2c Group-A seam that keeps internal/service free of the clustering
// probe packages (internal/enterprise/clustering/{clustercheck,webhookrelay})
// and the four cluster subsystem implementations.
//
// Nil in Community — when providers.Clustering == nil the container registers
// no cluster subsystems (today's CE behaviour: the four cluster_* subsystems
// never join the lifecycle). The real implementation lives in
// internal/enterprise/clustering and is wired by enterprise.Providers().
//
// ClusterSubsystems applies the same inner gates the registration block used to
// apply inline (repo presence, node capabilities, configured endpoints), so the
// provider returns only the subsystems this node should actually run.
// leaderelection STAYS CE (it gates all single-node workers); the moved
// subsystems reach the elector via the exported Container accessors carried in
// ClusterDeps.
type ClusteringProvider interface {
	// ClusterSubsystems returns the cluster subsystems this node should run,
	// after applying the per-subsystem inner gates. Empty/nil when no inner
	// gate passes (e.g. a DB-less non-relay node).
	ClusterSubsystems(deps ClusterDeps) []Subsystem

	// NewWebhookRelayClient builds the DMZ→job-tier mTLS relay client from the
	// node.relay.* config (reads the PEM files + constructs the EE
	// webhookrelay.Client). Returns the neutral contracts.WebhookRelayClient so
	// internal/service drops its webhookrelay import. Called from initHTTPServer
	// on RelayMode nodes only. An error here aborts boot (fail closed on a
	// misconfigured relay node). This folds the former inline
	// webhookrelay.NewClientFromPEM construction behind the same provider.
	NewWebhookRelayClient(rc config.RelayConfig) (contracts.WebhookRelayClient, error)
}

// ClusterDeps carries the CE-held handles the EE clustering provider needs to
// construct its subsystems. Every field is a neutral CE type (or an exported
// Container accessor surfaced via the Container pointer) so internal/service can
// populate it without importing internal/enterprise. Assembled by the container
// in registerSubsystems and passed to providers.Clustering.ClusterSubsystems.
type ClusterDeps struct {
	// Container is the live container; the provider reads node identity, repos,
	// config, version, observability registry, operator-alert sink, and the
	// worker elector through its exported accessors (mirrors the trading
	// subsystem's deps.Container pattern).
	Container *Container
	// WebhookRelayClient is the DMZ→job-tier mTLS relay client built earlier in
	// initHTTPServer (nil on non-relay nodes). The provider type-asserts it to
	// the concrete *webhookrelay.Client for the webhook-heartbeat subsystem.
	WebhookRelayClient contracts.WebhookRelayClient
}

// BBEngineComponents bundles the EE-built replay engine adapter and the
// upgraded HealingApplier into a single value returned by BBReplayEngineFactory.
// Both fields may be nil when the DB is not available (Postgres-only).
type BBEngineComponents struct {
	ReplayEngine   api.BlackBoxReplayEngine
	HealingApplier contracts.HealingApplier
}

// ProviderSet is the set of edition-gated capability providers and presence
// flags the container consults during initialisation and registerSubsystems.
//
// Group A fields (BlackBox, Instinct, Trading) follow the provider-yields-
// Subsystem pattern: a nil return from the provider method means "this edition
// omits the subsystem". Real implementations live in internal/enterprise/…
//
// Group B fields (Admin, MemoryFirewall, OIDC, Logship) are presence flags set
// by main. Each init site checks its flag as the outer edition gate on top of
// the existing inner config/repo gate. False is the zero value and the
// Community default — Community omits all Group B capabilities. (Clustering was
// a Group B bool until Phase 2c; it is now a Group-A-style ClusteringProvider.)
//
// Group C fields (ReplaySafety, InstinctBudget, Healing) are the Phase-1c
// contract interfaces. They are nil in CommunityProviders (Community uses no
// IP engine); the real EE implementations are wired in Task 5 via the
// enterprise provider aggregator.
//
// Group D fields (factory functions) are the Phase-1c EE component factories.
// They are nil in Community; enterprise populates them so the service container
// can build EE components (blackbox service/engine, instinct scorer) without
// importing the EE domain packages.
//
// Use CommunityProviders() for the safe Community default.
// EnterpriseProviders() (in internal/enterprise) provides the full EE set.
type ProviderSet struct {
	// Group A — provider yields a Subsystem (nil => omit). Real impls in internal/enterprise.
	BlackBox BlackBoxProvider
	Instinct InstinctProvider
	Trading  TradingProvider

	// Identity builds the EE browser-login surface (OIDC/SSO + RBAC session
	// backend) behind a neutral-type seam. Nil in Community — login disabled
	// (today's CE behaviour). Real impl in internal/enterprise/identity,
	// wired by enterprise.Providers(). Group A-style: a nil provider means
	// "this edition omits browser login". (Phase 2c relocation seam.)
	Identity IdentityProvider

	// Clustering builds the EE clustering subsystems (heartbeat, webhook
	// heartbeat, node pruner, endpoint monitor) behind a neutral-type seam.
	// Nil in Community — no cluster subsystems registered (today's CE
	// behaviour). Real impl in internal/enterprise/clustering, wired by
	// enterprise.Providers(). Group A-style: a nil provider means "this edition
	// omits clustering". (Phase 2c relocation seam.)
	Clustering ClusteringProvider

	// Group B — presence flags (false => this edition omits the capability).
	// main sets these; init sites check them as the OUTER edition gate.
	Admin          bool
	MemoryFirewall bool
	OIDC           bool
	Logship        bool

	// Group C — Phase-1c contract interfaces (nil in Community; EE wires real impls in Task 5).
	//
	// ReplaySafety is the deny-by-default replay-safety classifier injected from
	// EE. Nil in Community — CE callers fail closed on nil (all tools blocked
	// during replay). Real impl: *blackbox.ReplaySafetyClassifier (EE).
	ReplaySafety contracts.ReplaySafetyClassifier
	// InstinctBudget is the learned tool-budget tier resolver injected from EE.
	// Nil in Community — CE callers interpret nil as "no learned tier" (no-tier
	// signal, no panic). Real impl: EE adapter over instinct.LearnedTier (EE).
	InstinctBudget contracts.InstinctBudgetResolver
	// Healing is the counterfactual replay plan applier injected from EE. Nil in
	// Community — CE callers interpret nil as "healing skipped" (empty trace /
	// no-op, matching today's Community behaviour). Real impl: EE adapter over
	// blackbox.Engine.Apply (EE).
	Healing contracts.HealingApplier

	// Group D — Phase-1c EE component factories (nil in Community).
	// These allow internal/service to build EE-domain components without importing
	// the EE domain packages (internal/blackbox, internal/instinct). Enterprise sets
	// them in enterprise.Providers(); community leaves them nil and CE callers skip
	// or no-op on nil.

	// BBTraceServiceFactory builds the api.BlackBoxTraceService adapter from the DB
	// and prometheus registerer. Nil in Community (endpoint 503s). Called lazily in
	// container_http.go after the DB is ready.
	BBTraceServiceFactory func(db *sql.DB, reg prometheus.Registerer) api.BlackBoxTraceService

	// BBReplayEngineFactory builds the api.BlackBoxReplayEngine adapter and the
	// upgraded contracts.HealingApplier from the DB, task creator, task repository,
	// and registerer. Nil in Community. Called lazily in container_http.go after DB +
	// taskCreator ready.
	BBReplayEngineFactory func(db *sql.DB, tc *taskcreate.Creator, taskRepo persistence.TaskRepository, reg prometheus.Registerer) BBEngineComponents

	// HealingObserverFactory builds the EE blackbox metrics observer (backed by
	// *blackbox.Metrics) from the prometheus registerer. Nil in Community — nil
	// observer is safe (workflowhealing constructors nil-guard). Called lazily in
	// container_http.go after the observability registry is ready.
	HealingObserverFactory func(reg prometheus.Registerer) contracts.HealingObserver

	// InstinctScorerFactory builds the instinct confidence scorer for the API's
	// recompute endpoint. Nil in Community (scorer not provided; api handles nil scorer
	// by skipping the recompute option). Called lazily in container_http.go.
	InstinctScorerFactory func(cfg config.InstinctConfig) persistence.InstinctScorer

	// InstinctBudgetFactory builds the contracts.InstinctBudgetResolver (backed by
	// instinct.LearnedTier in EE) from an InstinctRepository. Nil in Community —
	// the container interprets nil as "no learned tier" and skips budget resolution.
	// Called lazily in container_scheduler.go when the instinct repo is ready.
	InstinctBudgetFactory func(repo persistence.InstinctRepository) contracts.InstinctBudgetResolver

	// LogForwarderFactory builds the contracts.LogForwarder (backed by the EE
	// internal/enterprise/logship Router) from the logging.forward config block
	// and an env resolver. Nil in Community — the container then constructs no
	// forwarder and leaves the root logger / audit repos untouched (zero
	// overhead, today's CE behaviour). Returns (nil, nil) when forwarding is
	// disabled in config and (nil, err) when an enabled sink is misconfigured
	// (fail closed). Called from initLogship at boot (Phase 2c seam).
	LogForwarderFactory func(cfg config.LogForwardConfig, getenv func(string) string) (contracts.LogForwarder, error)

	// TradingSeriesProbeFactory builds the featuredoctor.TradingSeriesProbe
	// (backed by the EE internal/enterprise/trading series probe) from the
	// project registry, the equity-snapshot repo, an optional metric sink, and
	// a logger. Nil in Community — the container then wires no probe and the
	// trading-series feature-doctor surface reports "not available" (today's CE
	// behaviour). Returns the neutral featuredoctor interface so internal/service
	// never names the concrete EE probe type. Called lazily in container_http.go
	// after the snapshot repo + registry are ready (Phase 2c seam).
	TradingSeriesProbeFactory func(projects *registry.Registry, snapshots persistence.TradingPositionsSnapshotRepository, record func(projectID, code string, count int), logger zerolog.Logger) featuredoctor.TradingSeriesProbe
}

// communityBlackBox is the Community Edition no-op: it contributes no
// subsystem, so a Community build never registers the Black Box detector.
type communityBlackBox struct{}

func (communityBlackBox) BlackBoxSubsystem() Subsystem { return nil }

// communityInstinct is the Community Edition no-op for the Instinct subsystem.
type communityInstinct struct{}

func (communityInstinct) InstinctSubsystem() Subsystem { return nil }

// communityTrading is the Community Edition no-op for the Trading subsystem.
type communityTrading struct{}

func (communityTrading) TradingSubsystem() Subsystem { return nil }

// CommunityProviders is the safe default set — every Group A provider is a
// no-op and every Group B flag is false. An unstamped/Community binary gets
// this; the Enterprise main overrides it with real providers via
// internal/enterprise.Providers().
func CommunityProviders() ProviderSet {
	return ProviderSet{
		BlackBox: communityBlackBox{},
		Instinct: communityInstinct{},
		Trading:  communityTrading{},
		// Memory firewall is a Community feature (Phase 2c reclassification).
		// The inner memoryFirewallEditionGatePasses() still requires a Postgres
		// eval repo, so CE-on-Postgres enforces policy while CE-on-SQLite runs
		// unfiltered (no behavioural regression).
		MemoryFirewall: true,
		// Group B flags default false (zero value) — Community omits all.
		// Group C interfaces default nil — Community has no EE IP engines.
		// Group D factories default nil — Community has no EE component factories.
	}
}

// ContainerOption configures a Container during construction.
type ContainerOption func(*Container)

// WithProviders injects the edition's provider set. Must be applied before
// registerSubsystems runs (the constructor does so).
func WithProviders(set ProviderSet) ContainerOption {
	return func(c *Container) { c.providers = set }
}

// applyOptions defaults c.providers to CommunityProviders then applies each
// option in order. Extracted from NewContainer to keep its cognitive complexity
// within the project lint threshold.
func (c *Container) applyOptions(opts []ContainerOption) {
	c.providers = CommunityProviders()
	for _, opt := range opts {
		opt(c)
	}
}
