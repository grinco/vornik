package ui

// Cluster + worker observability surface for the admin UI. Pairs
// with the daemon_leader_locks_health doctor check (api package)
// but renders the same data as a dedicated page so operators
// don't have to drill into a doctor JSON to see "who's running
// what in the cluster".
//
// Two views over the same daemon_leader_locks table:
//   - Nodes:   unique holder_id (hostname:pid:nonce) with the
//              worker IDs it owns + an aggregate health badge.
//   - Workers: per-worker row with current holder, classification
//              (ACTIVE / STALE / EXPIRED), and human-readable
//              "renewed Xs ago / expires in Ys" timing.
//
// Classification mirrors the doctor check exactly so both
// surfaces never disagree about a row's health.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// LeaderLockSource is the narrow contract the cluster admin page
// consumes. The service container plugs the Postgres
// persistence.DaemonLeaderLockRepository here; SQLite + unwired
// deployments leave it nil and the page renders an empty state.
type LeaderLockSource interface {
	List(ctx context.Context) ([]*persistence.DaemonLeaderLock, error)
}

// ClusterNodeSource is the narrow contract the fleet view consumes — the
// cluster_nodes heartbeat registry. Unlike the lease tables (which only show
// instances that own a singleton lease), this surfaces EVERY heartbeating
// node, including webhook/relay nodes that hold no leases. The service
// container plugs persistence.ClusterNodeRepository here; unwired deployments
// leave it nil and the fleet section is omitted.
type ClusterNodeSource interface {
	List(ctx context.Context) ([]*persistence.ClusterNode, error)
}

// fleetStaleAfter is the heartbeat-freshness bound for the fleet health badge.
// Matches the /api/v1/cluster handler's threshold (3 missed ~15s beats) so the
// UI and CLI never disagree about a node's health. Well inside the pruner's
// 5-minute delete grace, so a stale-but-present row is a real signal, not a
// race with pruning.
const fleetStaleAfter = 45 * time.Second

// FleetNode is one row in the fleet table — a cluster_nodes registry entry,
// pre-classified for the template.
type FleetNode struct {
	InstanceID  string
	Profile     string
	Version     string
	Address     string
	Health      string // ACTIVE | STALE
	LastSeen    time.Time
	LastSeenAgo string
}

// FleetCounts is the fleet summary banner aggregate.
type FleetCounts struct {
	Nodes  int
	Active int
	Stale  int
}

// summarizeFleet classifies cluster_nodes rows for the fleet table and detects
// version skew (the rolling-deploy hazard). Pure — unit-tested without the UI
// server. A node is STALE when its last heartbeat is older than fleetStaleAfter,
// else ACTIVE. Skew is true when two or more distinct non-empty build versions
// are present across the fleet.
func summarizeFleet(nodes []*persistence.ClusterNode, now time.Time) ([]FleetNode, FleetCounts, bool) {
	out := make([]FleetNode, 0, len(nodes))
	var counts FleetCounts
	versions := map[string]struct{}{}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		health := "ACTIVE"
		if n.StaleAfter(now, fleetStaleAfter) {
			health = "STALE"
			counts.Stale++
		} else {
			counts.Active++
		}
		counts.Nodes++
		if v := n.Version; v != "" {
			versions[v] = struct{}{}
		}
		out = append(out, FleetNode{
			InstanceID:  n.InstanceID,
			Profile:     n.Profile,
			Version:     n.Version,
			Address:     n.Address,
			Health:      health,
			LastSeen:    n.LastSeen,
			LastSeenAgo: humanClusterDuration(now.Sub(n.LastSeen)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out, counts, len(versions) > 1
}

// AdminHealthClusterData backs /ui/admin/health/cluster.
type AdminHealthClusterData struct {
	adminCommonData

	// Available reports whether ANY cluster source is wired (leases or
	// fleet). When false, the template renders a "single-process deployment"
	// hint instead of the empty tables.
	Available bool

	// LeasesAvailable / FleetAvailable gate the two independent sections so a
	// node with only one source wired renders just that section.
	LeasesAvailable bool
	FleetAvailable  bool

	// Fleet lists every heartbeating cluster_nodes row (incl. webhook/relay
	// nodes that hold no leases). VersionSkew flags a rolling-deploy mismatch.
	Fleet       []FleetNode
	FleetCounts FleetCounts
	VersionSkew bool

	// Error carries a List() failure for the operator. Non-nil
	// disables the tables; both lists stay zero.
	Error string

	// Nodes lists each unique holder_id across all workers in
	// alphabetical order. One row per daemon-instance.
	Nodes []AdminClusterNode

	// Workers lists each daemon_leader_locks row in alphabetical
	// order by worker_id. One row per managed worker.
	Workers []AdminClusterWorker

	// Counts aggregates classifications for the summary banner.
	Counts AdminClusterCounts

	// GeneratedAt is the wall-clock when the snapshot was taken.
	// Displayed in the page header so operators know whether
	// they're looking at a stale tab.
	GeneratedAt time.Time
}

// AdminClusterCounts holds the per-classification aggregate.
type AdminClusterCounts struct {
	Nodes   int
	Active  int
	Stale   int
	Expired int
}

// AdminClusterNode is one synthesised node-level view.
type AdminClusterNode struct {
	// HolderID is the literal value stored on each worker row —
	// hostname:pid:hex-nonce. The combined string is the per-
	// daemon-instance identity.
	HolderID string

	// Status is the aggregate verdict: ACTIVE if every worker
	// owned by this node is ACTIVE; STALE if any is STALE; ERROR
	// if any is EXPIRED. Lets the UI render a single badge per
	// node row without the operator having to scan the worker
	// list.
	Status string

	// Workers is the list of worker_ids this node currently owns,
	// in alphabetical order.
	Workers []string

	// LastRenewed is the most recent renewed_at across this
	// node's workers — gives operators a rough "is this daemon
	// still ticking?" signal. RenewedAgo is the same value
	// rendered as a human-readable duration ("12s", "3m 5s").
	LastRenewed time.Time
	RenewedAgo  string
}

// AdminClusterWorker is one row in the per-worker table.
type AdminClusterWorker struct {
	WorkerID   string
	HolderID   string
	Status     string // ACTIVE / STALE / EXPIRED
	RenewedAgo string
	ExpiresIn  string // "expires in 30s" / "expired 2m ago"
	AcquiredAt time.Time
	RenewedAt  time.Time
	ExpiresAt  time.Time
}

// AdminHealthCluster renders /ui/admin/health/cluster.
func (s *Server) AdminHealthCluster(w http.ResponseWriter, r *http.Request) {
	data := AdminHealthClusterData{
		adminCommonData: adminCommonData{
			Title:       "Admin Health — Cluster",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		LeasesAvailable: s.leaderLockSource != nil,
		FleetAvailable:  s.clusterNodeSource != nil,
		GeneratedAt:     time.Now().UTC(),
	}
	data.Available = data.LeasesAvailable || data.FleetAvailable
	if !data.Available {
		s.render(w, "admin_health_cluster.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	now := time.Now()
	if data.FleetAvailable {
		nodes, err := s.clusterNodeSource.List(ctx)
		if err != nil {
			data.Error = err.Error()
			s.logger.Warn().Err(err).Msg("admin cluster: list cluster nodes failed")
		} else {
			data.Fleet, data.FleetCounts, data.VersionSkew = summarizeFleet(nodes, now)
		}
	}
	if data.LeasesAvailable {
		rows, err := s.leaderLockSource.List(ctx)
		if err != nil {
			data.Error = err.Error()
			s.logger.Warn().Err(err).Msg("admin cluster: list leader locks failed")
		} else {
			data.Workers, data.Counts = buildClusterWorkers(rows, now)
			data.Nodes = buildClusterNodes(data.Workers, now)
			data.Counts.Nodes = len(data.Nodes)
		}
	}
	s.render(w, "admin_health_cluster.html", data)
}

// buildClusterWorkers turns persistence rows into the per-worker
// view + aggregate counts. Pure — tested without spinning up the
// UI server.
func buildClusterWorkers(rows []*persistence.DaemonLeaderLock, now time.Time) ([]AdminClusterWorker, AdminClusterCounts) {
	workers := make([]AdminClusterWorker, 0, len(rows))
	var counts AdminClusterCounts
	for _, r := range rows {
		if r == nil {
			continue
		}
		status, _ := classifyLeaderLockUI(r, now)
		w := AdminClusterWorker{
			WorkerID:   r.WorkerID,
			HolderID:   r.HolderID,
			Status:     status,
			RenewedAgo: humanClusterDuration(now.Sub(r.RenewedAt)),
			AcquiredAt: r.AcquiredAt,
			RenewedAt:  r.RenewedAt,
			ExpiresAt:  r.ExpiresAt,
		}
		expiresIn := r.ExpiresAt.Sub(now)
		if expiresIn > 0 {
			w.ExpiresIn = "in " + humanClusterDuration(expiresIn)
		} else {
			w.ExpiresIn = humanClusterDuration(-expiresIn) + " ago"
		}
		workers = append(workers, w)
		switch status {
		case "ACTIVE":
			counts.Active++
		case "STALE":
			counts.Stale++
		case "EXPIRED":
			counts.Expired++
		}
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].WorkerID < workers[j].WorkerID })
	return workers, counts
}

// buildClusterNodes groups workers by holder_id. The aggregate
// status uses the worst per-worker verdict: EXPIRED dominates
// STALE which dominates ACTIVE.
func buildClusterNodes(workers []AdminClusterWorker, now time.Time) []AdminClusterNode {
	if len(workers) == 0 {
		return nil
	}
	type accum struct {
		workers     []string
		hasActive   bool
		hasStale    bool
		hasExpired  bool
		lastRenewed time.Time
	}
	byHolder := map[string]*accum{}
	for _, w := range workers {
		a, ok := byHolder[w.HolderID]
		if !ok {
			a = &accum{}
			byHolder[w.HolderID] = a
		}
		a.workers = append(a.workers, w.WorkerID)
		switch w.Status {
		case "ACTIVE":
			a.hasActive = true
		case "STALE":
			a.hasStale = true
		case "EXPIRED":
			a.hasExpired = true
		}
		if w.RenewedAt.After(a.lastRenewed) {
			a.lastRenewed = w.RenewedAt
		}
	}
	out := make([]AdminClusterNode, 0, len(byHolder))
	for holder, a := range byHolder {
		status := "ACTIVE"
		switch {
		case a.hasExpired:
			status = "EXPIRED"
		case a.hasStale:
			status = "STALE"
		}
		sort.Strings(a.workers)
		node := AdminClusterNode{
			HolderID:    holder,
			Status:      status,
			Workers:     a.workers,
			LastRenewed: a.lastRenewed,
			RenewedAgo:  humanClusterDuration(now.Sub(a.lastRenewed)),
		}
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HolderID < out[j].HolderID })
	return out
}

// classifyLeaderLockUI mirrors the doctor check's classification
// to keep both surfaces in lockstep. Lives in this package so the
// UI doesn't import internal/api (which would pull a much larger
// dependency surface).
func classifyLeaderLockUI(r *persistence.DaemonLeaderLock, now time.Time) (string, string) {
	expiresIn := r.ExpiresAt.Sub(now)
	renewedAgo := now.Sub(r.RenewedAt)
	switch {
	case expiresIn <= 0:
		return "EXPIRED", fmt.Sprintf("holder=%s expired %s ago", r.HolderID, humanClusterDuration(-expiresIn))
	case renewedAgo > leaderelection.DefaultTTL:
		return "STALE", fmt.Sprintf("holder=%s last renewed %s ago", r.HolderID, humanClusterDuration(renewedAgo))
	default:
		return "ACTIVE", fmt.Sprintf("holder=%s renewed %s ago", r.HolderID, humanClusterDuration(renewedAgo))
	}
}

// humanClusterDuration renders the same coarse format the doctor
// check uses ("12s", "3m 5s", "1h 24m", "2d 4h"). Duplicated
// rather than imported from internal/api so the UI doesn't pick
// up the api package's whole import graph.
func humanClusterDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) - days*24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, h)
}
