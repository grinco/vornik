// Package persistence provides database abstractions and repository implementations
// for the vornik daemon.
//
// The current runtime database is PostgreSQL.
//
// Repositories:
//   - TaskRepository: Task queue operations
//   - ExecutionRepository: Execution state management
//   - ArtifactRepository: Artifact metadata storage
package persistence
