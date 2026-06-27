// Package registry provides in-memory registries for projects, swarms, and workflows.
//
// The registry is a read-only cache that loads configuration from YAML files
// and provides thread-safe access to the loaded definitions. It supports:
//
//   - Project definitions (projects/*.yaml)
//   - Swarm definitions (swarms/*.yaml)
//   - Workflow definitions (workflows/*.yaml)
//
// All configuration files are validated during load:
//   - Schema validation ensures required fields are present
//   - Cross-reference validation ensures referenced IDs exist
//   - Graph validation ensures workflows are valid and reachable
//
// The registry supports hot reload: calling Load() or Reload() will re-read
// all configuration files and validate them. On success, the in-memory cache
// is updated atomically. On failure, the existing configuration is preserved.
//
// Usage:
//
//	reg := registry.New()
//	if err := reg.Load("/path/to/configs"); err != nil {
//	    log.Fatalf("failed to load config: %v", err)
//	}
//
//	// Get a project with all its resolved dependencies
//	project, swarm, workflow, err := reg.ResolveProjectConfig("my-project")
//
//	// Or get individual components
//	project := reg.GetProject("my-project")
//	swarm := reg.GetSwarm(project.SwarmID)
package registry
