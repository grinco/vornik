package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ClusterNodeRepository is the SQLite implementation of
// persistence.ClusterNodeRepository. Single-process (local) deployments
// only have one node, but a real upsert/list/delete means
// `vornikctl cluster status` works without special-casing the SQLite
// branch. Capabilities are stored as JSON bytes in a BLOB column.
type ClusterNodeRepository struct {
	db DBTX
}

// NewClusterNodeRepository constructs the repo over db.
func NewClusterNodeRepository(db *sql.DB) *ClusterNodeRepository {
	return &ClusterNodeRepository{db: db}
}

// Upsert inserts or refreshes this node's row (ON CONFLICT instance_id).
// last_seen is stamped with CURRENT_TIMESTAMP by the DB engine — node.LastSeen
// is ignored so that the single-process SQLite deployment also uses the DB
// clock for freshness (mirrors the postgres NOW() behaviour).
func (r *ClusterNodeRepository) Upsert(ctx context.Context, node *persistence.ClusterNode) error {
	if node == nil {
		return fmt.Errorf("cluster_node: node is nil")
	}
	caps, err := json.Marshal(node.Capabilities)
	if err != nil {
		return fmt.Errorf("cluster_node: marshal capabilities: %w", err)
	}
	// strftime('%Y-%m-%dT%H:%M:%SZ', 'now') produces an RFC3339-compatible
	// string that sorts correctly when compared with Go-formatted thresholds
	// (unlike CURRENT_TIMESTAMP which uses a space separator that breaks
	// lexicographic comparison against T-separated RFC3339 strings).
	const q = `
INSERT INTO cluster_nodes (instance_id, profile, version, address, capabilities, last_seen)
VALUES (?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
ON CONFLICT (instance_id) DO UPDATE SET
    profile      = excluded.profile,
    version      = excluded.version,
    address      = excluded.address,
    capabilities = excluded.capabilities,
    last_seen    = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')`
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
		var capsBytes []byte
		var lastSeen sqlTime
		if err := rows.Scan(
			&n.InstanceID, &n.Profile, &n.Version, &n.Address,
			&capsBytes, &lastSeen,
		); err != nil {
			return nil, fmt.Errorf("cluster_node: list scan: %w", err)
		}
		if err := json.Unmarshal(capsBytes, &n.Capabilities); err != nil {
			return nil, fmt.Errorf("cluster_node: unmarshal capabilities: %w", err)
		}
		n.LastSeen = lastSeen.Time
		out = append(out, &n)
	}
	return out, rows.Err()
}

// DeleteByInstanceID removes a node's row (called on graceful drain).
func (r *ClusterNodeRepository) DeleteByInstanceID(ctx context.Context, instanceID string) error {
	const q = `DELETE FROM cluster_nodes WHERE instance_id = ?`
	if _, err := r.db.ExecContext(ctx, q, instanceID); err != nil {
		return fmt.Errorf("cluster_node: delete: %w", err)
	}
	return nil
}

// DeleteStale deletes cluster_nodes rows whose last_seen is older than
// olderThan, excluding any instance_id in protectedInstanceIDs.
//
// SQLite has no ergonomic NOW()-interval expression, so the staleness
// threshold is computed in Go from time.Now() — acceptable for single-process
// deployments where there is no cross-node clock skew.
//
// When protectedInstanceIDs is empty the NOT IN clause is omitted entirely,
// meaning all stale rows are deleted. Returns the number of rows deleted.
func (r *ClusterNodeRepository) DeleteStale(ctx context.Context, olderThan time.Duration, protectedInstanceIDs []string) (int, error) {
	// NOTE: the stored last_seen has whole-second resolution (strftime('%S')
	// in Upsert), so olderThan must be >> 1s — the 5-minute default grace gives
	// ample margin and the second-boundary case is conservatively safe (a row
	// in the same second as the threshold sorts as not-yet-stale).
	threshold := sqliteTime(time.Now().UTC().Add(-olderThan))

	var q string
	var args []interface{}

	if len(protectedInstanceIDs) == 0 {
		q = `DELETE FROM cluster_nodes WHERE last_seen < ?`
		args = []interface{}{threshold}
	} else {
		placeholders := make([]string, len(protectedInstanceIDs))
		args = make([]interface{}, 0, 1+len(protectedInstanceIDs))
		args = append(args, threshold)
		for i, id := range protectedInstanceIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		q = `DELETE FROM cluster_nodes WHERE last_seen < ? AND instance_id NOT IN (` +
			strings.Join(placeholders, ", ") + `)`
	}

	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("cluster_node: delete stale: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Compile-time interface conformance.
var _ persistence.ClusterNodeRepository = (*ClusterNodeRepository)(nil)
