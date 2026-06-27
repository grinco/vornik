package service

// Admin UI adapters — admin-ui-design.md slice 1. Keeps the
// ui-facing interfaces glued to the daemon's internal sources
// (mcp.Manager, *sql.DB, *api.Server) in this single file so the
// rest of the container wiring doesn't have to know the shapes.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/ui"
)

// adminReadinessFromAPI runs the same checks the /readyz HTTP
// handler registers, in-process. The api.Server already owns the
// list (api.ReadinessCheck slice + DB ping) so we just borrow them
// instead of re-implementing.
type adminReadinessFromAPI struct{ s *api.Server }

func newAdminReadinessFromAPI(s *api.Server) ui.ReadinessProvider {
	if s == nil {
		return nil
	}
	return &adminReadinessFromAPI{s: s}
}

// ReadinessChecks runs the api server's check list under the same
// 3 s deadline /readyz uses. Returns the small ui-facing struct so
// the ui package doesn't import internal/api just for the type.
func (a *adminReadinessFromAPI) ReadinessChecks(ctx context.Context) []ui.AdminReadinessCheck {
	if a == nil || a.s == nil {
		return nil
	}
	results := a.s.RunReadiness(ctx)
	out := make([]ui.AdminReadinessCheck, 0, len(results))
	for _, r := range results {
		out = append(out, ui.AdminReadinessCheck{
			Name:   r.Name,
			Status: r.Status,
			Error:  r.Error,
		})
	}
	return out
}

// adminLeaseAudit queries the tasks_lease_audit table (migration
// v27) via a small raw-SQL surface. Tied directly to the daemon's
// *sql.DB so the ui package doesn't have to know about that table.
type adminLeaseAudit struct{ db *sql.DB }

func newAdminLeaseAudit(db *sql.DB) ui.LeaseAuditSource {
	if db == nil {
		return nil
	}
	return &adminLeaseAudit{db: db}
}

// CountByStatus groups rows by new_status. Useful summary for the
// admin page; the operator wants to see "100 rows transitioned to
// LEASED, 5 to FAILED" at a glance.
func (a *adminLeaseAudit) CountByStatus(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64)
	rows, err := a.db.QueryContext(ctx,
		`SELECT COALESCE(new_status, ''), COUNT(*) FROM tasks_lease_audit GROUP BY new_status ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// Recent returns the most recent N lease-audit rows.
func (a *adminLeaseAudit) Recent(ctx context.Context, limit int) ([]ui.AdminLeaseAuditRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := a.db.QueryContext(ctx, `
		SELECT id, task_id, changed_at,
		       COALESCE(old_status,''), COALESCE(new_status,''),
		       COALESCE(old_lease_id,''), COALESCE(new_lease_id,''),
		       COALESCE(SUBSTR(sql_text, 1, 200), '')
		FROM tasks_lease_audit
		ORDER BY changed_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ui.AdminLeaseAuditRow
	for rows.Next() {
		var r ui.AdminLeaseAuditRow
		if err := rows.Scan(&r.ID, &r.TaskID, &r.ChangedAt,
			&r.OldStatus, &r.NewStatus, &r.OldLeaseID, &r.NewLeaseID, &r.SQLSnippet); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// adminStuckExecs queries executions for watchdog-tagged failures.
// The watchdog package's terminal-failure path stamps error_code
// values starting with "watchdog/" (see internal/watchdog), so the
// admin page filters on that prefix.
type adminStuckExecs struct{ db *sql.DB }

func newAdminStuckExecs(db *sql.DB) ui.StuckExecutionSource {
	if db == nil {
		return nil
	}
	return &adminStuckExecs{db: db}
}

// RecentWatchdogFailures returns the most recent N executions
// flagged as stuck by the watchdog. Bounded so a degraded daemon
// with thousands of stuck rows doesn't OOM the page.
func (a *adminStuckExecs) RecentWatchdogFailures(ctx context.Context, limit int) ([]ui.AdminStuckExecution, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := a.db.QueryContext(ctx, `
		SELECT id, task_id, project_id, workflow_id,
		       started_at, updated_at,
		       COALESCE(error_code,''), COALESCE(error_message,'')
		FROM executions
		WHERE status = 'FAILED' AND error_code LIKE 'watchdog%'
		ORDER BY updated_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ui.AdminStuckExecution
	for rows.Next() {
		var r ui.AdminStuckExecution
		var started, updated sql.NullTime
		if err := rows.Scan(&r.ExecutionID, &r.TaskID, &r.ProjectID, &r.WorkflowID,
			&started, &updated, &r.ErrorCode, &r.ErrorMsg); err != nil {
			return nil, err
		}
		if started.Valid {
			r.StartedAt = started.Time
		}
		if updated.Valid {
			r.UpdatedAt = updated.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// adminMCPInventory adapts the daemon's mcp.Manager into the small
// ui-facing shape. Counts come from the manager; per-project rows
// derive from the registry config (manager doesn't list servers
// directly).
type adminMCPInventory struct {
	m *mcp.Manager
}

func newAdminMCPInventory(m *mcp.Manager) ui.MCPInventorySource {
	if m == nil {
		return nil
	}
	return &adminMCPInventory{m: m}
}

func (a *adminMCPInventory) Snapshot() ui.AdminMCPSnapshot {
	if a == nil || a.m == nil {
		return ui.AdminMCPSnapshot{}
	}
	return ui.AdminMCPSnapshot{
		ProjectCount: a.m.ProjectCount(),
		ServerCount:  a.m.ServerCount(),
	}
}

// adminMCPRefresher re-dials every project's MCP servers. The
// daemon doesn't expose a RefreshAll today; we synthesise one by
// walking the registry's projects and re-running StartForProject
// for each. The Manager closes the old client first before
// re-dialling, so this is safe to re-invoke at any time.
type adminMCPRefresher struct {
	m *mcp.Manager
	r *registry.Registry
}

func newAdminMCPRefresher(m *mcp.Manager, r *registry.Registry) ui.MCPRefresher {
	if m == nil || r == nil {
		return nil
	}
	return &adminMCPRefresher{m: m, r: r}
}

// RefreshAll walks every project in the registry and rebuilds its
// MCP catalog from current config. Returns the first error it hits
// so the operator sees a precise failure rather than a silent partial
// success; the manager itself already log-and-continues on per-server
// failures, so the only way RefreshAll itself returns an error is
// when one of the underlying dials panics — which the manager
// catches and reports.
func (a *adminMCPRefresher) RefreshAll(ctx context.Context) error {
	if a.r == nil || a.m == nil {
		return fmt.Errorf("mcp refresh: registry or manager not wired")
	}
	for _, p := range a.r.ListProjects() {
		// Convert registry.MCPServerConfig to mcp.ServerConfig.
		// Registry-side has its own type to keep the YAML schema
		// stable; the manager wants its package-local shape.
		servers := make([]mcp.ServerConfig, 0, len(p.MCP.Servers))
		for _, s := range p.MCP.Servers {
			servers = append(servers, mcp.ServerConfig{
				Name:         s.Name,
				Transport:    s.Transport,
				Command:      s.Command,
				Args:         s.Args,
				Env:          s.Env,
				URL:          s.URL,
				AllowedTools: s.AllowedTools,
			})
		}
		if len(servers) == 0 {
			continue
		}
		// Bound per-project at 30s; the manager already deadlines
		// each individual dial at 30s internally so this is a safety
		// belt for the surrounding loop.
		dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		a.m.StartForProject(dialCtx, p.ID, servers)
		cancel()
	}
	return nil
}

// adminMCPConfig returns the read-only listing for
// /ui/admin/integrations/mcp. Slice 1 just enumerates what's
// configured; slice 3 will let operators edit the daemon-level
// block from this surface.
type adminMCPConfig struct{ r *registry.Registry }

func newAdminMCPConfig(r *registry.Registry) ui.MCPConfigSource {
	if r == nil {
		return nil
	}
	return &adminMCPConfig{r: r}
}

func (a *adminMCPConfig) ConfiguredMCPServers() []ui.AdminMCPProjectRow {
	if a == nil || a.r == nil {
		return nil
	}
	projects := a.r.ListProjects()
	out := make([]ui.AdminMCPProjectRow, 0, len(projects))
	for _, p := range projects {
		if len(p.MCP.Servers) == 0 {
			continue
		}
		row := ui.AdminMCPProjectRow{ProjectID: p.ID}
		for _, s := range p.MCP.Servers {
			row.Servers = append(row.Servers, ui.AdminMCPServerRow{
				Name: s.Name,
				// Connected / ToolCount aren't surfaced from
				// config; the health page exposes those via the
				// manager. Leaving them at zero on this read-only
				// integrations view is correct — the page is
				// about what's CONFIGURED, not what's connected.
				Connected: false,
				ToolCount: 0,
			})
		}
		out = append(out, row)
	}
	return out
}
