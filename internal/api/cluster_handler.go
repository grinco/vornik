package api

import (
	"net/http"
	"time"
)

// ClusterStatus handles GET /api/v1/cluster — returns the fleet registry
// (cluster_nodes), the singleton-leader lease map (daemon_leader_locks),
// and a version-skew flag.
//
// Response shape:
//
//	{
//	  "nodes":        [{instance_id, profile, version, address, last_seen, stale}],
//	  "leases":       [{worker_id, holder_id, expires_at, holder_profile}],
//	  "version_skew": bool  // true when nodes carry > 1 distinct non-empty version
//	}
func (s *Server) ClusterStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if s.clusterNodeRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "CLUSTER_NOT_CONFIGURED",
			"cluster node repository not wired on this daemon")
		return
	}

	ctx := r.Context()
	now := time.Now()

	nodes, _ := s.clusterNodeRepo.List(ctx)

	// Build a lookup map: instance_id → profile for the lease join.
	profileByID := make(map[string]string, len(nodes))
	for _, n := range nodes {
		profileByID[n.InstanceID] = n.Profile
	}

	// Version-skew: > 1 distinct non-empty version across all nodes.
	versionSet := make(map[string]struct{})
	for _, n := range nodes {
		if n.Version != "" {
			versionSet[n.Version] = struct{}{}
		}
	}
	versionSkew := len(versionSet) > 1

	// Build node rows.
	type nodeRow struct {
		InstanceID string    `json:"instance_id"`
		Profile    string    `json:"profile"`
		Version    string    `json:"version"`
		Address    string    `json:"address"`
		LastSeen   time.Time `json:"last_seen"`
		Stale      bool      `json:"stale"`
	}
	nodeRows := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		nodeRows = append(nodeRows, nodeRow{
			InstanceID: n.InstanceID,
			Profile:    n.Profile,
			Version:    n.Version,
			Address:    n.Address,
			LastSeen:   n.LastSeen,
			Stale:      n.StaleAfter(now, 45*time.Second),
		})
	}

	// Build lease rows (nil-safe when leaderLockRepo is not wired).
	type leaseRow struct {
		WorkerID      string    `json:"worker_id"`
		HolderID      string    `json:"holder_id"`
		ExpiresAt     time.Time `json:"expires_at"`
		HolderProfile string    `json:"holder_profile"`
	}
	var leaseRows []leaseRow
	if s.leaderLockRepo != nil {
		locks, _ := s.leaderLockRepo.List(ctx)
		leaseRows = make([]leaseRow, 0, len(locks))
		for _, l := range locks {
			leaseRows = append(leaseRows, leaseRow{
				WorkerID:      l.WorkerID,
				HolderID:      l.HolderID,
				ExpiresAt:     l.ExpiresAt,
				HolderProfile: profileByID[l.HolderID], // empty string when no matching node
			})
		}
	}
	if leaseRows == nil {
		leaseRows = []leaseRow{}
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"nodes":        nodeRows,
		"leases":       leaseRows,
		"version_skew": versionSkew,
	})
}
