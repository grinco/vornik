//go:build integration
// +build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// integrationDBName is the canonical default for the integration
// test database. Distinct from the daemon's "vornik_test" so a
// running daemon cannot race test fixtures via project-wide
// LeaseTask. Override with POSTGRES_DB in CI where the image owns
// its own database.
const integrationDBName = "vornik_integration_test"

func getTestDBURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	host := getEnvOrDefault("POSTGRES_HOST", "localhost")
	port := getEnvOrDefault("POSTGRES_PORT", "5432")
	user := getEnvOrDefault("POSTGRES_USER", "vornik")
	pass := getEnvOrDefault("POSTGRES_PASSWORD", "vornik")
	db := getEnvOrDefault("POSTGRES_DB", integrationDBName)
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, pass, host, port, db)
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func connectDB(t *testing.T) *sql.DB {
	dbURL := getTestDBURL()
	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err, "Failed to open database connection")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = db.PingContext(ctx)
	require.NoError(t, err, "Failed to ping database")

	return db
}

func TestDatabase_Connection(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	var result int
	err := db.QueryRow("SELECT 1").Scan(&result)
	assert.NoError(t, err)
	assert.Equal(t, 1, result)
}

func TestTasksTable_CRUD(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	taskID := fmt.Sprintf("test-task-%d", time.Now().UnixNano())

	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, 'project-1', 'QUEUED', 5, 'USER', 1, 3, NOW(), NOW())
	`, taskID)
	require.NoError(t, err, "Failed to insert task")

	var status string
	err = db.QueryRow(`SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status)
	assert.NoError(t, err)
	assert.Equal(t, "QUEUED", status)

	_, err = db.Exec(`UPDATE tasks SET status = 'RUNNING', updated_at = NOW() WHERE id = $1`, taskID)
	assert.NoError(t, err)

	_, err = db.Exec(`DELETE FROM tasks WHERE id = $1`, taskID)
	assert.NoError(t, err)
}

func TestTasksTable_Lease(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	taskID := fmt.Sprintf("lease-test-%d", time.Now().UnixNano())

	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, 'project-1', 'QUEUED', 5, 'USER', 1, 3, NOW(), NOW())
	`, taskID)
	require.NoError(t, err)

	leaseID := fmt.Sprintf("lease-%d", time.Now().UnixNano())

	result, err := db.Exec(`
		UPDATE tasks 
		SET status = 'LEASED', 
		    lease_id = $1, 
		    leased_by = 'executor', 
		    leased_at = NOW(), 
		    lease_expires_at = NOW() + INTERVAL '5 minutes',
		    updated_at = NOW()
		WHERE id = $2 AND status = 'QUEUED'
	`, leaseID, taskID)
	require.NoError(t, err)

	rows, _ := result.RowsAffected()
	assert.Equal(t, int64(1), rows)

	db.Exec(`DELETE FROM tasks WHERE id = $1`, taskID)
}

func TestTasksTable_PriorityOrdering(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	projectID := fmt.Sprintf("priority-project-%d", time.Now().UnixNano())

	for i, priority := range []int{1, 5, 10, 3, 7} {
		taskID := fmt.Sprintf("priority-task-%d-%d", time.Now().UnixNano(), i)
		_, err := db.Exec(`
			INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
			VALUES ($1, $2, 'QUEUED', $3, 'USER', 1, 3, NOW(), NOW())
		`, taskID, projectID, priority)
		require.NoError(t, err)
	}

	rows, err := db.Query(`
		SELECT priority FROM tasks 
		WHERE project_id = $1 AND status = 'QUEUED'
		ORDER BY priority DESC
	`, projectID)
	require.NoError(t, err)
	defer rows.Close()

	var priorities []int
	for rows.Next() {
		var p int
		rows.Scan(&p)
		priorities = append(priorities, p)
	}

	assert.Equal(t, []int{10, 7, 5, 3, 1}, priorities)

	db.Exec(`DELETE FROM tasks WHERE project_id = $1`, projectID)
}

func TestExecutionsTable_CRUD(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	taskID := fmt.Sprintf("exec-task-%d", time.Now().UnixNano())
	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, 'project-1', 'RUNNING', 5, 'USER', 1, 3, NOW(), NOW())
	`, taskID)
	require.NoError(t, err)

	execID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	_, err = db.Exec(`
		INSERT INTO executions (id, task_id, project_id, workflow_id, workflow_revision, status, created_at, updated_at)
		VALUES ($1, $2, 'project-1', 'workflow-1', 'v1', 'RUNNING', NOW(), NOW())
	`, execID, taskID)
	require.NoError(t, err)

	var status string
	err = db.QueryRow(`SELECT status FROM executions WHERE id = $1`, execID).Scan(&status)
	assert.NoError(t, err)
	assert.Equal(t, "RUNNING", status)

	_, err = db.Exec(`UPDATE executions SET status = 'COMPLETED', completed_at = NOW(), updated_at = NOW() WHERE id = $1`, execID)
	assert.NoError(t, err)

	db.Exec(`DELETE FROM executions WHERE id = $1`, execID)
	db.Exec(`DELETE FROM tasks WHERE id = $1`, taskID)
}

func TestArtifactsTable_CRUD(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	artifactID := fmt.Sprintf("artifact-%d", time.Now().UnixNano())
	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec-%d", time.Now().UnixNano())

	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, 'project-1', 'RUNNING', 5, 'USER', 1, 3, NOW(), NOW())
	`, taskID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO executions (id, task_id, project_id, workflow_id, workflow_revision, status, created_at, updated_at)
		VALUES ($1, $2, 'project-1', 'workflow-1', 'v1', 'RUNNING', NOW(), NOW())
	`, execID, taskID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO artifacts (id, project_id, execution_id, task_id, name, artifact_class, storage_path, created_at)
		VALUES ($1, 'project-1', $2, $3, 'output.json', 'OUTPUT', '/artifacts/output.json', NOW())
	`, artifactID, execID, taskID)
	require.NoError(t, err)

	var name, class string
	err = db.QueryRow(`SELECT name, artifact_class FROM artifacts WHERE id = $1`, artifactID).Scan(&name, &class)
	assert.NoError(t, err)
	assert.Equal(t, "output.json", name)
	assert.Equal(t, "OUTPUT", class)

	_, err = db.Exec(`DELETE FROM artifacts WHERE id = $1`, artifactID)
	assert.NoError(t, err)

	_, err = db.Exec(`DELETE FROM executions WHERE id = $1`, execID)
	assert.NoError(t, err)

	_, err = db.Exec(`DELETE FROM tasks WHERE id = $1`, taskID)
	assert.NoError(t, err)
}
