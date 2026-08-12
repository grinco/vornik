package persistence

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Knowledge-skill store (LLD 2026-07-07-knowledge-skill-store-design).
//
// A knowledge skill is instructional know-how — the agentskills.io
// SKILL.md shape (frontmatter + Markdown body) — that an agent reads
// and applies as guidance. It is DISTINCT from the SWARM-SKILL.md
// capability-skill (workflow+roles) primitive, and shares no storage
// or retrieval path with project RAG memory. Skills are daemon-owned:
// authored from any client, served to swarm roles and every companion
// MCP client.
//
// Maturity lifecycle: draft -> (human approval) -> active -> trusted,
// with decay to retired. Only active/trusted skills are eligible for
// injection. This slice (A+) implements the store + the draft/active
// gate; the usage-signal-driven promotion/decay engine is slice D and
// only reads the usage_* counters this layer already persists.

// Skill maturity states. CHECK-constrained in both backends' schema.
const (
	SkillMaturityDraft   = "draft"
	SkillMaturityActive  = "active"
	SkillMaturityTrusted = "trusted"
	SkillMaturityRetired = "retired"
)

// Skill feedback signals passed to SkillRepository.RecordFeedback.
// Written by the executor-trusted path (SkillMetrics capability);
// consumed by slice D, not read this slice.
const (
	SkillSignalFired     = "fired"
	SkillSignalWorked    = "worked"
	SkillSignalCorrected = "corrected"
)

// ErrSkillNameConflict is returned by Create when a skill with the same
// (project_id, repo_scope, name) already exists. Callers that want
// edit-in-place semantics use Upsert instead.
const ErrSkillNameConflict RepositoryError = "skill name conflict"

// Skill is one knowledge-skill record. RepoScope uses the migration-75
// token convention: "" is persisted as NULL (uncategorized, visible in
// every scope), "*" is cross-cutting, any other value is a repo token.
type Skill struct {
	ID          string
	ProjectID   string
	RepoScope   string
	Name        string
	Description string
	Body        string
	BodySHA256  string
	Domain      string
	Tags        []string
	Roles       []string // swarm roles this skill applies to; empty = any role
	Maturity    string
	Version     int
	// IsGlobal, when true, makes an approved skill inject into EVERY
	// project's roles, not just its home ProjectID. The home project is
	// still ProjectID (the authoring key's project) — is_global only
	// widens injection. Default false.
	IsGlobal     bool
	OriginClient string
	OriginTask   string
	Author       string

	// Embedding backs the propose-time near-duplicate preflight (LLD §12.2).
	// Stored as a JSON-encoded float array in a TEXT column, NOT a pgvector
	// type — §1 keeps this table backend-portable, and cosine is computed in
	// Go over the candidate set. Empty means un-embedded (the lazy-backfill
	// state); EmbeddingModel records which model produced it, because a
	// vector from another model is not comparable and must be recomputed.
	Embedding      []float32
	EmbeddingModel string

	// SupersedesID is set when this skill replaced another via the preflight's
	// `supersedes` disposition. The target is RETIRED, never overwritten — §6
	// binds approval to a body hash, so an approved body must stay readable.
	SupersedesID string
	// DistinctJustification is the required note explaining why this skill
	// exists alongside a near-duplicate the preflight flagged. Surfaced by
	// skill_audit so a bypass is auditable rather than silent.
	DistinctJustification string

	// Usage counters — written by RecordFeedback, not read this slice.
	UsageFired     int64
	UsageWorked    int64
	UsageCorrected int64
	LastFiredAt    *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EncodeSkillVector marshals an embedding for the JSON-TEXT column. A nil or
// empty vector encodes as "" (the un-embedded state) rather than "null"/"[]",
// so "has no embedding" is a single unambiguous value across both backends.
//
// Shared by the Postgres and SQLite repositories deliberately: the column is
// backend-portable by design (LLD §12.2), so its codec must be too — two
// implementations would be two chances to disagree about the wire form.
func EncodeSkillVector(v []float32) string {
	if len(v) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeSkillVector parses the JSON-TEXT embedding column. Any malformed value
// decodes to nil (un-embedded) rather than erroring: a corrupt vector must
// degrade the preflight to its fallback metric, never fail a skill read.
func DecodeSkillVector(s string) []float32 {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var v []float32
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	return v
}

// SkillListFilter narrows List. All fields are optional; the zero value
// lists every skill in the project.
type SkillListFilter struct {
	// RepoScope is the task/caller scope. "" = no scope constraint
	// (project-wide). When set, a row matches if its repo_scope equals
	// RepoScope, equals "*", or is NULL (the last dropped by
	// StrictScope).
	RepoScope   string
	StrictScope bool

	// Maturities restricts to these maturity states; empty = any.
	Maturities []string

	// Domain restricts to an exact domain; "" = any.
	Domain string

	// Role matches skills whose roles list contains Role OR is empty
	// (empty roles = applies to any role). "" = no role constraint.
	Role string

	// IncludeGlobal widens the match to any is_global skill in addition
	// to the caller's projectID (the cross-project injection tier). It is
	// only honored when projectID is non-empty — an empty projectID never
	// widens to all rows (the OR-to-true guard). When false, List is
	// strictly project-scoped as before.
	IncludeGlobal bool

	// Limit caps the result count; <= 0 = unbounded.
	Limit int
}

// SkillRepository is the backend-agnostic contract for the knowledge-
// skill store. Implemented by internal/persistence/{postgres,sqlite}
// and verified by repotest.RunSkillSuite. project_id is always supplied
// by the caller (never trusted from an MCP client arg — the API layer
// binds it to the key's project).
type SkillRepository interface {
	// Create inserts a new skill. Returns ErrSkillNameConflict if
	// (project_id, repo_scope, name) already exists.
	Create(ctx context.Context, s *Skill) error

	// Upsert inserts a new skill, or if (project_id, repo_scope, name)
	// exists, bumps its version, replaces the mutable fields, and
	// resets maturity to draft (an edited body must be re-approved).
	// Returns the stored row.
	Upsert(ctx context.Context, s *Skill) (*Skill, error)

	// GetByID fetches a skill by primary key regardless of scope.
	// Returns ErrNotFound if absent.
	GetByID(ctx context.Context, id string) (*Skill, error)

	// Get fetches a skill by its scope-qualified natural key. Because
	// names are unique only within (project_id, repo_scope), a bare
	// name never returns a cross-scope match. Returns ErrNotFound if
	// absent.
	Get(ctx context.Context, projectID, repoScope, name string) (*Skill, error)

	// List returns skills for a project matching the filter, newest
	// updated first, capped by filter.Limit.
	List(ctx context.Context, projectID string, f SkillListFilter) ([]*Skill, error)

	// SetMaturity transitions a skill to the given maturity state.
	// Returns ErrNotFound if the id is unknown.
	SetMaturity(ctx context.Context, id, maturity string) error

	// RecordFeedback increments the usage counter for signal (fired /
	// worked / corrected) and, for "fired", stamps last_fired_at.
	// Idempotency/dedup by (skill, task, signal) is a caller concern.
	RecordFeedback(ctx context.Context, id, signal string) error

	// ListForMaturityScan returns every active/trusted skill across all
	// projects for the maturity worker to evaluate for promotion/decay.
	// Drafts and retired skills are excluded (the worker never touches
	// them).
	ListForMaturityScan(ctx context.Context) ([]*Skill, error)

	// ListDrafts returns every draft skill across all projects, oldest
	// first, capped by limit (<=0 = unbounded). Powers the operator
	// review digest / inbox.
	ListDrafts(ctx context.Context, limit int) ([]*Skill, error)

	// ListAcrossProjects returns skills from ALL projects in the given
	// maturity states (empty = any maturity), newest-updated first, capped
	// by limit (<=0 = unbounded). Powers the operator skills browser (the
	// admin UI), which surveys the whole store — distinct from ListDrafts
	// (oldest-first review queue) and the per-project List.
	ListAcrossProjects(ctx context.Context, maturities []string, limit int) ([]*Skill, error)

	// CountByMaturity returns row counts across ALL projects keyed by
	// maturity, including retired. Maturities with no rows are absent from
	// the map rather than present-and-zero.
	//
	// This exists so the dashboard "Learning" tile can count without
	// hydrating rows: ListAcrossProjects selects every column, and a skill
	// row carries its full Markdown Body plus a JSON-encoded Embedding.
	// Counting through that on the landing page — the most-requested page,
	// uncached — would read hundreds of KB per render to display three
	// integers.
	CountByMaturity(ctx context.Context) (map[string]int, error)

	// SetGlobal flips a skill's cross-project reach (is_global). Does NOT
	// change maturity — an already-approved skill stays approved and
	// simply widens/narrows where it injects on its next task. Returns
	// ErrNotFound when the id is unknown.
	SetGlobal(ctx context.Context, id string, global bool) error

	// SetEmbedding stores a skill's dedup-preflight vector and the model that
	// produced it (LLD §12.2). Touches ONLY those two columns: this is a lazy
	// backfill of derived data, so it must not bump version, reset maturity,
	// or otherwise disturb an approved skill — re-embedding a trusted skill
	// must never send it back to draft. Returns ErrNotFound for an unknown id.
	SetEmbedding(ctx context.Context, id string, embedding []float32, model string) error

	// ListVersions returns a skill's archived prior bodies, newest first.
	// Empty for a skill that has never been edited. See SkillVersion.
	ListVersions(ctx context.Context, skillID string) ([]*SkillVersion, error)
}

// SkillVersion is one archived prior body of a skill (LLD §12.2).
//
// Upsert archives the existing row here BEFORE overwriting it, so an
// operator-approved body is never destroyed by a re-propose: §6 binds approval
// to a body hash, which is only meaningful while the hashed text still exists.
// The archive is append-only and is not consulted by injection, search, or the
// preflight — it is the audit trail, not part of the working set.
type SkillVersion struct {
	ID          string
	SkillID     string
	Version     int
	Name        string
	Description string
	Body        string
	BodySHA256  string
	// Maturity is the maturity the skill held AT ARCHIVE TIME, which is what
	// makes the record meaningful: archiving a `trusted` body says an operator
	// had approved exactly this text.
	Maturity   string
	ArchivedAt time.Time
}
