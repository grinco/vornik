package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AdminAuditRepository is the SQLite parity for the postgres
// AdminAuditRepository — same shape, INET dropped to TEXT (SQLite
// has no INET type) and JSONB dropped to TEXT carrying a serialised
// JSON string. Tests + the in-process dev backend exercise it.
type AdminAuditRepository struct {
	db DBTX
}

// NewAdminAuditRepository constructs an AdminAuditRepository over db.
func NewAdminAuditRepository(db DBTX) *AdminAuditRepository {
	return &AdminAuditRepository{db: db}
}

// Insert writes one admin-audit row. Empty Before / After / IP
// collapse to NULL so a query for "rows with no before-state" can
// use the column directly without filtering "".
func (r *AdminAuditRepository) Insert(ctx context.Context, entry *persistence.AdminAuditEntry) error {
	if entry == nil {
		return fmt.Errorf("admin_audit: nil entry")
	}
	if entry.ID == "" {
		entry.ID = persistence.GenerateID("admaud")
	}
	ts := entry.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	beforeArg := nullableString(entry.Before)
	afterArg := nullableString(entry.After)

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO admin_audit (
			id, ts, principal, source, action, target,
			before_state, after_state, ip, user_agent
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, sqliteTime(ts), entry.Principal, entry.Source, entry.Action,
		entry.Target, beforeArg, afterArg, entry.IP, entry.UserAgent,
	)
	return err
}

// List returns rows matching the filter, newest first. PageSize must
// be > 0 — same guard as the postgres impl.
func (r *AdminAuditRepository) List(ctx context.Context, filter persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	if filter.PageSize <= 0 {
		return nil, fmt.Errorf("admin_audit: PageSize must be > 0")
	}
	var b strings.Builder
	b.WriteString(`
		SELECT id, ts, principal, source, action, target,
		       COALESCE(before_state, ''), COALESCE(after_state, ''),
		       COALESCE(ip, ''), user_agent
		FROM admin_audit WHERE 1=1`)
	args := make([]any, 0, 6)

	if filter.Action != "" {
		b.WriteString(" AND action = ?")
		args = append(args, filter.Action)
	}
	if filter.Principal != "" {
		b.WriteString(" AND principal = ?")
		args = append(args, filter.Principal)
	}
	if filter.TargetPrefix != "" {
		// ESCAPE clause needed in SQLite — the default LIKE has no
		// escape character, so a literal % / _ in the operator's
		// prefix would otherwise widen the filter.
		b.WriteString(` AND target LIKE ? ESCAPE '\'`)
		args = append(args, escapeLikePrefix(filter.TargetPrefix)+"%")
	}
	if !filter.Since.IsZero() {
		b.WriteString(" AND ts >= ?")
		args = append(args, sqliteTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		b.WriteString(" AND ts < ?")
		args = append(args, sqliteTime(filter.Until))
	}

	b.WriteString(" ORDER BY ts DESC LIMIT ?")
	args = append(args, filter.PageSize)
	if filter.Offset > 0 {
		b.WriteString(" OFFSET ?")
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.AdminAuditEntry
	for rows.Next() {
		var (
			e  persistence.AdminAuditEntry
			ts sqlTime
		)
		if err := rows.Scan(
			&e.ID, &ts, &e.Principal, &e.Source, &e.Action,
			&e.Target, &e.Before, &e.After, &e.IP, &e.UserAgent,
		); err != nil {
			return nil, err
		}
		e.Timestamp = ts.Time
		out = append(out, &e)
	}
	return out, rows.Err()
}

// nullableString returns NULL for empty input so the column collapses
// rather than carrying an empty-string row that filtered queries
// would still match against COALESCE.
func nullableString(s string) any {
	if s == "" {
		return sql.NullString{}
	}
	return s
}

// escapeLikePrefix doubles SQLite LIKE wildcard chars. SQLite's
// default LIKE doesn't honour an ESCAPE clause unless one is
// declared — and the operator-supplied prefix shouldn't expand
// pattern semantics, so we replace the metacharacters with their
// literal hex-escape form. Backslash also gets doubled so the
// resulting pattern stays consistent if the prefix already
// contained one.
func escapeLikePrefix(in string) string {
	r := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return r.Replace(in)
}
