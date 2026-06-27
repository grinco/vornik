package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// ClusterNodeRepository implements persistence.ClusterNodeRepository
// against PostgreSQL. Each DB-having node upserts its own row on
// startup and on every heartbeat tick; the row is deleted on graceful
// drain. The capabilities column is JSONB — marshalled/unmarshalled as
// a flat map[string]bool.
type ClusterNodeRepository struct {
	db DBTX
}

// NewClusterNodeRepository constructs the repo over db.
func NewClusterNodeRepository(db DBTX) *ClusterNodeRepository {
	return &ClusterNodeRepository{db: db}
}

// Upsert inserts or refreshes this node's row (ON CONFLICT instance_id).
// last_seen is stamped by the DB server clock (NOW()) — node.LastSeen is
// ignored, ensuring staleness comparisons in DeleteStale use one clock.
func (r *ClusterNodeRepository) Upsert(ctx context.Context, node *persistence.ClusterNode) error {
	if node == nil {
		return fmt.Errorf("cluster_node: node is nil")
	}
	caps, err := json.Marshal(node.Capabilities)
	if err != nil {
		return fmt.Errorf("cluster_node: marshal capabilities: %w", err)
	}
	const q = `
INSERT INTO cluster_nodes (instance_id, profile, version, address, capabilities, last_seen)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (instance_id) DO UPDATE SET
    profile      = EXCLUDED.profile,
    version      = EXCLUDED.version,
    address      = EXCLUDED.address,
    capabilities = EXCLUDED.capabilities,
    last_seen    = NOW()`
	if _, err := r.db.ExecContext(ctx, q,
		node.InstanceID,
		node.Profile,
		node.Version,
		node.Address,
		caps,
	); err != nil {
		return fmt.Errorf("cluster_node: upsert: %w", err)
	}
	return nil
}

// List returns every node row sorted by instance_id.
func (r *ClusterNodeRepository) List(ctx context.Context) ([]*persistence.ClusterNode, error) {
	const q = `
SELECT instance_id, profile, version, address, capabilities, last_seen
FROM cluster_nodes
ORDER BY instance_id`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("cluster_node: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ClusterNode
	for rows.Next() {
		var n persistence.ClusterNode
		var capsJSON []byte
		if err := rows.Scan(
			&n.InstanceID, &n.Profile, &n.Version, &n.Address,
			&capsJSON, &n.LastSeen,
		); err != nil {
			return nil, fmt.Errorf("cluster_node: list scan: %w", err)
		}
		if err := json.Unmarshal(capsJSON, &n.Capabilities); err != nil {
			return nil, fmt.Errorf("cluster_node: unmarshal capabilities: %w", err)
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}

// DeleteByInstanceID removes a node's row (called on graceful drain).
func (r *ClusterNodeRepository) DeleteByInstanceID(ctx context.Context, instanceID string) error {
	const q = `DELETE FROM cluster_nodes WHERE instance_id = $1`
	if _, err := r.db.ExecContext(ctx, q, instanceID); err != nil {
		return fmt.Errorf("cluster_node: delete: %w", err)
	}
	return nil
}

// DeleteStale atomically deletes every cluster_nodes row whose last_seen is
// older than olderThan (per the DB clock), except rows whose instance_id is
// in protectedInstanceIDs. Uses make_interval(secs => $1) so the grace
// period is expressed in seconds and compared against NOW() in the DB —
// one clock for both write (Upsert) and comparison closes clock-skew + race.
// An empty protectedInstanceIDs slice → ANY('{}') matches nothing → all
// stale rows are deleted (correct behaviour). Returns rows deleted.
func (r *ClusterNodeRepository) DeleteStale(ctx context.Context, olderThan time.Duration, protectedInstanceIDs []string) (int, error) {
	const q = `DELETE FROM cluster_nodes
WHERE last_seen < NOW() - make_interval(secs => $1)
  AND NOT (instance_id = ANY($2))`
	// A *nil* slice marshals to SQL NULL, and `instance_id = ANY(NULL)` is NULL
	// (not FALSE), so `NOT (... = ANY(NULL))` is NULL — which excludes EVERY row
	// and silently deletes nothing. Normalize nil → empty array so "protect
	// nothing" reaps all stale rows (ANY('{}') = FALSE), matching the non-nil
	// empty slice the pruner passes and the docstring's stated contract.
	if protectedInstanceIDs == nil {
		protectedInstanceIDs = []string{}
	}
	res, err := r.db.ExecContext(ctx, q, olderThan.Seconds(), pq.Array(protectedInstanceIDs))
	if err != nil {
		return 0, fmt.Errorf("cluster_node: delete stale: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Compile-time interface conformance.
var _ persistence.ClusterNodeRepository = (*ClusterNodeRepository)(nil)
