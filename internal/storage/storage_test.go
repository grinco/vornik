package storage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// stubDBTX satisfies persistence.DBTX with no-op methods so Build()
// can run without a live database. The repository constructors only
// store the DBTX in a private field — they don't issue queries at
// construction — so this is enough to exercise the wiring.
type stubDBTX struct{ persistence.DBTX }

func TestBuild_AllRepositoriesPopulated(t *testing.T) {
	repos := Build(stubDBTX{})
	if repos == nil {
		t.Fatal("Build returned nil")
	}

	rv := reflect.ValueOf(*repos)
	for i := 0; i < rv.NumField(); i++ {
		field := rv.Field(i)
		name := rv.Type().Field(i).Name
		if field.IsNil() {
			t.Errorf("Repositories.%s is nil — Build() forgot to wire it", name)
		}
	}
}

func TestOpen_DefaultsToPostgres(t *testing.T) {
	cfg := config.DatabaseConfig{
		// Driver omitted intentionally — should default to "postgres".
		Host:    "127.0.0.1",
		Port:    1, // unreachable, forces Connect error
		Name:    "vornik",
		User:    "vornik",
		SSLMode: "disable",
	}
	_, err := Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected an error when connecting to an unreachable host")
	}
	if !strings.Contains(err.Error(), "connect postgres") {
		t.Errorf("expected postgres-connect error, got: %v", err)
	}
}

func TestOpen_SQLiteReturnsRepositories(t *testing.T) {
	cfg := config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}
	backend, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(sqlite :memory:): %v", err)
	}
	defer func() { _ = backend.Close() }()

	if backend.PG != nil {
		t.Errorf("backend.PG should be nil on SQLite, got %v", backend.PG)
	}
	if backend.Repos == nil {
		t.Fatal("backend.Repos is nil")
	}
	// postgresOnly lists Repositories fields the SQLite branch
	// intentionally leaves nil — no SQLite mirror exists for them.
	// Identity (migration 90 identity core) is postgres-only per
	// oidc-identity-permissions-design.md / the Phase-2 plan: the
	// identity tables ship on Postgres alongside most of the schema,
	// and authz (its only consumer) runs against the Postgres backend.
	postgresOnly := map[string]bool{
		"Identity":   true,
		"UISessions": true,
	}
	rv := reflect.ValueOf(*backend.Repos)
	for i := 0; i < rv.NumField(); i++ {
		field := rv.Field(i)
		name := rv.Type().Field(i).Name
		if postgresOnly[name] {
			if !field.IsNil() {
				t.Errorf("Repositories.%s should be nil on SQLite (postgres-only), got non-nil", name)
			}
			continue
		}
		if field.IsNil() {
			t.Errorf("Repositories.%s is nil on SQLite", name)
		}
	}
}

func TestOpen_SQLiteFactoryLeaseSkipsBlockedRows(t *testing.T) {
	ctx := context.Background()
	backend, err := Open(ctx, config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open(sqlite :memory:): %v", err)
	}
	defer func() { _ = backend.Close() }()

	for i := 0; i < 70; i++ {
		task := &persistence.Task{
			ID:           persistence.GenerateID("blocked"),
			ProjectID:    "factory-project",
			Status:       persistence.TaskStatusQueued,
			Priority:     50,
			Payload:      []byte(`{}`),
			Dependencies: []string{persistence.GenerateID("missing")},
		}
		if err := backend.Repos.Tasks.Create(ctx, task); err != nil {
			t.Fatalf("Create blocked %d: %v", i, err)
		}
	}
	ready := &persistence.Task{
		ID:        persistence.GenerateID("ready"),
		ProjectID: "factory-project",
		Status:    persistence.TaskStatusQueued,
		Priority:  50,
		Payload:   []byte(`{}`),
	}
	if err := backend.Repos.Tasks.Create(ctx, ready); err != nil {
		t.Fatalf("Create ready: %v", err)
	}

	leased, err := backend.Repos.Tasks.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID:            "factory-project",
		LeaseHolder:          "storage-test",
		LeaseDurationSeconds: 300,
	})
	if err != nil {
		if errors.Is(err, persistence.ErrNoTasksAvailable) {
			t.Fatalf("factory-backed SQLite lease starved behind blocked rows: %v", err)
		}
		t.Fatalf("LeaseTask: %v", err)
	}
	if leased.ID != ready.ID {
		t.Fatalf("leased %s, want %s", leased.ID, ready.ID)
	}
}

func TestOpen_RejectsUnknownDriver(t *testing.T) {
	cfg := config.DatabaseConfig{Driver: "mysql"}
	_, err := Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected an error for unknown driver")
	}
	if !strings.Contains(err.Error(), "unsupported database driver") {
		t.Errorf("expected unsupported-driver message, got: %v", err)
	}
}
