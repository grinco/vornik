package persistence

import (
	"context"
	"database/sql"
)

// WebWritePendingLister is the read-side query the inbox approval surface (web
// write actions, Task 6) needs to render pending rows as "Needs approval"
// cards. It is deliberately kept OUT of the committed WebWriteRepo interface
// (web_write_actions.go, Task 3) — that interface is the write/CAS contract the
// dispatcher tool depends on, and widening it would touch the committed file.
// The SQL-backed *webWriteRepo satisfies BOTH interfaces (the method below is
// declared on the same concrete type from a separate file), so a wiring that
// holds a WebWriteRepo can type-assert to WebWritePendingLister for the listing.
type WebWritePendingLister interface {
	// ListPendingByProject returns every web_write_action still awaiting an
	// operator decision (status='pending'), newest first. When projectIDs is
	// empty the whole pending set is returned (all-access scope); otherwise the
	// result is filtered to rows whose project_id is in the set. Callers that
	// enforce per-request scope (the inbox) still re-check each row, so the
	// filter here is a query-scope convenience, not the trust boundary.
	ListPendingByProject(ctx context.Context, projectIDs []string) ([]*WebWriteAction, error)
}

// ListPendingByProject implements WebWritePendingLister on the SQL-backed repo.
// It fetches the pending set in one query and filters by project in Go (pending
// rows are low-volume by construction — one per awaiting form submission — so a
// dynamic IN clause buys nothing over an in-memory membership test).
func (r *webWriteRepo) ListPendingByProject(ctx context.Context, projectIDs []string) ([]*WebWriteAction, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT submission_id, project_id, task_id, agent_run_id, target_url, target_host,
		       payload_json, selector_bindings, field_table_json, volatile_fields,
		       screenshot_ref, confirmation_ref, status, approval_token_hash,
		       insecure_bypass, approver, created_at, decided_at, submitted_at, token_consumed_at
		FROM web_write_actions
		WHERE status = 'pending'
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var allow map[string]bool
	if len(projectIDs) > 0 {
		allow = make(map[string]bool, len(projectIDs))
		for _, id := range projectIDs {
			allow[id] = true
		}
	}

	var out []*WebWriteAction
	for rows.Next() {
		var (
			a               WebWriteAction
			taskID          sql.NullString
			agentRunID      sql.NullString
			screenshotRef   sql.NullString
			confirmationRef sql.NullString
			tokenHash       sql.NullString
			approver        sql.NullString
		)
		if err := rows.Scan(
			&a.SubmissionID, &a.ProjectID, &taskID, &agentRunID, &a.TargetURL, &a.TargetHost,
			&a.PayloadJSON, &a.SelectorBindings, &a.FieldTableJSON, &a.VolatileFields,
			&screenshotRef, &confirmationRef, &a.Status, &tokenHash,
			&a.InsecureBypass, &approver, &a.CreatedAt, &a.DecidedAt, &a.SubmittedAt, &a.TokenConsumedAt,
		); err != nil {
			return nil, err
		}
		a.TaskID = taskID.String
		a.AgentRunID = agentRunID.String
		a.ScreenshotRef = screenshotRef.String
		a.ConfirmationRef = confirmationRef.String
		a.ApprovalTokenHash = tokenHash.String
		a.Approver = approver.String
		if allow != nil && !allow[a.ProjectID] {
			continue
		}
		cp := a
		out = append(out, &cp)
	}
	return out, rows.Err()
}
