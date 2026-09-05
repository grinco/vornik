package cli

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/secrets"
)

// The historical half of the redaction seam, for the three stores that had
// only the live half.
//
// `secrets scan-history` was written for tool_audit_log and stayed that way
// while three more stores acquired live redaction decorators: step_prompts
// (2026-09-03), chat_system_prompts and chat_audit_log (2026-09-04). Every row
// written before its store was decorated still holds whatever it captured, and
// the step-prompt design SAID this command covered it, which it never did —
// see https://docs.vornik.io
// §4.3 and §5, and the correction in the step-prompt design's §0.2.
//
// Two shapes, and the difference is the whole reason this file exists:
//
//   - chat_audit_log is an ordinary row: redact the text, UPDATE in place.
//   - chat_system_prompts and step_prompts are CONTENT-ADDRESSED. The hash is
//     the primary key AND what the referrers hold, so redacting a body changes
//     its identity. That is not an UPDATE but a re-key: insert under the new
//     hash, repoint the referrers, delete the old row — all in one transaction
//     per row, because a crash between two of the three repointed step-prompt
//     columns would leave one part of a prompt resolving to redacted bytes and
//     another to raw ones, a state nothing would detect or report.

// contentStore describes a content-addressed prompt store: bodies keyed by the
// sha256 of their own content, referenced by hash from elsewhere.
type contentStore struct {
	// table holds (hash, body).
	table string
	// repoint rewrites every referring column from the old hash to the new
	// one. ONE statement, whatever the number of columns: a partial repoint
	// must not be a state this code can produce.
	repoint string
	// checkpoint labels the redaction events in secret_redaction_audit.
	checkpoint string
	// hash is the store's OWN identity function. Both are sha256-hex today,
	// but borrowing one store's hasher for another is exactly how a key
	// function drifts from the rows it keys.
	hash func(string) string
}

// contentStores is the closed set. Adding a store means adding its referrers
// here — there is no discovery, deliberately: a referrer this map does not
// know about is one the re-key would silently orphan.
var contentStores = []contentStore{
	{
		table:      "chat_system_prompts",
		repoint:    `UPDATE chat_audit_log SET system_prompt_hash = $1 WHERE system_prompt_hash = $2`,
		checkpoint: secrets.CheckpointToolAudit,
		hash:       persistence.HashChatSystemPrompt,
	},
	{
		table: "step_prompts",
		// All three columns in ONE statement (design §5.1). Three separate
		// UPDATEs would make a partial repoint reachable on a crash.
		repoint: `UPDATE execution_step_outcomes SET
			prompt_system_hash = CASE WHEN prompt_system_hash = $2 THEN $1 ELSE prompt_system_hash END,
			prompt_user_hash   = CASE WHEN prompt_user_hash   = $2 THEN $1 ELSE prompt_user_hash   END,
			prompt_tools_hash  = CASE WHEN prompt_tools_hash  = $2 THEN $1 ELSE prompt_tools_hash  END
			WHERE prompt_system_hash = $2 OR prompt_user_hash = $2 OR prompt_tools_hash = $2`,
		checkpoint: secrets.CheckpointToolAudit,
		hash:       persistence.HashStepPrompt,
	},
}

// contentStoreHit is one prompt body queued for re-keying.
type contentStoreHit struct {
	oldHash, newBody string
	counts           map[string]int
}

// scanContentStore scans (and with apply=true re-keys) the bodies in one
// content-addressed prompt store.
func scanContentStore(ctx context.Context, db *sql.DB, store contentStore, detector secrets.Detector, since time.Time, apply bool, sel ruleSelection, sampleN int) (scanHistoryResult, error) {
	res := scanHistoryResult{
		Applied: apply, Rules: sel.spec,
		CountsByType: map[string]int{}, SelectedByType: map[string]int{}, ExcludedByType: map[string]int{},
	}
	salt := newRunSalt()
	sampled := map[string]int{}

	q := fmt.Sprintf(`SELECT hash, body FROM %s WHERE 1=1`, store.table)
	args := []any{}
	if !since.IsZero() {
		args = append(args, since)
		q += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	q += " ORDER BY created_at ASC"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, fmt.Errorf("query %s: %w", store.table, err)
	}
	var hits []contentStoreHit
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var hash, body string
			if err = rows.Scan(&hash, &body); err != nil {
				err = fmt.Errorf("scan %s row: %w", store.table, err)
				return
			}
			res.RowsScanned++
			findings := detector.Scan([]byte(body))
			if len(findings) == 0 {
				continue
			}
			res.RowsMatched++
			countBySelection(&res, sel, findings, nil)
			if sampleN > 0 {
				collectSamples(&res, salt, sampled, sampleN, sel, hash, store.table, body, findings)
			}
			selected := selectFindings(findings, sel)
			if len(selected) == 0 {
				continue
			}
			hits = append(hits, contentStoreHit{
				oldHash: hash,
				newBody: string(secrets.Redact([]byte(body), selected)),
				counts:  secrets.CountByType(selected),
			})
		}
		err = rows.Err()
	}()
	if err != nil {
		return res, err
	}
	if !apply {
		return res, nil
	}
	for _, h := range hits {
		if rerr := rekeyPromptBody(ctx, db, store, h); rerr != nil {
			return res, rerr
		}
	}
	return res, nil
}

// rekeyPromptBody replaces one prompt body with its redacted form under the
// hash of the REDACTED bytes, repoints every referrer, and removes the old
// row — atomically.
//
// Ordering inside the transaction is insert → repoint → delete, so that at no
// committed point does a referrer name a body that is not there. The
// transaction is what makes that claim true rather than merely likely: without
// it a crash mid-repoint leaves prompt parts disagreeing about which bytes
// they are, which nothing detects.
//
// Idempotent: a second run scans the redacted body, finds nothing, and never
// reaches here.
func rekeyPromptBody(ctx context.Context, db *sql.DB, store contentStore, h contentStoreHit) (err error) {
	newHash := store.hash(h.newBody)
	if newHash == h.oldHash {
		return nil // nothing changed; not reachable via a selected finding, but cheap to assert
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin re-key: %w", store.table, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// INSERT first. A conflict means another body already redacted to exactly
	// these bytes — the row is already there, so this is a hit, not an error,
	// and the repoint below simply points at the existing row.
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (hash, body, created_at) VALUES ($1, $2, NOW()) ON CONFLICT (hash) DO NOTHING`,
		store.table), newHash, h.newBody); err != nil {
		return fmt.Errorf("%s: insert redacted body: %w", store.table, err)
	}
	if _, err = tx.ExecContext(ctx, store.repoint, newHash, h.oldHash); err != nil {
		return fmt.Errorf("%s: repoint referrers: %w", store.table, err)
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE hash = $1`, store.table), h.oldHash); err != nil {
		return fmt.Errorf("%s: delete raw body: %w", store.table, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit re-key: %w", store.table, err)
	}

	// The audit trail is written OUTSIDE the transaction on purpose: it is a
	// record of what happened, and a failure to record must not roll back the
	// redaction itself.
	auditRepo := postgres.NewSecretRedactionAuditRepository(db)
	events := make([]persistence.SecretRedactionEvent, 0, len(h.counts))
	for ft, n := range h.counts {
		events = append(events, persistence.SecretRedactionEvent{
			Checkpoint: store.checkpoint, FindingType: ft, Count: n, Source: "scan",
		})
	}
	if aerr := auditRepo.Record(ctx, events); aerr != nil {
		return fmt.Errorf("%s: record audit for %s: %w", store.table, h.oldHash, aerr)
	}
	return nil
}

// chatAuditHit is one chat_audit_log row queued for in-place redaction.
type chatAuditHit struct {
	id, projectID            string
	userMessage, response    string
	toolCalls                string
	rewriteUser, rewriteResp bool
	rewriteTools             bool
	counts                   map[string]int
}

// scanChatAuditHistory scans (and with apply=true redacts) the free text on
// chat_audit_log: what a person typed, what the model said back, and the tool
// calls in between. Ordinary rows, rewritten in place — the prompt BODIES
// those rows point at are the content-addressed store above.
func scanChatAuditHistory(ctx context.Context, db *sql.DB, detector secrets.Detector, project string, since time.Time, apply bool, sel ruleSelection, sampleN int) (scanHistoryResult, error) {
	res := scanHistoryResult{
		Applied: apply, Rules: sel.spec,
		CountsByType: map[string]int{}, SelectedByType: map[string]int{}, ExcludedByType: map[string]int{},
	}
	salt := newRunSalt()
	sampled := map[string]int{}

	q := `SELECT id, project_id, user_message, response, tool_calls_json FROM chat_audit_log WHERE 1=1`
	args := []any{}
	if project != "" {
		args = append(args, project)
		q += fmt.Sprintf(" AND project_id = $%d", len(args))
	}
	if !since.IsZero() {
		args = append(args, since)
		q += fmt.Sprintf(" AND ts >= $%d", len(args))
	}
	q += " ORDER BY ts ASC"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, fmt.Errorf("query chat_audit_log: %w", err)
	}
	var hits []chatAuditHit
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, projectID, userMsg, response, toolCalls string
			if err = rows.Scan(&id, &projectID, &userMsg, &response, &toolCalls); err != nil {
				err = fmt.Errorf("scan chat_audit_log row: %w", err)
				return
			}
			res.RowsScanned++
			userF := detector.Scan([]byte(userMsg))
			respF := detector.Scan([]byte(response))
			toolF := detector.Scan([]byte(toolCalls))
			if len(userF) == 0 && len(respF) == 0 && len(toolF) == 0 {
				continue
			}
			res.RowsMatched++
			countBySelection(&res, sel, userF, append(append([]secrets.Finding{}, respF...), toolF...))
			if sampleN > 0 {
				collectSamples(&res, salt, sampled, sampleN, sel, id, "chat", userMsg, userF)
				collectSamples(&res, salt, sampled, sampleN, sel, id, "chat", response, respF)
				collectSamples(&res, salt, sampled, sampleN, sel, id, "chat", toolCalls, toolF)
			}
			if h, ok := chatHitForSelection(sel, id, projectID, userMsg, response, toolCalls, userF, respF, toolF); ok {
				hits = append(hits, h)
			}
		}
		err = rows.Err()
	}()
	if err != nil {
		return res, err
	}
	if !apply {
		return res, nil
	}
	if err := applyChatAuditRedactions(ctx, db, hits); err != nil {
		return res, err
	}
	return res, nil
}

// chatHitForSelection builds the queued rewrite for one chat row, redacting
// ONLY the selected findings. A row that matched but has nothing selected is
// not queued: rewriting it would be a no-op UPDATE and a misleading audit row.
// Mirrors hitForSelection on the tool-audit path.
func chatHitForSelection(sel ruleSelection, id, projectID, userMsg, response, toolCalls string, userF, respF, toolF []secrets.Finding) (chatAuditHit, bool) {
	h := chatAuditHit{id: id, projectID: projectID, counts: map[string]int{}}
	for _, part := range []struct {
		findings []secrets.Finding
		text     string
		dst      *string
		flag     *bool
	}{
		{userF, userMsg, &h.userMessage, &h.rewriteUser},
		{respF, response, &h.response, &h.rewriteResp},
		{toolF, toolCalls, &h.toolCalls, &h.rewriteTools},
	} {
		selected := selectFindings(part.findings, sel)
		if len(selected) == 0 {
			continue
		}
		*part.dst = string(secrets.Redact([]byte(part.text), selected))
		*part.flag = true
		for ft, n := range secrets.CountByType(selected) {
			h.counts[ft] += n
		}
	}
	return h, h.rewriteUser || h.rewriteResp || h.rewriteTools
}

// applyChatAuditRedactions rewrites each matched row in place and records its
// per-type counts to secret_redaction_audit (source="scan").
func applyChatAuditRedactions(ctx context.Context, db *sql.DB, hits []chatAuditHit) error {
	auditRepo := postgres.NewSecretRedactionAuditRepository(db)
	for _, h := range hits {
		// COALESCE keeps a column that had nothing selected exactly as it is,
		// so one statement covers every combination without three variants.
		if _, err := db.ExecContext(ctx, `
			UPDATE chat_audit_log
			   SET user_message    = COALESCE($1, user_message),
			       response        = COALESCE($2, response),
			       tool_calls_json = COALESCE($3, tool_calls_json)
			 WHERE id = $4`,
			nullableIf(h.rewriteUser, h.userMessage),
			nullableIf(h.rewriteResp, h.response),
			nullableIf(h.rewriteTools, h.toolCalls),
			h.id); err != nil {
			return fmt.Errorf("redact chat_audit_log row %s: %w", h.id, err)
		}
		events := make([]persistence.SecretRedactionEvent, 0, len(h.counts))
		for ft, n := range h.counts {
			events = append(events, persistence.SecretRedactionEvent{
				ProjectID: h.projectID, Checkpoint: secrets.CheckpointToolAudit,
				FindingType: ft, Count: n, Source: "scan",
			})
		}
		if err := auditRepo.Record(ctx, events); err != nil {
			return fmt.Errorf("record audit for chat row %s: %w", h.id, err)
		}
	}
	return nil
}

// nullableIf returns v when write is true and NULL otherwise, so a single
// UPDATE with COALESCE leaves untouched columns untouched.
func nullableIf(write bool, v string) any {
	if !write {
		return nil
	}
	return v
}
