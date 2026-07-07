package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SkillRepository is the SQLite persistence.SkillRepository — the
// knowledge-skill store (LLD 2026-07-07-knowledge-skill-store-design).
// repo_scope "" is persisted as NULL (uncategorized); tags/roles are
// JSON TEXT via sqliteStringArray.
type SkillRepository struct {
	db DBTX
}

// NewSkillRepository constructs a SkillRepository over db.
func NewSkillRepository(db DBTX) *SkillRepository { return &SkillRepository{db: db} }

const skillColumns = `id, project_id, repo_scope, name, description, body, body_sha256,
	domain, tags, roles, maturity, version, origin_client, origin_task, author,
	usage_fired, usage_worked, usage_corrected, last_fired_at, created_at, updated_at`

// scopeArg maps the Go "" convention to a NULL column value.
func scopeArg(scope string) interface{} {
	if scope == "" {
		return nil
	}
	return scope
}

// Create inserts a new skill, returning ErrSkillNameConflict on a duplicate
// (project_id, repo_scope, name).
func (r *SkillRepository) Create(ctx context.Context, s *persistence.Skill) error {
	if _, err := r.Get(ctx, s.ProjectID, s.RepoScope, s.Name); err == nil {
		return persistence.ErrSkillNameConflict
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return err
	}
	return r.insert(ctx, s)
}

func (r *SkillRepository) insert(ctx context.Context, s *persistence.Skill) error {
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}
	if s.Maturity == "" {
		s.Maturity = persistence.SkillMaturityDraft
	}
	if s.Version == 0 {
		s.Version = 1
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_skills (`+skillColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.ProjectID, scopeArg(s.RepoScope), s.Name, s.Description, s.Body, s.BodySHA256,
		nullStr(s.Domain), sqliteStringArray(s.Tags), sqliteStringArray(s.Roles),
		s.Maturity, s.Version, nullStr(s.OriginClient), nullStr(s.OriginTask), nullStr(s.Author),
		s.UsageFired, s.UsageWorked, s.UsageCorrected, sqliteTimePtr(s.LastFiredAt),
		sqliteTime(s.CreatedAt), sqliteTime(s.UpdatedAt),
	)
	return err
}

// Upsert inserts a skill or, when its natural key already exists, bumps the
// version, replaces the mutable fields, and resets maturity to draft.
func (r *SkillRepository) Upsert(ctx context.Context, s *persistence.Skill) (*persistence.Skill, error) {
	existing, err := r.Get(ctx, s.ProjectID, s.RepoScope, s.Name)
	if errors.Is(err, persistence.ErrNotFound) {
		if err := r.insert(ctx, s); err != nil {
			return nil, err
		}
		return r.Get(ctx, s.ProjectID, s.RepoScope, s.Name)
	}
	if err != nil {
		return nil, err
	}
	// Edit-in-place: bump version, replace mutable fields, reset to
	// draft (an edited body must be re-approved).
	_, err = r.db.ExecContext(ctx, `
		UPDATE project_skills SET
			description = ?, body = ?, body_sha256 = ?, domain = ?,
			tags = ?, roles = ?, maturity = ?, version = ?,
			origin_client = ?, origin_task = ?, author = ?, updated_at = ?
		WHERE id = ?`,
		s.Description, s.Body, s.BodySHA256, nullStr(s.Domain),
		sqliteStringArray(s.Tags), sqliteStringArray(s.Roles),
		persistence.SkillMaturityDraft, existing.Version+1,
		nullStr(s.OriginClient), nullStr(s.OriginTask), nullStr(s.Author),
		sqliteTime(time.Now().UTC()), existing.ID,
	)
	if err != nil {
		return nil, err
	}
	return r.GetByID(ctx, existing.ID)
}

// GetByID fetches a skill by primary key, returning ErrNotFound if absent.
func (r *SkillRepository) GetByID(ctx context.Context, id string) (*persistence.Skill, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+skillColumns+` FROM project_skills WHERE id = ?`, id)
	return scanSkillRow(row)
}

// Get fetches a skill by its scope-qualified natural key, returning
// ErrNotFound if absent.
func (r *SkillRepository) Get(ctx context.Context, projectID, repoScope, name string) (*persistence.Skill, error) {
	var (
		q    string
		args []interface{}
	)
	if repoScope == "" {
		q = `SELECT ` + skillColumns + ` FROM project_skills
			WHERE project_id = ? AND repo_scope IS NULL AND name = ?`
		args = []interface{}{projectID, name}
	} else {
		q = `SELECT ` + skillColumns + ` FROM project_skills
			WHERE project_id = ? AND repo_scope = ? AND name = ?`
		args = []interface{}{projectID, repoScope, name}
	}
	return scanSkillRow(r.db.QueryRowContext(ctx, q, args...))
}

// List returns skills matching the filter, newest-updated first.
func (r *SkillRepository) List(ctx context.Context, projectID string, f persistence.SkillListFilter) ([]*persistence.Skill, error) {
	var b strings.Builder
	b.WriteString(`SELECT ` + skillColumns + ` FROM project_skills WHERE project_id = ?`)
	args := []interface{}{projectID}

	if f.RepoScope != "" {
		if f.StrictScope {
			b.WriteString(` AND (repo_scope = ? OR repo_scope = '*')`)
			args = append(args, f.RepoScope)
		} else {
			b.WriteString(` AND (repo_scope = ? OR repo_scope = '*' OR repo_scope IS NULL)`)
			args = append(args, f.RepoScope)
		}
	}
	if len(f.Maturities) > 0 {
		b.WriteString(` AND maturity IN (` + placeholders(len(f.Maturities)) + `)`)
		for _, m := range f.Maturities {
			args = append(args, m)
		}
	}
	if f.Domain != "" {
		b.WriteString(` AND domain = ?`)
		args = append(args, f.Domain)
	}
	if f.Role != "" {
		// roles is a JSON array of quoted strings; an empty roles list
		// ('[]') applies to any role. The quoted LIKE avoids matching a
		// role that is a prefix of another (e.g. "researcher" vs
		// "researcher-lead").
		b.WriteString(` AND (roles = '[]' OR roles LIKE ?)`)
		args = append(args, `%"`+f.Role+`"%`)
	}
	b.WriteString(` ORDER BY updated_at DESC`)
	if f.Limit > 0 {
		b.WriteString(` LIMIT ?`)
		args = append(args, f.Limit)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.Skill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListForMaturityScan returns all active/trusted skills across projects.
func (r *SkillRepository) ListForMaturityScan(ctx context.Context) ([]*persistence.Skill, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+skillColumns+`
		FROM project_skills WHERE maturity IN ('active','trusted')`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.Skill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListDrafts returns all draft skills across projects, oldest first.
func (r *SkillRepository) ListDrafts(ctx context.Context, limit int) ([]*persistence.Skill, error) {
	q := `SELECT ` + skillColumns + ` FROM project_skills WHERE maturity = 'draft' ORDER BY created_at ASC`
	args := []interface{}{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.Skill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetMaturity transitions a skill to the given maturity state.
func (r *SkillRepository) SetMaturity(ctx context.Context, id, maturity string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE project_skills SET maturity = ?, updated_at = ? WHERE id = ?`,
		maturity, sqliteTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	return errIfNoRows(res)
}

// RecordFeedback increments the usage counter for the given signal and, for
// "fired", stamps last_fired_at.
func (r *SkillRepository) RecordFeedback(ctx context.Context, id, signal string) error {
	col, err := skillUsageColumn(signal)
	if err != nil {
		return err
	}
	q := `UPDATE project_skills SET ` + col + ` = ` + col + ` + 1`
	args := []interface{}{}
	if signal == persistence.SkillSignalFired {
		q += `, last_fired_at = ?`
		args = append(args, sqliteTime(time.Now().UTC()))
	}
	q += ` WHERE id = ?`
	args = append(args, id)
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	return errIfNoRows(res)
}

// skillUsageColumn whitelists the signal → column mapping so the
// interpolated column name can never be attacker-controlled.
func skillUsageColumn(signal string) (string, error) {
	switch signal {
	case persistence.SkillSignalFired:
		return "usage_fired", nil
	case persistence.SkillSignalWorked:
		return "usage_worked", nil
	case persistence.SkillSignalCorrected:
		return "usage_corrected", nil
	default:
		return "", fmt.Errorf("sqlite: unknown skill feedback signal %q", signal)
	}
}

type skillScanner interface {
	Scan(dest ...interface{}) error
}

func scanSkillRow(row *sql.Row) (*persistence.Skill, error) {
	s, err := scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	return s, err
}

func scanSkill(sc skillScanner) (*persistence.Skill, error) {
	var (
		s          persistence.Skill
		repoScope  sql.NullString
		domain     sql.NullString
		tags       sqliteStringArray
		roles      sqliteStringArray
		originCl   sql.NullString
		originTask sql.NullString
		author     sql.NullString
		lastFired  sql.NullString
		createdAt  sqlTime
		updatedAt  sqlTime
	)
	if err := sc.Scan(
		&s.ID, &s.ProjectID, &repoScope, &s.Name, &s.Description, &s.Body, &s.BodySHA256,
		&domain, &tags, &roles, &s.Maturity, &s.Version, &originCl, &originTask, &author,
		&s.UsageFired, &s.UsageWorked, &s.UsageCorrected, &lastFired, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	s.RepoScope = repoScope.String
	s.Domain = domain.String
	s.Tags = []string(tags)
	s.Roles = []string(roles)
	s.OriginClient = originCl.String
	s.OriginTask = originTask.String
	s.Author = author.String
	if lastFired.Valid && lastFired.String != "" {
		if t, err := parseSqliteTime(lastFired.String); err == nil {
			s.LastFiredAt = &t
		}
	}
	s.CreatedAt = createdAt.Time
	s.UpdatedAt = updatedAt.Time
	return &s, nil
}

func nullStr(v string) interface{} {
	if v == "" {
		return nil
	}
	return v
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func errIfNoRows(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}
