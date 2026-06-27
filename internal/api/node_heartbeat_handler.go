package api

import (
	"encoding/json"
	"io"
	"net/http"

	"vornik.io/vornik/internal/persistence"
)

const maxNodeHeartbeatBytes = 16 * 1024 // identity payload only; tiny

// nodeHeartbeatEnvelope is what a DB-less DMZ node POSTs over mTLS so the job
// tier can register it in cluster_nodes on its behalf (slice C2). mTLS at the
// listener authenticates the calling node; the job tier stamps last_seen.
// Keep the json tags in sync with webhookrelay.heartbeatEnvelope.
type nodeHeartbeatEnvelope struct {
	InstanceID   string          `json:"instance_id"`
	Profile      string          `json:"profile"`
	Version      string          `json:"version"`
	Address      string          `json:"address"`
	Capabilities map[string]bool `json:"capabilities"`
}

// NodeHeartbeat handles POST /internal/v1/node-heartbeat (job tier, mTLS-only).
func (s *Server) NodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST required")
		return
	}
	if s.clusterNodeRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "CLUSTER_NOT_CONFIGURED", "cluster registry not configured")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxNodeHeartbeatBytes))
	if err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid heartbeat body")
		return
	}
	var env nodeHeartbeatEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid heartbeat envelope")
		return
	}
	if env.InstanceID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "instance_id is required")
		return
	}
	// LastSeen is intentionally omitted: the repo's Upsert stamps it with the
	// DB clock, eliminating the job-tier Go-clock as a skew vector for DMZ nodes.
	if err := s.clusterNodeRepo.Upsert(r.Context(), &persistence.ClusterNode{
		InstanceID:   env.InstanceID,
		Profile:      env.Profile,
		Version:      env.Version,
		Address:      env.Address,
		Capabilities: env.Capabilities,
	}); err != nil {
		s.logger.Warn().Err(err).Str("instance_id", env.InstanceID).Msg("node heartbeat upsert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to record node heartbeat")
		return
	}
	// Positive signal: the always-on counter plus a steady-state debug line so
	// "DMZ nodes are heartbeating" is observable without enabling trace logging
	// for the failure path only.
	s.apiMetrics.RecordNodeHeartbeatReceived(env.Profile)
	s.logger.Debug().
		Str("instance_id", env.InstanceID).
		Str("profile", env.Profile).
		Str("version", env.Version).
		Msg("node heartbeat received")
	w.WriteHeader(http.StatusNoContent)
}
