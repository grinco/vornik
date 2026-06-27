package mcp

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// DefaultCatalogTTL is the freshness window for daemon-level catalog
// entries. After this much wall-clock time we trigger an async refresh
// on the next read, but always return the last-known catalog so the
// HTTP/UI surface is never blocked behind a slow MCP server. Exposed
// for tests; production never overrides this.
const DefaultCatalogTTL = 5 * time.Minute

// ServerSnapshot is one daemon-level MCP server's discovery state.
// Returned by Registry.Snapshot in the order operators declared the
// servers (stable sort by Name so the UI rows don't shuffle between
// refreshes). Reachable=false rows still appear — operators need to
// see broken servers, not have them silently dropped.
type ServerSnapshot struct {
	// Name is the operator-supplied unique key for this server.
	Name string `json:"name"`
	// Transport is "stdio" or "sse".
	Transport string `json:"transport"`
	// URL is set for sse-transport servers.
	URL string `json:"url,omitempty"`
	// Command is set for stdio-transport servers.
	Command string `json:"command,omitempty"`
	// Reachable reports whether the most recent connect / refresh
	// succeeded. False means the server is configured but the
	// tools/list call failed (transport down, refused, timed out,
	// auth missing, etc.).
	Reachable bool `json:"reachable"`
	// Tools is the post-allowlist catalog the server advertised.
	// Nil when Reachable=false; empty when the server connected
	// but advertised no tools.
	Tools []Tool `json:"tools"`
	// Error is the short human-readable reason Reachable is false.
	// Empty on the happy path.
	Error string `json:"error,omitempty"`
	// LastCheckedAt is the wall-clock time we last attempted (or
	// completed) a refresh. The operator UI displays this so a
	// long-stale row is obvious.
	LastCheckedAt time.Time `json:"last_checked_at"`
}

// Registry caches the daemon-level MCP server catalog. Distinct from
// Manager (which scopes per-project active clients used by agents) —
// this is the discovery surface only. The whole point is that an
// operator can see "what's installed at the daemon level" without
// hand-walking each project YAML; clients here NEVER get attached to
// a project's effective tool list. That separation enforces the
// auto-discovery ≠ auto-grant boundary the rollout brief calls out.
//
// The Registry holds no live clients. Each refresh connects, lists
// tools, closes — keeping no long-lived stdio subprocesses around for
// the discovery surface. SSE refreshes are cheap (one HTTP POST), so
// re-connecting per refresh isn't a hot-path concern. The cost is
// bounded by len(Servers) × refresh frequency, and we throttle
// refreshes at DefaultCatalogTTL (5 min) so even a 100-server
// deployment averages well under one connect/sec.
type Registry struct {
	mu         sync.RWMutex
	servers    []ServerConfig
	catalog    map[string]ServerSnapshot
	ttl        time.Duration
	logger     zerolog.Logger
	refreshing map[string]struct{} // server names currently being refreshed async
}

// NewRegistry builds a Registry from the supplied daemon-level
// server configs. Refreshes are NOT triggered here — callers run
// RefreshAll(ctx) once at startup so the surface comes up populated.
// ttl=0 falls back to DefaultCatalogTTL; passing a custom value is
// for tests only.
func NewRegistry(servers []ServerConfig, ttl time.Duration, logger zerolog.Logger) *Registry {
	if ttl <= 0 {
		ttl = DefaultCatalogTTL
	}
	r := &Registry{
		servers:    append([]ServerConfig(nil), servers...),
		catalog:    make(map[string]ServerSnapshot, len(servers)),
		ttl:        ttl,
		logger:     logger,
		refreshing: make(map[string]struct{}),
	}
	// Pre-seed every server with a Reachable=false placeholder so the
	// UI shows them even before the first refresh completes. Without
	// this the operator's first page load (before RefreshAll fires)
	// would look like the daemon has no servers configured.
	now := time.Now()
	for _, cfg := range r.servers {
		r.catalog[cfg.Name] = ServerSnapshot{
			Name:          cfg.Name,
			Transport:     cfg.Transport,
			URL:           cfg.URL,
			Command:       cfg.Command,
			Reachable:     false,
			Error:         "not yet refreshed",
			LastCheckedAt: now,
		}
	}
	return r
}

// ServerCount returns how many daemon-level servers are configured
// (regardless of reachability). Cheap; used by readiness logging.
func (r *Registry) ServerCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.servers)
}

// Snapshot returns the current catalog sorted by server name. Safe to
// call concurrently. Triggers an async refresh for any server whose
// LastCheckedAt is older than the TTL, but always returns the
// last-known state — callers (HTTP, UI) never wait for a slow MCP
// server. Pass a context with a deadline if you want the async
// refresh bounded; ctx is only used by the spawned refresh goroutine.
func (r *Registry) Snapshot(ctx context.Context) []ServerSnapshot {
	r.mu.RLock()
	now := time.Now()
	stale := make([]ServerConfig, 0)
	out := make([]ServerSnapshot, 0, len(r.catalog))
	for _, cfg := range r.servers {
		snap, ok := r.catalog[cfg.Name]
		if !ok {
			snap = ServerSnapshot{
				Name:          cfg.Name,
				Transport:     cfg.Transport,
				URL:           cfg.URL,
				Command:       cfg.Command,
				Reachable:     false,
				Error:         "not yet refreshed",
				LastCheckedAt: now,
			}
		}
		out = append(out, snap)
		if now.Sub(snap.LastCheckedAt) >= r.ttl {
			stale = append(stale, cfg)
		}
	}
	r.mu.RUnlock()

	// Trigger async refresh for stale entries. We do this OUTSIDE the
	// read lock so the goroutine can take the write lock when it
	// finishes. Snapshot itself never blocks on the actual connect.
	for _, cfg := range stale {
		r.spawnRefresh(ctx, cfg)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RefreshAll synchronously connects to every configured server and
// updates the cached catalog. Used at startup so the registry is
// populated before the first HTTP request. The per-server connect
// timeout is bounded by ctx — caller should pass a deadline.
// Aggregate errors are logged but never bubble up: a partial-failure
// catalog is more useful than refusing to start.
func (r *Registry) RefreshAll(ctx context.Context) {
	r.mu.RLock()
	configs := append([]ServerConfig(nil), r.servers...)
	r.mu.RUnlock()

	var wg sync.WaitGroup
	for _, cfg := range configs {
		cfg := cfg
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.refreshOne(ctx, cfg)
		}()
	}
	wg.Wait()
}

// spawnRefresh starts an async refresh for one server unless one is
// already in flight. The dedupe keeps a flood of stale Snapshot()
// calls from kicking off N goroutines for the same wedged server.
func (r *Registry) spawnRefresh(ctx context.Context, cfg ServerConfig) {
	r.mu.Lock()
	if _, busy := r.refreshing[cfg.Name]; busy {
		r.mu.Unlock()
		return
	}
	r.refreshing[cfg.Name] = struct{}{}
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.refreshing, cfg.Name)
			r.mu.Unlock()
		}()
		// Async refresh uses a fresh context so the original HTTP
		// request canceling doesn't kill the refresh mid-connect.
		// The 30s budget matches Connect()'s own timeout.
		refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctx // signature documents intent; we deliberately don't propagate
		r.refreshOne(refreshCtx, cfg)
	}()
}

// refreshOne attempts a connect + tools/list against one server and
// writes the result back to the catalog. Errors are recorded on the
// snapshot, not returned — every server gets some row, reachable or
// not.
func (r *Registry) refreshOne(ctx context.Context, cfg ServerConfig) {
	now := time.Now()
	client, err := Connect(ctx, cfg, r.logger.With().Str("server", cfg.Name).Str("scope", "daemon").Logger())
	snap := ServerSnapshot{
		Name:          cfg.Name,
		Transport:     cfg.Transport,
		URL:           cfg.URL,
		Command:       cfg.Command,
		LastCheckedAt: now,
	}
	if err != nil {
		snap.Reachable = false
		snap.Error = shortenError(err)
	} else {
		snap.Reachable = true
		// Copy the slice so closing the client below doesn't reach
		// back into our stored snapshot.
		tools := client.Tools()
		snap.Tools = append([]Tool(nil), tools...)
		_ = client.Close()
	}

	r.mu.Lock()
	r.catalog[cfg.Name] = snap
	r.mu.Unlock()
}

// shortenError caps the error string at 256 bytes so a hostile MCP
// server can't bloat our cached snapshot with a megabyte of upstream
// stack trace.
func shortenError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const cap = 256
	if len(s) > cap {
		return s[:cap] + "…"
	}
	return s
}

// ErrRegistryNotConfigured is returned by RegistrySource consumers
// when the daemon was started without an mcp.servers block. Lets
// the HTTP handler return 200 + empty array rather than 500.
var ErrRegistryNotConfigured = errors.New("mcp registry not configured")
