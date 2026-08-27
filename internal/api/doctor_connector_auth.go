package api

// Doctor check: connector_auth.
//
// The equivalent of model_health / model_circuits, for CONNECTORS. Those exist
// for LLM routes; nothing covered MCP connector auth, and grepping the doctor
// surface for mcp/connector/auth returned nothing at all before this.
//
// The gap had a cost. On 2026-08-25 the Atlassian connector on vornik-marketing
// lost auth. Every tool call 401'd for roughly 51 hours. `vornikctl mcp
// oauth-status` reported the grant connected, unexpired and healthy the whole
// time, because it reads the stored grant and the stored grant looked fine —
// the credential had been frozen into the MCP client's headers at wiring time
// and the vendor had moved on. The only trace was a warn line per call and a
// paragraph inside one task's result JSON.
//
// So this check reads BOTH sides and refuses to trust either alone:
//
//   - the stored grant (needs_reconnect, expiry, whether a refresh token
//     exists) — the daemon's INTENT, and
//   - recent auth-class rows in tool_audit_log — what the VENDOR actually did.
//
// The second is the one that would have caught this incident on its first run
// after expiry, and it is only expressible because migration 168 gave tool
// failures a type. A configured value is a statement of intent, never evidence
// of behaviour.
//
// Design: https://docs.vornik.io §3.4

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// connectorAuthWindow is how far back recent auth failures are counted.
	// One hour: long enough to survive a quiet period between scheduled jobs,
	// short enough that a resolved incident stops being reported quickly.
	connectorAuthWindow = time.Hour
	// connectorAuthExpiryWarn is how close to expiry a grant with NO refresh
	// token gets a warning. Such a grant cannot renew itself — a human must
	// reconnect — so the warning has to arrive with enough notice to act on.
	connectorAuthExpiryWarn = 24 * time.Hour
)

// connectorAuthFailure is a count of recent auth-class tool failures for one
// connector, as observed by the daemon and recorded in tool_audit_log.
type connectorAuthFailure struct {
	projectID string
	server    string
	count     int
	last      time.Time
}

// connectorGrantState is the stored grant for one (project, server).
type connectorGrantState struct {
	projectID      string
	server         string
	needsReconnect bool
	expiresAt      *time.Time
	hasRefresh     bool
}

// checkConnectorAuth reports connectors whose credentials are failing or about
// to.
func (h *DoctorHandlers) checkConnectorAuth(ctx context.Context) DoctorCheck {
	const name = "connector_auth"
	if h == nil || h.db == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no database; skipping"}
	}

	grants, err := h.queryConnectorGrants(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("grant query failed: %v", err)}
	}
	failures, err := h.queryRecentConnectorAuthFailures(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("auth-failure query failed: %v", err)}
	}

	if len(grants) == 0 && len(failures) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no OAuth-backed MCP connectors are configured"}
	}

	items, worst := evaluateConnectorAuth(grants, failures, time.Now().UTC())
	if len(items) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("%d connector grant(s) healthy; no authentication failures in the last %s", len(grants), connectorAuthWindow),
		}
	}
	return DoctorCheck{
		Name:    name,
		Status:  worst,
		Message: fmt.Sprintf("%d connector credential issue(s)", len(items)),
		Items:   items,
	}
}

// evaluateConnectorAuth is the pure decision half, so the rules are testable
// without a database.
//
// Ordering matters: observed failures outrank stored state, because the vendor
// is authoritative about its own credential and the stored grant is only our
// belief about it. A connector that is 401ing RIGHT NOW while its grant reads
// healthy is exactly the 2026-08-25 shape, and it must not be reported as a
// mere warning because the row looks fine.
func evaluateConnectorAuth(grants []connectorGrantState, failures []connectorAuthFailure, now time.Time) ([]string, string) {
	worst := "OK"
	raise := func(status string) {
		if status == "ERROR" || (status == "WARNING" && worst == "OK") {
			worst = status
		}
	}

	failureByKey := map[string]connectorAuthFailure{}
	for _, f := range failures {
		failureByKey[f.projectID+"\x00"+f.server] = f
	}

	var items []string
	reported := map[string]bool{}

	for _, g := range grants {
		key := g.projectID + "\x00" + g.server
		label := connectorLabel(g.projectID, g.server)
		fix := connectorFixHint(g.projectID, g.server)

		if f, ok := failureByKey[key]; ok {
			reported[key] = true
			raise("ERROR")
			items = append(items, fmt.Sprintf(
				"%s: %d tool call(s) rejected with 401/403 in the last %s (most recent %s) while the stored grant still reads healthy — %s",
				label, f.count, connectorAuthWindow, f.last.UTC().Format(time.RFC3339), fix))
			continue
		}
		if g.needsReconnect {
			raise("ERROR")
			items = append(items, fmt.Sprintf("%s: the stored grant is flagged needs_reconnect — %s", label, fix))
			continue
		}
		// A grant that cannot renew itself and is close to expiry is a
		// scheduled outage. Warn with enough notice to act on.
		if !g.hasRefresh && g.expiresAt != nil {
			left := g.expiresAt.Sub(now)
			switch {
			case left <= 0:
				raise("ERROR")
				items = append(items, fmt.Sprintf(
					"%s: expired at %s and has no refresh token — %s",
					label, g.expiresAt.UTC().Format(time.RFC3339), fix))
			case left <= connectorAuthExpiryWarn:
				raise("WARNING")
				items = append(items, fmt.Sprintf(
					"%s: expires in %s and has no refresh token, so it cannot renew itself — %s",
					label, left.Round(time.Minute), fix))
			}
		}
	}

	// Auth failures for a connector with NO stored grant at all. Rarer, and
	// worth its own line: it means something is calling a server nobody
	// connected, rather than a grant that went stale.
	for _, f := range failures {
		key := f.projectID + "\x00" + f.server
		if reported[key] {
			continue
		}
		if grantExists(grants, f.projectID, f.server) {
			continue
		}
		raise("ERROR")
		items = append(items, fmt.Sprintf(
			"%s: %d tool call(s) rejected with 401/403 in the last %s and there is NO stored grant — %s",
			connectorLabel(f.projectID, f.server), f.count, connectorAuthWindow,
			connectorFixHint(f.projectID, f.server)))
	}

	sort.Strings(items)
	return items, worst
}

func grantExists(grants []connectorGrantState, projectID, server string) bool {
	for _, g := range grants {
		if g.projectID == projectID && g.server == server {
			return true
		}
	}
	return false
}

// connectorLabel names a connector the way an operator will look for it: a
// daemon-scope grant lives at project "" and is shared by every project that
// subscribes to it by name, which is worth saying rather than printing an
// empty field.
func connectorLabel(projectID, server string) string {
	if projectID == "" {
		return fmt.Sprintf("%q (daemon scope)", server)
	}
	return fmt.Sprintf("%q (project %s)", server, projectID)
}

// connectorFixHint is the command that fixes it. A finding that names a
// condition without naming its cure is how the original incident stayed
// invisible for two days.
func connectorFixHint(projectID, server string) string {
	cmd := "vornikctl mcp connect " + server
	if projectID != "" {
		cmd += " -p " + projectID
	}
	return "fix: " + cmd
}

// queryConnectorGrants reads the stored grants.
func (h *DoctorHandlers) queryConnectorGrants(ctx context.Context) ([]connectorGrantState, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT project_id, server_name, needs_reconnect, expires_at, (refresh_token <> '')
		  FROM mcp_oauth_tokens`)
	if err != nil {
		// The table is created by migration; a deployment mid-upgrade (or a
		// test harness with a partial schema) is a skip, not a failure.
		return nil, nil //nolint:nilerr // absent table = nothing to check
	}
	defer func() { _ = rows.Close() }()

	var out []connectorGrantState
	for rows.Next() {
		var g connectorGrantState
		var expires *time.Time
		if err := rows.Scan(&g.projectID, &g.server, &g.needsReconnect, &expires, &g.hasRefresh); err != nil {
			return nil, err
		}
		g.expiresAt = expires
		out = append(out, g)
	}
	return out, rows.Err()
}

// queryRecentConnectorAuthFailures counts auth-class tool failures per
// connector in the recent window.
//
// Matches the class POSITIVELY (= 'auth'). It never infers a failure from the
// ABSENCE of a class: rows written before migration 168 carry outcome_class ”
// meaning UNKNOWN, and reading those as anything would either invent failures
// or — worse — declare all history successful.
//
// The connector name is derived from the tool name's `mcp__<server>__<tool>`
// qualification, which is the only place the audit row records which server a
// call went to.
func (h *DoctorHandlers) queryRecentConnectorAuthFailures(ctx context.Context) ([]connectorAuthFailure, error) {
	since := time.Now().UTC().Add(-connectorAuthWindow)
	rows, err := h.db.QueryContext(ctx, `
		SELECT project_id, tool_name, COUNT(*), MAX(created_at)
		  FROM tool_audit_log
		 WHERE outcome_class = 'auth'
		   AND created_at >= $1
		 GROUP BY project_id, tool_name`, since)
	if err != nil {
		return nil, nil //nolint:nilerr // pre-migration column absent = nothing to check
	}
	defer func() { _ = rows.Close() }()

	// Aggregate per SERVER, not per tool: an operator reconnects a connector,
	// not a tool, so five tools failing on one dead grant is one finding.
	agg := map[string]*connectorAuthFailure{}
	for rows.Next() {
		var projectID, toolName string
		var count int
		var last time.Time
		if err := rows.Scan(&projectID, &toolName, &count, &last); err != nil {
			return nil, err
		}
		server := serverFromQualifiedTool(toolName)
		if server == "" {
			continue
		}
		key := projectID + "\x00" + server
		if cur, ok := agg[key]; ok {
			cur.count += count
			if last.After(cur.last) {
				cur.last = last
			}
			continue
		}
		agg[key] = &connectorAuthFailure{projectID: projectID, server: server, count: count, last: last}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]connectorAuthFailure, 0, len(agg))
	for _, v := range agg {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].projectID != out[j].projectID {
			return out[i].projectID < out[j].projectID
		}
		return out[i].server < out[j].server
	})
	return out, nil
}

// serverFromQualifiedTool extracts `<server>` from `mcp__<server>__<tool>`.
// Returns "" for a container-local tool, which has no connector to reconnect.
func serverFromQualifiedTool(toolName string) string {
	if !strings.HasPrefix(toolName, "mcp__") {
		return ""
	}
	rest := strings.TrimPrefix(toolName, "mcp__")
	server, _, ok := strings.Cut(rest, "__")
	if !ok || server == "" {
		return ""
	}
	return server
}
