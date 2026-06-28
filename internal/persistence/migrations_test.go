package persistence

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestMigrationStruct tests Migration struct fields.
func TestMigrationStruct(t *testing.T) {
	m := Migration{
		Version: 1,
		Name:    "test_migration",
		Up:      "CREATE TABLE test (id INT)",
		Down:    "DROP TABLE test",
	}

	if m.Version != 1 {
		t.Errorf("expected Version 1, got %d", m.Version)
	}
	if m.Name != "test_migration" {
		t.Errorf("expected Name 'test_migration', got '%s'", m.Name)
	}
	if m.Up != "CREATE TABLE test (id INT)" {
		t.Errorf("unexpected Up: %s", m.Up)
	}
	if m.Down != "DROP TABLE test" {
		t.Errorf("unexpected Down: %s", m.Down)
	}
}

// TestMigrationWithoutDown tests migration without down migration.
func TestMigrationWithoutDown(t *testing.T) {
	m := Migration{
		Version: 1,
		Name:    "irreversible",
		Up:      "CREATE TABLE test (id INT)",
		Down:    "",
	}

	if m.Down != "" {
		t.Error("expected empty Down")
	}
}

// TestDefaultMigrations verifies default migrations exist.
func TestDefaultMigrations(t *testing.T) {
	if len(DefaultMigrations) == 0 {
		t.Error("expected at least one default migration")
	}

	// Check first migration
	m0 := DefaultMigrations[0]
	if m0.Version != 1 {
		t.Errorf("expected first migration version 1, got %d", m0.Version)
	}
	if m0.Name != "initial_schema" {
		t.Errorf("expected first migration name 'initial_schema', got '%s'", m0.Name)
	}
	if m0.Up == "" {
		t.Error("expected non-empty Up SQL")
	}
	if m0.Down == "" {
		t.Error("expected non-empty Down SQL")
	}
}

// TestMigration83_WorkflowProposalsKind pins the §8.5 additive
// `kind` column migration: present, additive (ADD COLUMN IF NOT
// EXISTS), defaulted to the 'unspecified' sentinel (so existing rows
// satisfy NOT NULL), indexed, and reversible.
func TestMigration83_WorkflowProposalsKind(t *testing.T) {
	var m *Migration
	for i := range DefaultMigrations {
		if DefaultMigrations[i].Version == 83 {
			m = &DefaultMigrations[i]
			break
		}
	}
	if m == nil {
		t.Fatal("migration 83 not found")
	}
	if m.Name != "workflow_proposals_kind" {
		t.Errorf("name = %q", m.Name)
	}
	for _, want := range []string{
		"ADD COLUMN IF NOT EXISTS kind",
		"DEFAULT 'unspecified'",
		"idx_workflow_proposals_kind",
	} {
		if !strings.Contains(m.Up, want) {
			t.Errorf("Up missing %q", want)
		}
	}
	if !strings.Contains(m.Down, "DROP COLUMN IF EXISTS kind") {
		t.Errorf("Down should drop the column: %q", m.Down)
	}
}

func findMigration(t *testing.T, version int) *Migration {
	t.Helper()
	for i := range DefaultMigrations {
		if DefaultMigrations[i].Version == version {
			return &DefaultMigrations[i]
		}
	}
	t.Fatalf("migration %d not found", version)
	return nil
}

func TestMigration85_CreateInstincts(t *testing.T) {
	m := findMigration(t, 85)
	if m.Name != "create_instincts" {
		t.Errorf("name = %q", m.Name)
	}
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS instincts",
		"trigger_key",
		"trigger_json", // NOT the reserved word `trigger`
		"idx_instincts_dedup",
		"idx_instincts_project_domain",
		"idx_instincts_status_confidence",
	} {
		if !strings.Contains(m.Up, want) {
			t.Errorf("Up missing %q", want)
		}
	}
	// The dedup index is the upsert atomicity key — keep it unique.
	if !strings.Contains(m.Up, "UNIQUE INDEX IF NOT EXISTS idx_instincts_dedup") {
		t.Errorf("idx_instincts_dedup must be UNIQUE")
	}
	if !strings.Contains(m.Down, "DROP TABLE IF EXISTS instincts") {
		t.Errorf("Down should drop the instincts table: %q", m.Down)
	}
}

func TestMigration86_CreateInstinctEvidenceAndApplications(t *testing.T) {
	m := findMigration(t, 86)
	if m.Name != "create_instinct_evidence_and_applications" {
		t.Errorf("name = %q", m.Name)
	}
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS instinct_evidence",
		"PRIMARY KEY (instinct_id, outcome_id)", // idempotency key
		"REFERENCES instincts(id) ON DELETE CASCADE",
		"CREATE TABLE IF NOT EXISTS instinct_applications",
		"applied_at", // NOT the reserved word `at`
		"idx_instinct_applications_instinct",
	} {
		if !strings.Contains(m.Up, want) {
			t.Errorf("Up missing %q", want)
		}
	}
	for _, want := range []string{
		"DROP TABLE IF EXISTS instinct_applications",
		"DROP TABLE IF EXISTS instinct_evidence",
	} {
		if !strings.Contains(m.Down, want) {
			t.Errorf("Down missing %q", want)
		}
	}
}

func TestMigration87_CreateWorkflowHealingCandidates(t *testing.T) {
	m := findMigration(t, 87)
	if m.Name != "create_workflow_healing_candidates" {
		t.Errorf("name = %q", m.Name)
	}
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS workflow_healing_candidates",
		// Hard FK to the trigger ledger, cascade so dismissing a
		// trigger cleans up its candidates.
		"REFERENCES workflow_healing_triggers(id) ON DELETE CASCADE",
		"proposal_id", // soft FK to the proposals tree (no DB constraint)
		"candidate_genome_hash",
		"proposal_diff",
		// Status lifecycle CHECK — the promote/reject path depends on it.
		"CHECK (status IN ('draft','trial_running','trial_passed','trial_failed','rejected','promoted'))",
		"idx_healing_candidates_trigger",
		"idx_healing_candidates_project_time",
		"idx_healing_candidates_proposal",
	} {
		if !strings.Contains(m.Up, want) {
			t.Errorf("Up missing %q", want)
		}
	}
	if !strings.Contains(m.Down, "DROP TABLE IF EXISTS workflow_healing_candidates") {
		t.Errorf("Down should drop the candidates table: %q", m.Down)
	}
}

func TestMigration88_CreateWorkflowHealingTrials(t *testing.T) {
	m := findMigration(t, 88)
	if m.Name != "create_workflow_healing_trials" {
		t.Errorf("name = %q", m.Name)
	}
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS workflow_healing_trials",
		"REFERENCES workflow_healing_candidates(id) ON DELETE CASCADE",
		// The three trial modes + the five verdicts the runner emits.
		"CHECK (mode IN ('static','replay','shadow'))",
		"CHECK (verdict IN ('pending','passed','failed','inconclusive','errored'))",
		// JSONB result columns the runner writes (column names; the SQL
		// pads alignment so don't pin the exact `name JSONB` spacing).
		"evidence_execution_ids",
		"baseline_summary",
		"candidate_summary",
		"scorecard",
		"JSONB NOT NULL DEFAULT '{}'::jsonb",
		"idx_healing_trials_candidate",
	} {
		if !strings.Contains(m.Up, want) {
			t.Errorf("Up missing %q", want)
		}
	}
	if !strings.Contains(m.Down, "DROP TABLE IF EXISTS workflow_healing_trials") {
		t.Errorf("Down should drop the trials table: %q", m.Down)
	}
}

func TestMigration90_IdentityCore(t *testing.T) {
	m := findMigration(t, 90)
	if m.Name != "identity_core" {
		t.Errorf("name = %q", m.Name)
	}
	// All six tables must be created.
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS users",
		"CREATE TABLE IF NOT EXISTS groups",
		"CREATE TABLE IF NOT EXISTS group_projects",
		"CREATE TABLE IF NOT EXISTS group_members",
		"CREATE TABLE IF NOT EXISTS user_identities",
		"CREATE TABLE IF NOT EXISTS link_codes",
	} {
		if !strings.Contains(m.Up, want) {
			t.Errorf("Up missing %q", want)
		}
	}
	// Key constraints.
	if !strings.Contains(m.Up, "UNIQUE (channel, external_id)") {
		t.Errorf("Up missing UNIQUE (channel, external_id)")
	}
	if !strings.Contains(m.Up, "CHECK (role IN ('admin', 'user'))") {
		t.Errorf("Up missing CHECK (role IN ('admin', 'user'))")
	}
	// All three reverse-lookup indexes must be present.
	for _, want := range []string{
		"idx_group_members_user",
		"idx_user_identities_user",
		"idx_link_codes_user",
	} {
		if !strings.Contains(m.Up, want) {
			t.Errorf("Up missing index %q", want)
		}
	}
	// Down must drop the six tables in reverse dependency order:
	// link_codes → user_identities → group_members → group_projects → groups → users.
	drops := []string{
		"DROP TABLE IF EXISTS link_codes",
		"DROP TABLE IF EXISTS user_identities",
		"DROP TABLE IF EXISTS group_members",
		"DROP TABLE IF EXISTS group_projects",
		"DROP TABLE IF EXISTS groups",
		"DROP TABLE IF EXISTS users",
	}
	for _, want := range drops {
		if !strings.Contains(m.Down, want) {
			t.Errorf("Down missing %q", want)
		}
	}
	// Verify relative ordering of the DROP statements.
	for i := 0; i < len(drops)-1; i++ {
		posA := strings.Index(m.Down, drops[i])
		posB := strings.Index(m.Down, drops[i+1])
		if posA > posB {
			t.Errorf("Down: %q must appear before %q", drops[i], drops[i+1])
		}
	}
}

// TestMigrationInfoStruct tests MigrationInfo struct.
func TestMigrationInfoStruct(t *testing.T) {
	applied := "2024-01-01T00:00:00Z"
	info := MigrationInfo{
		Version:   1,
		Name:      "test",
		AppliedAt: &applied,
	}

	if info.Version != 1 {
		t.Errorf("expected Version 1, got %d", info.Version)
	}
	if info.Name != "test" {
		t.Errorf("expected Name 'test', got '%s'", info.Name)
	}
	if info.AppliedAt == nil || *info.AppliedAt != applied {
		t.Errorf("expected AppliedAt '%s', got %v", applied, info.AppliedAt)
	}
}

// TestMigrationStatusStruct tests MigrationStatus struct.
func TestMigrationStatusStruct(t *testing.T) {
	status := &MigrationStatus{
		CurrentVersion: 1,
		Applied: []MigrationInfo{
			{Version: 1, Name: "initial_schema"},
		},
		Pending: []MigrationInfo{
			{Version: 2, Name: "add_users"},
		},
	}

	if status.CurrentVersion != 1 {
		t.Errorf("expected CurrentVersion 1, got %d", status.CurrentVersion)
	}
	if len(status.Applied) != 1 {
		t.Errorf("expected 1 applied migration, got %d", len(status.Applied))
	}
	if len(status.Pending) != 1 {
		t.Errorf("expected 1 pending migration, got %d", len(status.Pending))
	}
}

func TestMigrationRunnerVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(7))

	runner := NewMigrationRunner(db)
	version, err := runner.Version(context.Background())
	if err != nil {
		t.Fatalf("Version() error = %v", err)
	}
	if version != 7 {
		t.Fatalf("Version() = %d, want 7", version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMigrationRunnerStatusSplitsAppliedAndPending(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Status() does NOT take the migration advisory lock — only
	// Run() does. So no expectMigrationAdvisoryLock here.
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version, name FROM migrations ORDER BY version")).
		WillReturnRows(sqlmock.NewRows([]string{"version", "name"}).AddRow(1, "one"))

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{
		{Version: 1, Name: "one", Up: "SELECT 1", Down: "SELECT 1"},
		{Version: 2, Name: "two", Up: "SELECT 2", Down: "SELECT 2"},
	}

	status, err := runner.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.CurrentVersion != 1 {
		t.Fatalf("CurrentVersion = %d, want 1", status.CurrentVersion)
	}
	if len(status.Applied) != 1 || status.Applied[0].Version != 1 || status.Applied[0].AppliedAt == nil {
		t.Fatalf("Applied = %#v, want version 1 with applied marker", status.Applied)
	}
	if len(status.Pending) != 1 || status.Pending[0].Version != 2 {
		t.Fatalf("Pending = %#v, want version 2", status.Pending)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMigration37_URLLivenessColumns pins the URL-liveness migration's
// shape. The Up SQL must add both columns and the recheck index;
// the Down SQL must drop them. Asserts on the schema-affecting
// strings rather than mocking each statement because Postgres's
// IF NOT EXISTS semantics make the migration self-idempotent.
func TestMigration37_URLLivenessColumns(t *testing.T) {
	var m *Migration
	for i := range DefaultMigrations {
		if DefaultMigrations[i].Version == 37 {
			m = &DefaultMigrations[i]
			break
		}
	}
	if m == nil {
		t.Fatal("migration 37 not found")
	}
	if m.Name != "project_memory_chunks_url_liveness" {
		t.Errorf("name = %q, want project_memory_chunks_url_liveness", m.Name)
	}
	wantUpSubstrings := []string{
		"ALTER TABLE project_memory_chunks",
		"last_checked_at",
		"TIMESTAMPTZ",
		"is_alive",
		"BOOLEAN",
		"idx_project_memory_chunks_url_recheck",
	}
	for _, sub := range wantUpSubstrings {
		if !strings.Contains(m.Up, sub) {
			t.Errorf("Up missing %q", sub)
		}
	}
	wantDownSubstrings := []string{
		"DROP INDEX IF EXISTS idx_project_memory_chunks_url_recheck",
		"DROP COLUMN IF EXISTS is_alive",
		"DROP COLUMN IF EXISTS last_checked_at",
	}
	for _, sub := range wantDownSubstrings {
		if !strings.Contains(m.Down, sub) {
			t.Errorf("Down missing %q", sub)
		}
	}
}

// TestMigration46_TasksChatTurnID — confirms the migration that
// links tasks back to the dispatcher turn that spawned them is
// registered with the expected Up/Down. Tested via string substring
// because the migration's IF NOT EXISTS semantics make it self-
// idempotent and the literal SQL is the audit surface.
func TestMigration46_TasksChatTurnID(t *testing.T) {
	var m *Migration
	for i := range DefaultMigrations {
		if DefaultMigrations[i].Version == 46 {
			m = &DefaultMigrations[i]
			break
		}
	}
	if m == nil {
		t.Fatal("migration 46 not found")
	}
	if m.Name != "tasks_chat_turn_id" {
		t.Errorf("name = %q, want tasks_chat_turn_id", m.Name)
	}
	for _, sub := range []string{
		"ALTER TABLE tasks",
		"ADD COLUMN IF NOT EXISTS chat_turn_id TEXT",
		"CREATE INDEX IF NOT EXISTS idx_tasks_chat_turn_id",
		"WHERE chat_turn_id IS NOT NULL",
	} {
		if !strings.Contains(m.Up, sub) {
			t.Errorf("Up missing %q", sub)
		}
	}
	for _, sub := range []string{
		"DROP INDEX IF EXISTS idx_tasks_chat_turn_id",
		"DROP COLUMN IF EXISTS chat_turn_id",
	} {
		if !strings.Contains(m.Down, sub) {
			t.Errorf("Down missing %q", sub)
		}
	}
}

// TestMigration37_AppliesCleanlyOnFreshSchema simulates the
// migration applying against an empty (post-bootstrap) DB. Verifies
// the Up runs inside a transaction and records the version.
func TestMigration37_AppliesCleanlyOnFreshSchema(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
	mock.ExpectBegin()
	mock.ExpectExec("ALTER TABLE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO migrations (version, name) VALUES ($1, $2)")).
		WithArgs(37, "project_memory_chunks_url_liveness").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunner(db)
	// Trim the migration set to [v1, v37]. The applied set returned above
	// contains v1, so v1 is skipped and v37 is pending — exercising the
	// "apply the next unapplied migration" path against v37's real Up SQL.
	var m37 Migration
	for _, m := range DefaultMigrations {
		if m.Version == 37 {
			m37 = m
			break
		}
	}
	if m37.Version != 37 {
		t.Fatal("migration 37 not found in DefaultMigrations")
	}
	runner.migrations = []Migration{
		{Version: 1, Name: "initial_schema", Up: "SELECT 1", Down: "SELECT 1"},
		m37,
	}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMigrationRunnerRollbackErrorsWhenNoneApplied(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(0))

	runner := NewMigrationRunner(db)
	err = runner.Rollback(context.Background())
	if err == nil || err.Error() != "no migrations to rollback" {
		t.Fatalf("Rollback() error = %v, want no migrations to rollback", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMigrationRunnerRunAppliesPendingMigration(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE example (id INT)")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO migrations (version, name) VALUES ($1, $2)")).
		WithArgs(1, "one").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "one", Up: "CREATE TABLE example (id INT)", Down: "DROP TABLE example"}}

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestTaskStatusEnum tests task status constants.
func TestTaskStatusEnum(t *testing.T) {
	statuses := []TaskStatus{
		TaskStatusPending,
		TaskStatusQueued,
		TaskStatusLeased,
		TaskStatusRunning,
		TaskStatusWaitingForChildren,
		TaskStatusCompleted,
		TaskStatusFailed,
		TaskStatusCancelled,
		TaskStatusAwaitingApproval,
	}

	expected := []string{
		"PENDING", "QUEUED", "LEASED", "RUNNING",
		"WAITING_FOR_CHILDREN", "COMPLETED", "FAILED", "CANCELLED",
		"AWAITING_APPROVAL",
	}

	for i, status := range statuses {
		if string(status) != expected[i] {
			t.Errorf("expected status '%s', got '%s'", expected[i], status)
		}
	}
}

// TestExecutionStatusEnum tests execution status constants.
func TestExecutionStatusEnum(t *testing.T) {
	statuses := []ExecutionStatus{
		ExecutionStatusPending,
		ExecutionStatusRunning,
		ExecutionStatusCompleted,
		ExecutionStatusFailed,
		ExecutionStatusCancelled,
	}

	expected := []string{
		"PENDING", "RUNNING", "COMPLETED", "FAILED", "CANCELLED",
	}

	for i, status := range statuses {
		if string(status) != expected[i] {
			t.Errorf("expected status '%s', got '%s'", expected[i], status)
		}
	}
}

// TestTaskCreationSourceEnum tests task creation source constants.
func TestTaskCreationSourceEnum(t *testing.T) {
	sources := []TaskCreationSource{
		TaskCreationSourceUser,
		TaskCreationSourceDelegation,
		TaskCreationSourceAutonomous,
	}

	expected := []string{"USER", "DELEGATION", "AUTONOMOUS"}

	for i, source := range sources {
		if string(source) != expected[i] {
			t.Errorf("expected source '%s', got '%s'", expected[i], source)
		}
	}
}

// TestDelegationModeEnum tests delegation mode constants.
func TestDelegationModeEnum(t *testing.T) {
	modes := []DelegationMode{
		DelegationModeSequential,
		DelegationModeParallel,
		DelegationModeFanOut,
	}

	expected := []string{"SEQUENTIAL", "PARALLEL", "FAN_OUT"}

	for i, mode := range modes {
		if string(mode) != expected[i] {
			t.Errorf("expected mode '%s', got '%s'", expected[i], mode)
		}
	}
}

// TestArtifactClassEnum tests artifact class constants.
func TestArtifactClassEnum(t *testing.T) {
	classes := []ArtifactClass{
		ArtifactClassOutput,
		ArtifactClassIntermediate,
		ArtifactClassSnapshot,
		ArtifactClassLog,
		ArtifactClassMetadata,
	}

	expected := []string{"OUTPUT", "INTERMEDIATE", "SNAPSHOT", "LOG", "METADATA"}

	for i, class := range classes {
		if string(class) != expected[i] {
			t.Errorf("expected class '%s', got '%s'", expected[i], class)
		}
	}
}

// TestTaskStruct tests Task struct fields.
func TestTaskStruct(t *testing.T) {
	task := Task{
		ID:             "task-123",
		ProjectID:      "proj-1",
		Status:         TaskStatusQueued,
		Priority:       50,
		CreationSource: TaskCreationSourceUser,
		Attempt:        1,
		MaxAttempts:    3,
	}

	if task.ID != "task-123" {
		t.Errorf("expected ID 'task-123', got '%s'", task.ID)
	}
	if task.ProjectID != "proj-1" {
		t.Errorf("expected ProjectID 'proj-1', got '%s'", task.ProjectID)
	}
	if task.Status != TaskStatusQueued {
		t.Errorf("expected Status 'QUEUED', got '%s'", task.Status)
	}
	if task.Priority != 50 {
		t.Errorf("expected Priority 50, got %d", task.Priority)
	}
}

// TestTaskStruct_ChatTurnID — migration v46 field. Verifies the
// pointer carries the dispatcher turn id and JSON-serialises with
// the expected lowercase key.
func TestTaskStruct_ChatTurnID(t *testing.T) {
	turn := "chat_20260521190824_aaaa"
	task := Task{ID: "task-1", ProjectID: "p", ChatTurnID: &turn}
	if task.ChatTurnID == nil || *task.ChatTurnID != turn {
		t.Fatalf("ChatTurnID = %v, want %s", task.ChatTurnID, turn)
	}
	// Confirm the omitempty tag absorbs a nil; the JSON shape is
	// what /ui/admin/chat-audit + retention scripts read.
	bareJSON, err := json.Marshal(Task{ID: "task-bare", ProjectID: "p"})
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	if strings.Contains(string(bareJSON), "chat_turn_id") {
		t.Errorf("bare task should omit chat_turn_id, got %s", bareJSON)
	}
	taggedJSON, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal tagged: %v", err)
	}
	if !strings.Contains(string(taggedJSON), `"chat_turn_id":"`+turn+`"`) {
		t.Errorf("tagged JSON missing chat_turn_id: %s", taggedJSON)
	}
}

// TestExecutionStruct tests Execution struct fields.
func TestExecutionStruct(t *testing.T) {
	exec := Execution{
		ID:         "exec-123",
		TaskID:     "task-123",
		ProjectID:  "proj-1",
		WorkflowID: "wf-1",
		Status:     ExecutionStatusRunning,
	}

	if exec.ID != "exec-123" {
		t.Errorf("expected ID 'exec-123', got '%s'", exec.ID)
	}
	if exec.TaskID != "task-123" {
		t.Errorf("expected TaskID 'task-123', got '%s'", exec.TaskID)
	}
}

// TestArtifactStruct tests Artifact struct fields.
func TestArtifactStruct(t *testing.T) {
	artifact := Artifact{
		ID:            "art-123",
		ProjectID:     "proj-1",
		Name:          "output.json",
		ArtifactClass: ArtifactClassOutput,
		StoragePath:   "/artifacts/output.json",
	}

	if artifact.ID != "art-123" {
		t.Errorf("expected ID 'art-123', got '%s'", artifact.ID)
	}
	if artifact.Name != "output.json" {
		t.Errorf("expected Name 'output.json', got '%s'", artifact.Name)
	}
}

// TestTaskFilterStruct tests TaskFilter struct.
func TestTaskFilterStruct(t *testing.T) {
	status := TaskStatusQueued
	projectID := "proj-1"

	filter := TaskFilter{
		ProjectID: &projectID,
		Status:    &status,
		PageSize:  100,
		Offset:    0,
	}

	if filter.ProjectID == nil || *filter.ProjectID != projectID {
		t.Errorf("expected ProjectID '%s'", projectID)
	}
	if filter.Status == nil || *filter.Status != status {
		t.Errorf("expected Status '%s'", status)
	}
	if filter.PageSize != 100 {
		t.Errorf("expected PageSize 100, got %d", filter.PageSize)
	}
}

// TestRepositoryErrors tests repository error constants.
func TestRepositoryErrors(t *testing.T) {
	errors := []RepositoryError{
		ErrNotFound,
		ErrDuplicateKey,
		ErrOptimisticLock,
		ErrNoTasksAvailable,
		ErrLeaseNotFound,
		ErrLeaseExpired,
	}

	expected := []string{
		"not found",
		"duplicate key",
		"optimistic lock conflict",
		"no tasks available",
		"lease not found",
		"lease expired",
	}

	for i, err := range errors {
		if err.Error() != expected[i] {
			t.Errorf("expected error '%s', got '%s'", expected[i], err.Error())
		}
	}
}

// TestLeaseOptionsStruct tests LeaseOptions struct.
func TestLeaseOptionsStruct(t *testing.T) {
	opts := LeaseOptions{
		ProjectID:            "proj-1",
		LeaseHolder:          "executor-1",
		LeaseDurationSeconds: 300,
		PriorityFloor:        25,
	}

	if opts.ProjectID != "proj-1" {
		t.Errorf("expected ProjectID 'proj-1', got '%s'", opts.ProjectID)
	}
	if opts.LeaseHolder != "executor-1" {
		t.Errorf("expected LeaseHolder 'executor-1', got '%s'", opts.LeaseHolder)
	}
	if opts.LeaseDurationSeconds != 300 {
		t.Errorf("expected LeaseDurationSeconds 300, got %d", opts.LeaseDurationSeconds)
	}
}

// TestMigration91_UISessions verifies the ui_sessions migration schema.
func TestMigration91_UISessions(t *testing.T) {
	m := findMigration(t, 91)
	if m.Name != "ui_sessions" {
		t.Errorf("name = %q", m.Name)
	}
	// Table must be created.
	if !strings.Contains(m.Up, "CREATE TABLE IF NOT EXISTS ui_sessions") {
		t.Errorf("Up missing CREATE TABLE IF NOT EXISTS ui_sessions")
	}
	// token_hash must be UNIQUE.
	if !strings.Contains(m.Up, "token_hash    TEXT NOT NULL UNIQUE") {
		t.Errorf("Up missing token_hash TEXT NOT NULL UNIQUE")
	}
	// Spec-mandated NOT NULLs: a session without an expiry or
	// provider is a policy hole, not a nullable nicety.
	if !strings.Contains(m.Up, "expires_at    TIMESTAMP WITH TIME ZONE NOT NULL") {
		t.Errorf("Up missing expires_at NOT NULL")
	}
	if !strings.Contains(m.Up, "provider      TEXT NOT NULL") {
		t.Errorf("Up missing provider NOT NULL")
	}
	if !strings.Contains(m.Up, "last_seen_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()") {
		t.Errorf("Up missing last_seen_at NOT NULL DEFAULT NOW()")
	}
	// Partial index on active sessions by hash.
	if !strings.Contains(m.Up, "idx_ui_sessions_hash") {
		t.Errorf("Up missing idx_ui_sessions_hash")
	}
	if !strings.Contains(m.Up, "WHERE revoked_at IS NULL") {
		t.Errorf("Up missing partial index condition WHERE revoked_at IS NULL")
	}
	// Reverse-lookup index on user_id.
	if !strings.Contains(m.Up, "idx_ui_sessions_user") {
		t.Errorf("Up missing idx_ui_sessions_user")
	}
	// FK to users with CASCADE delete.
	if !strings.Contains(m.Up, "REFERENCES users(id) ON DELETE CASCADE") {
		t.Errorf("Up missing REFERENCES users(id) ON DELETE CASCADE")
	}
	// Down must drop the table.
	if !strings.Contains(m.Down, "DROP TABLE IF EXISTS ui_sessions") {
		t.Errorf("Down missing DROP TABLE IF EXISTS ui_sessions")
	}
}

// TestReleaseOptionsStruct tests ReleaseOptions struct.
func TestReleaseOptionsStruct(t *testing.T) {
	opts := ReleaseOptions{
		Attempt:     2,
		MaxAttempts: 5,
		Error:       "connection timeout",
	}

	if opts.Attempt != 2 {
		t.Errorf("expected Attempt 2, got %d", opts.Attempt)
	}
	if opts.MaxAttempts != 5 {
		t.Errorf("expected MaxAttempts 5, got %d", opts.MaxAttempts)
	}
	if opts.Error != "connection timeout" {
		t.Errorf("expected Error 'connection timeout', got '%s'", opts.Error)
	}
}

// TestMigration109_TradingExecReconcileColumns pins the Postgres
// migration that closes the model/SQLite-vs-Postgres gap left by commit
// f361e99d (FilledQty + TradingFill exec/source columns shipped to the
// model + sqlite schema only). Without it the reconcile loop's persist
// failed every tick on prod with `column "filled_qty" of relation
// "trading_orders" does not exist` (incident 2026-06-27). Additive,
// idempotent, and reversible.
func TestMigration109_TradingExecReconcileColumns(t *testing.T) {
	m := findMigration(t, 109)
	if m.Name != "trading_exec_reconcile_columns" {
		t.Errorf("name = %q", m.Name)
	}
	for _, want := range []string{
		"ALTER TABLE trading_orders ADD COLUMN IF NOT EXISTS filled_qty",
		"ALTER TABLE trading_fills ADD COLUMN IF NOT EXISTS exec_id",
		"ALTER TABLE trading_fills ADD COLUMN IF NOT EXISTS account_id",
		"ALTER TABLE trading_fills ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'reconcile'",
		"ALTER TABLE trading_fills ADD COLUMN IF NOT EXISTS source_detail",
		"CREATE TABLE IF NOT EXISTS trading_fills_shadow",
		"recorded_at",
	} {
		if !strings.Contains(m.Up, want) {
			t.Errorf("Up missing %q", want)
		}
	}
	// Reversible: every added column + the shadow table is dropped.
	for _, want := range []string{
		"DROP TABLE IF EXISTS trading_fills_shadow",
		"ALTER TABLE trading_fills DROP COLUMN IF EXISTS source_detail",
		"ALTER TABLE trading_fills DROP COLUMN IF EXISTS source",
		"ALTER TABLE trading_fills DROP COLUMN IF EXISTS account_id",
		"ALTER TABLE trading_fills DROP COLUMN IF EXISTS exec_id",
		"ALTER TABLE trading_orders DROP COLUMN IF EXISTS filled_qty",
	} {
		if !strings.Contains(m.Down, want) {
			t.Errorf("Down missing %q", want)
		}
	}
}
