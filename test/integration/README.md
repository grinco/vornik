# Integration Tests

This directory contains integration tests for vornik that require external
dependencies like PostgreSQL.

## Running Integration Tests

### Prerequisites

- PostgreSQL database running and accessible
- The `POSTGRES_USER` (default `vornik`) needs `CREATEDB` so the first
  test run can auto-provision the integration database. CI images that
  already provision the DB just set `POSTGRES_DB` to point at it.
- Environment variables configured (or defaults will be used)

### Dedicated test database

Integration tests default to the database `vornik_integration_test` —
**not** the daemon's `vornik_test` (per the standing note that
`vornik_test` is also the dev/prod DB). `TestMain` connects to the
`postgres` admin database, runs `CREATE DATABASE vornik_integration_test`
if it's missing, then applies migrations. After that the test binary
talks only to the dedicated DB.

Because the daemon never touches it, you no longer have to stop the
daemon to run integration tests — the scheduler race that previously
flaked `TestIntegrationLeaseTask_DependencyGating` is gone.

Set `POSTGRES_DB=vornik_test` to force the legacy shared DB, e.g. when
auditing a daemon-shared state. The Makefile `test-integration` target
still refuses to run when it detects an active daemon AND the configured
target is `vornik_test` — that combination is the one that flakes.

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `POSTGRES_HOST` | PostgreSQL hostname | `localhost` |
| `POSTGRES_PORT` | PostgreSQL port | `5432` |
| `POSTGRES_DB` | Database name | `vornik_test` |
| `POSTGRES_USER` | Database user | `vornik` |
| `POSTGRES_PASSWORD` | Database password | `vornik` |

### Running Tests

```bash
# Run all integration tests
make test-integration

# Or directly with go test
go test -tags=integration ./test/integration/...

# Run with custom PostgreSQL settings
POSTGRES_HOST=mydb POSTGRES_PASSWORD=secret make test-integration
```

### Using Docker/Podman for Test Database

```bash
# Start a test PostgreSQL instance.
# pgvector/pgvector:pg16 is upstream postgres:16 + pgvector extension
# pre-installed. Memory-subsystem tests require pgvector; a plain
# postgres image will fail the tests that exercise embedding search.
podman run --name vornik-test-postgres \
  -e POSTGRES_USER=vornik \
  -e POSTGRES_PASSWORD=vornik \
  -e POSTGRES_DB=vornik_test \
  -p 5432:5432 \
  -d docker.io/pgvector/pgvector:pg16

# Run tests
make test-integration

# Stop and remove the test database
podman stop vornik-test-postgres
podman rm vornik-test-postgres
```

### Skipping Integration Tests

Integration tests are automatically skipped when running with `-short`:

```bash
go test -short ./...
```

## Clustering test coverage (Slices A–C)

The clustering surface is tested in **two layers**:

**1. Runnable safety/fault layer** — `internal/service/cluster_*_test.go`. Plain unit
tests (NO build tag, NO database) that drive the real `ClusterHeartbeatSubsystem`
and `ClusterNodePrunerSubsystem` against in-memory repos with fault knobs, controlling
staleness via written `last_seen` (no sleeps). They run in the normal suite
(`go test ./internal/service/ -run 'TestInvariant|TestFault'`) and are the authoritative
proof of the reliability invariants:
- a node heartbeating within the grace window is **never** pruned (50-cycle interleave);
- a node holding an **active** lease is **never** reaped, no matter how stale its heartbeat;
- the pruner **bails** (deletes nothing) when it can't read the lease table (no reaping on a partial view);
- only the **leader** prunes; an expired lease does **not** protect;
- a transient DB error on a heartbeat, a leader handover, and concurrent/split-brain prunes
  **never** lose a live node.

The `pruneVictims` selection logic itself is mutation-tested (breaking the lease guard or
the partial-view bail makes the corresponding invariant test fail).

**2. Real-postgres integration layer** — `clustering_test.go` (`//go:build integration`).
Exercises the real `postgres.ClusterNodeRepository` + leader-lock repo + real
`leaderelection.Elector` handover + the heartbeat→prune lifecycle + the lease join against
a live database. Run it against a **throwaway** DB only:

```bash
TEST_DATABASE_URL=postgres://…/vornik_integration_test \
  go test -tags integration ./test/integration/ -run TestClusteringIntegration
```

Never point `TEST_DATABASE_URL` at the live `vornik_test`. The tests skip cleanly when it's unset.