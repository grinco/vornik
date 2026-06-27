package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AdminAuditRepository implements persistence.AdminAuditRepository
// against PostgreSQL. Mirrors the tool-audit repository's shape so
// operator instinct from /ui/audit transfers cleanly to /ui/admin/audit.
//
// before_state / after_state are JSONB on the column side. Go-side
// they're plain strings — the admin-UI handler writes whatever
// representation it needs (small JSON object for config edits,
// empty string for no-snapshot actions like mcp.refresh) and the
// Insert path quotes it as TEXT cast to JSONB. NULLs are written
// when the string is empty so the column doesn't accumulate "" rows.
type AdminAuditRepository struct {
	db DBTX
}

// NewAdminAuditRepository constructs an AdminAuditRepository over db.
func NewAdminAuditRepository(db DBTX) *AdminAuditRepository {
	return &AdminAuditRepository{db: db}
}

// Insert writes one admin-audit row. Timestamp defaults to NOW()
// when the caller leaves entry.Timestamp zero. before_state /
// after_state are stored as JSONB; empty string becomes NULL.
// IP is NULL when entry.IP is empty so the INET column doesn't
// reject "" (postgres parses "" as an invalid INET literal).
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
	beforeArg := jsonbOrNull(entry.Before)
	afterArg := jsonbOrNull(entry.After)
	ipArg := textOrNull(entry.IP)

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO admin_audit (
			id, ts, principal, source, action, target,
			before_state, after_state, ip, user_agent
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		entry.ID, ts, entry.Principal, entry.Source, entry.Action,
		entry.Target, beforeArg, afterArg, ipArg, entry.UserAgent,
	)
	return mapDBError(err)
}

// List returns rows matching the filter, newest first. PageSize must
// be > 0 — unbounded scans on this table are a footgun, so the
// repo refuses them.
func (r *AdminAuditRepository) List(ctx context.Context, filter persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	if filter.PageSize <= 0 {
		return nil, fmt.Errorf("admin_audit: PageSize must be > 0")
	}
	var b strings.Builder
	b.WriteString(`
		SELECT id, ts, principal, source, action, target,
		       COALESCE(before_state::text, ''),
		       COALESCE(after_state::text, ''),
		       COALESCE(host(ip), ''),
		       user_agent
		FROM admin_audit WHERE 1=1`)
	args := make([]any, 0, 6)
	argPos := 1

	if filter.Action != "" {
		fmt.Fprintf(&b, " AND action = $%d", argPos)
		args = append(args, filter.Action)
		argPos++
	}
	if filter.Principal != "" {
		fmt.Fprintf(&b, " AND principal = $%d", argPos)
		args = append(args, filter.Principal)
		argPos++
	}
	if filter.TargetPrefix != "" {
		fmt.Fprintf(&b, ` AND target LIKE $%d ESCAPE '\'`, argPos)
		// LIKE-escape the operator-supplied prefix so a stray %
		// or _ doesn't widen the filter unexpectedly. Explicit
		// ESCAPE clause keeps the behaviour identical to SQLite,
		// where the default LIKE has no escape character.
		args = append(args, escapeLikePrefix(filter.TargetPrefix)+"%")
		argPos++
	}
	if !filter.Since.IsZero() {
		fmt.Fprintf(&b, " AND ts >= $%d", argPos)
		args = append(args, filter.Since)
		argPos++
	}
	if !filter.Until.IsZero() {
		fmt.Fprintf(&b, " AND ts < $%d", argPos)
		args = append(args, filter.Until)
		argPos++
	}

	fmt.Fprintf(&b, " ORDER BY ts DESC LIMIT $%d", argPos)
	args = append(args, filter.PageSize)
	argPos++
	if filter.Offset > 0 {
		fmt.Fprintf(&b, " OFFSET $%d", argPos)
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.AdminAuditEntry
	for rows.Next() {
		var e persistence.AdminAuditEntry
		if err := rows.Scan(
			&e.ID, &e.Timestamp, &e.Principal, &e.Source, &e.Action,
			&e.Target, &e.Before, &e.After, &e.IP, &e.UserAgent,
		); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// jsonbOrNull returns sql.NullString{Valid:false} for empty input so
// the JSONB column stays NULL — a zero-length string isn't valid JSON
// and postgres would reject it with a syntax error if cast.
func jsonbOrNull(s string) any {
	if s == "" {
		return sql.NullString{}
	}
	return s
}

// textOrNull returns NULL for an empty INET-bound argument. lib/pq's
// INET driver chokes on "" because it isn't a parseable address.
func textOrNull(s string) any {
	if s == "" {
		return sql.NullString{}
	}
	return s
}

// escapeLikePrefix doubles every LIKE wildcard metacharacter so the
// supplied prefix is matched literally. ESCAPE clauses aren't needed
// because postgres treats backslash as the default escape character.
func escapeLikePrefix(in string) string {
	r := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return r.Replace(in)
}
