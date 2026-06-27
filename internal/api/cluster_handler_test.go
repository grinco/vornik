package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type stubClusterNodes struct{ nodes []*persistence.ClusterNode }

func (s stubClusterNodes) Upsert(context.Context, *persistence.ClusterNode) error { return nil }
func (s stubClusterNodes) List(context.Context) ([]*persistence.ClusterNode, error) {
	return s.nodes, nil
}
func (s stubClusterNodes) DeleteByInstanceID(context.Context, string) error { return nil }
func (s stubClusterNodes) DeleteStale(context.Context, time.Duration, []string) (int, error) {
	return 0, nil
}

type stubLeaderLocks struct {
	locks []*persistence.DaemonLeaderLock
}

func (s stubLeaderLocks) Acquire(context.Context, string, string, time.Time, time.Duration) (bool, int64, error) {
	return true, 1, nil
}
func (s stubLeaderLocks) Renew(context.Context, string, string, time.Time, time.Duration) (bool, error) {
	return true, nil
}
func (s stubLeaderLocks) Release(context.Context, string, string) error { return nil }
func (s stubLeaderLocks) Get(context.Context, string) (*persistence.DaemonLeaderLock, error) {
	return nil, nil
}
func (s stubLeaderLocks) List(context.Context) ([]*persistence.DaemonLeaderLock, error) {
	return s.locks, nil
}

func TestClusterStatus_ReportsFleetLeasesAndSkew(t *testing.T) {
	now := time.Now()
	nodes := []*persistence.ClusterNode{
		{InstanceID: "a", Profile: "worker", Version: "v1", LastSeen: now},
		{InstanceID: "b", Profile: "worker", Version: "v2", LastSeen: now}, // skew vs a
	}
	locks := []*persistence.DaemonLeaderLock{
		{WorkerID: "autonomy_manager", HolderID: "a", ExpiresAt: now.Add(time.Minute)},
	}
	srv := NewServer(
		WithClusterNodeRepository(stubClusterNodes{nodes}),
		WithLeaderLockRepository(stubLeaderLocks{locks}),
	)
	h := SetupRoutes(srv, minimalConfigForReadyz()) // empty cfg → all caps → ServeAPI on

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/cluster", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Nodes       []map[string]any `json:"nodes"`
		Leases      []map[string]any `json:"leases"`
		VersionSkew bool             `json:"version_skew"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(resp.Nodes))
	}
	if len(resp.Leases) != 1 || resp.Leases[0]["holder_id"] != "a" {
		t.Fatalf("lease map must show autonomy_manager held by a, got %+v", resp.Leases)
	}
	if !resp.VersionSkew {
		t.Fatal("two distinct versions must set version_skew=true")
	}
}

func TestClusterStatus_StaleNodeDoesNotMisattributeLease(t *testing.T) {
	now := time.Now()
	nodes := []*persistence.ClusterNode{
		// node "a" is stale: last_seen 2 minutes ago, profile "worker"
		{InstanceID: "a", Profile: "worker", Version: "v1", LastSeen: now.Add(-2 * time.Minute)},
		// node "b" is fresh: profile "ui"
		{InstanceID: "b", Profile: "ui", Version: "v1", LastSeen: now},
	}
	locks := []*persistence.DaemonLeaderLock{
		// lease is held by "b" (the fresh node), NOT the stale node "a"
		{WorkerID: "scheduler", HolderID: "b", ExpiresAt: now.Add(time.Minute)},
	}
	srv := NewServer(
		WithClusterNodeRepository(stubClusterNodes{nodes}),
		WithLeaderLockRepository(stubLeaderLocks{locks}),
	)
	h := SetupRoutes(srv, minimalConfigForReadyz())

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/cluster", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Nodes  []map[string]any `json:"nodes"`
		Leases []map[string]any `json:"leases"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Lease join must resolve to "b"'s profile ("ui"), not "a"'s ("worker").
	// This verifies the join is on holder_id and a stale row doesn't misattribute.
	if len(resp.Leases) != 1 {
		t.Fatalf("want 1 lease, got %d: %+v", len(resp.Leases), resp.Leases)
	}
	if got := resp.Leases[0]["holder_profile"]; got != "ui" {
		t.Errorf("holder_profile: got %q, want %q — stale node 'a' (worker) must not be joined to this lease", got, "ui")
	}
	if got := resp.Leases[0]["holder_id"]; got != "b" {
		t.Errorf("holder_id: got %q, want %q", got, "b")
	}

	// Node "a" (stale) must report stale:true; node "b" (fresh) must report stale:false.
	nodesByID := make(map[string]map[string]any, len(resp.Nodes))
	for _, n := range resp.Nodes {
		id, _ := n["instance_id"].(string)
		nodesByID[id] = n
	}
	if nodeA, ok := nodesByID["a"]; !ok {
		t.Fatal("node 'a' missing from response")
	} else if nodeA["stale"] != true {
		t.Errorf("node 'a' stale: got %v, want true", nodeA["stale"])
	}
	if nodeB, ok := nodesByID["b"]; !ok {
		t.Fatal("node 'b' missing from response")
	} else if nodeB["stale"] != false {
		t.Errorf("node 'b' stale: got %v, want false", nodeB["stale"])
	}
}
