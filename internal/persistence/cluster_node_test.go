package persistence

import (
	"testing"
	"time"
)

func TestClusterNode_StaleAfter(t *testing.T) {
	now := time.Unix(1000, 0)
	fresh := ClusterNode{LastSeen: now.Add(-10 * time.Second)}
	stale := ClusterNode{LastSeen: now.Add(-90 * time.Second)}
	if fresh.StaleAfter(now, 45*time.Second) {
		t.Fatal("10s-old heartbeat must not be stale at 45s ttl")
	}
	if !stale.StaleAfter(now, 45*time.Second) {
		t.Fatal("90s-old heartbeat must be stale at 45s ttl")
	}
}
