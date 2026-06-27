package persistence

import (
	"context"
	"time"
)

// ClusterNode is one row of the fleet registry (LLD §4.1). Every DB-having
// node self-registers + heartbeats its own row, keyed by the same holder id
// the leader electors use (so instance_id joins daemon_leader_locks.holder_id).
type ClusterNode struct {
	InstanceID   string
	Profile      string          // resolved: all|ui|worker|webhook
	Version      string          // build version (version-skew detection)
	Address      string          // advertised host:port, best-effort
	Capabilities map[string]bool // resolved NodeCapabilities, JSON-encoded in the column
	// LastSeen is populated on read (List). IGNORED by Upsert — the DB
	// stamps the server clock (NOW()) so the row's freshness is always
	// measured against a single clock. A node's self-reported time must
	// never drive staleness (clock-skew safety).
	LastSeen time.Time
}

// StaleAfter reports whether this node's heartbeat is older than ttl.
func (n ClusterNode) StaleAfter(now time.Time, ttl time.Duration) bool {
	return now.Sub(n.LastSeen) > ttl
}

// ClusterNodeRepository persists fleet heartbeats. Postgres: real upsert;
// SQLite: single-process stub (one node) — keeps local installs working.
type ClusterNodeRepository interface {
	// Upsert inserts or refreshes this node's row (ON CONFLICT instance_id).
	// The DB stamps last_seen = NOW(); node.LastSeen is ignored.
	Upsert(ctx context.Context, node *ClusterNode) error
	// List returns every node row sorted by instance_id.
	List(ctx context.Context) ([]*ClusterNode, error)
	// DeleteByInstanceID removes a node's row (called on graceful drain).
	DeleteByInstanceID(ctx context.Context, instanceID string) error
	// DeleteStale atomically deletes every row whose last_seen is older than
	// olderThan relative to the DATABASE clock, EXCEPT instance_ids in
	// protectedInstanceIDs (active lease-holders). Single statement: the
	// staleness re-check at delete time closes the heartbeat/prune race, and
	// using the DB clock for both the last_seen write (Upsert) and this
	// comparison eliminates cross-node clock skew. Returns rows deleted.
	DeleteStale(ctx context.Context, olderThan time.Duration, protectedInstanceIDs []string) (int, error)
}
