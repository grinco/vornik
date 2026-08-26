package service

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"vornik.io/vornik/internal/controlplane"
)

// Wiring for the connector-auth operator alert.
//
// The doctor check (`connector_auth`) reports the same condition on demand;
// this pushes it, because nobody runs doctor on a customer site between
// incidents. The 2026-08-25 P0 ran for ~51 hours with a warn line per 401 and
// nothing else — the operator found it by reading a task's result payload.
//
// Design: https://docs.vornik.io §3.4

// connectorAuthAlertWindow is how far back a scan counts auth failures. It must
// be at least as long as the scan interval, or a failure can fall between two
// ticks and never be alerted on.
const connectorAuthAlertWindow = 15 * time.Minute

// dbConnectorAuthSource reads auth-class tool failures straight from the audit
// ledger — the same typed column the doctor check reads (migration 168).
type dbConnectorAuthSource struct {
	db     *sql.DB
	window time.Duration
}

// RecentAuthFailures counts auth-class failures per (project, connector).
//
// Matches the class POSITIVELY (= 'auth'). It never infers a failure from the
// ABSENCE of a class: rows written before migration 168 carry ” meaning
// UNKNOWN, and reading those as anything would either invent failures or
// declare all history successful.
func (s *dbConnectorAuthSource) RecentAuthFailures(ctx context.Context) ([]controlplane.ConnectorAuthFailure, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	window := s.window
	if window <= 0 {
		window = connectorAuthAlertWindow
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, tool_name, COUNT(*), MAX(created_at)
		  FROM tool_audit_log
		 WHERE outcome_class = 'auth'
		   AND created_at >= $1
		 GROUP BY project_id, tool_name`, time.Now().UTC().Add(-window))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// Aggregate per SERVER, not per tool: an operator reconnects a connector,
	// not a tool, so five tools failing on one dead grant is one alert.
	agg := map[string]*controlplane.ConnectorAuthFailure{}
	for rows.Next() {
		var projectID, toolName string
		var count int
		var last time.Time
		if err := rows.Scan(&projectID, &toolName, &count, &last); err != nil {
			return nil, err
		}
		server := connectorServerFromTool(toolName)
		if server == "" {
			continue
		}
		key := projectID + "\x00" + server
		if cur, ok := agg[key]; ok {
			cur.Count += count
			if last.After(cur.Last) {
				cur.Last = last
			}
			continue
		}
		agg[key] = &controlplane.ConnectorAuthFailure{
			ProjectID: projectID, Server: server, Count: count, Last: last,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]controlplane.ConnectorAuthFailure, 0, len(agg))
	for _, v := range agg {
		out = append(out, *v)
	}
	return out, nil
}

// connectorServerFromTool extracts `<server>` from `mcp__<server>__<tool>`.
// Empty for a container-local tool, which has no connector to reconnect.
func connectorServerFromTool(toolName string) string {
	if !strings.HasPrefix(toolName, "mcp__") {
		return ""
	}
	server, _, ok := strings.Cut(strings.TrimPrefix(toolName, "mcp__"), "__")
	if !ok || server == "" {
		return ""
	}
	return server
}

// startConnectorAuthWorker wires and starts the alert worker, leader-gated.
//
// Unconditional — no config flag. A connector that has lost authentication is
// not a tuning preference, and the condition this reports was invisible for two
// days precisely because every surface that could have shown it was optional.
// The worker is silent on a healthy deployment and rate-limited on a broken
// one, so there is nothing to opt out of.
func (c *Container) startConnectorAuthWorker(ctx context.Context) {
	if c == nil || c.DB == nil {
		return
	}
	var alert func(subject, body string)
	if n := c.operatorAlertNotifier(); n != nil {
		alert = func(subject, body string) { n.NotifyOperator(ctx, subject, body) }
	}
	w := &controlplane.ConnectorAuthWorker{
		Failures: &dbConnectorAuthSource{db: c.DB, window: connectorAuthAlertWindow},
		Alert:    alert,
		Logger:   c.Logger.With().Str("component", "control-plane").Str("worker", "connector-auth").Logger(),
	}
	if elector := c.initWorkerElector("control_plane_connector_auth"); elector != nil {
		w.LeaderGate = elector
		elector.BootstrapAcquire(ctx)
		go elector.Run(ctx)
	}
	go w.Run(collectorsCtxFrom(ctx, c))
}
