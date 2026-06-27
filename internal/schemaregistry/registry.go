// Package schemaregistry loads JSON-Schema documents from
// configs/schemas/*.json at daemon boot and exposes a compiled-
// schema lookup the executor's resolve hook uses to validate
// cross_project_call result envelopes (LLD §4.2 + §5.3).
//
// Design choice for v1: in-memory loader fed by the on-disk
// directory. Migration 52 created a schema_registry table for
// future DB-backed schema management; v1 leaves that table
// empty and reads from disk, which mirrors how project +
// swarm + workflow YAML are managed (configs on disk, daemon
// loads at boot + SIGHUP).
//
// Concurrency: Registry is goroutine-safe via an RWMutex. The
// load path takes the write lock briefly; lookups take the
// read lock so the validation hot path doesn't contend with
// reloads.
package schemaregistry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Registry holds compiled JSON-Schema documents keyed by their
// $id (or filename slug when $id is absent). The executor's
// resolve hook calls Validate(envelopeData, schemaID) against
// this registry on every CPC completion.
type Registry struct {
	mu      sync.RWMutex
	schemas map[string]*jsonschema.Schema
	// dir tracks the source directory so Reload() can scan the
	// same path that was loaded initially. Empty when the
	// registry was constructed without a load.
	dir string
}

// New returns an empty registry. Use Load to populate from disk,
// or pass directly to executor.WithSchemaRegistry for an
// always-empty registry (test paths, deployments without
// configs/schemas/).
func New() *Registry {
	return &Registry{schemas: map[string]*jsonschema.Schema{}}
}

// Load constructs a registry by scanning dir for *.json files,
// each treated as one schema definition. The schema's id
// defaults to the filename (without .json); a `$id` field in
// the body overrides. A non-existent dir is NOT an error —
// returns an empty registry so deployments without
// configs/schemas/ keep working (the resolve hook falls back
// to envelope-shape validation when no schema is registered
// for the call's expected_schema id).
//
// Returns a multi-error when individual files fail; the
// registry still contains every successfully-loaded schema.
func Load(dir string) (*Registry, error) {
	r := New()
	r.dir = dir
	if err := r.scan(dir); err != nil {
		return r, err
	}
	return r, nil
}

// scan reads every *.json under dir into the schema map.
// Internal helper called by both Load (initial load) and
// Reload (SIGHUP refresh).
func (r *Registry) scan(dir string) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("schemaregistry: read dir %q: %w", dir, err)
	}
	fresh := map[string]*jsonschema.Schema{}
	var loadErrs []string
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			loadErrs = append(loadErrs, fmt.Sprintf("%s: read: %v", ent.Name(), err))
			continue
		}
		id, compiled, err := compileSchema(ent.Name(), body)
		if err != nil {
			loadErrs = append(loadErrs, fmt.Sprintf("%s: %v", ent.Name(), err))
			continue
		}
		fresh[id] = compiled
	}
	r.mu.Lock()
	r.schemas = fresh
	r.mu.Unlock()
	if len(loadErrs) > 0 {
		return fmt.Errorf("schemaregistry: %d files failed to load: %s", len(loadErrs), strings.Join(loadErrs, "; "))
	}
	return nil
}

// Reload re-scans the directory the registry was originally
// loaded from. Idempotent — operators can call this from the
// config-reload path on SIGHUP without re-reading the dir
// path. Returns an error when the registry was constructed
// with New (no dir on file) so the caller can fall back to
// the operator's explicit reload path.
func (r *Registry) Reload() error {
	r.mu.RLock()
	dir := r.dir
	r.mu.RUnlock()
	if dir == "" {
		return fmt.Errorf("schemaregistry: reload called on a registry constructed without a directory")
	}
	return r.scan(dir)
}

// compileSchema reads the schema body, picks its id (either
// from the `$id` field in the body or the filename), and
// compiles via the jsonschema library. Returns the resolved
// id + the compiled schema or an error.
func compileSchema(filename string, body []byte) (string, *jsonschema.Schema, error) {
	// Determine the schema id. The on-disk filename without
	// the .json suffix is the default; an explicit `$id` field
	// in the body overrides so authors can declare canonical
	// names that survive a rename.
	id := strings.TrimSuffix(filename, ".json")
	var meta struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(body, &meta); err == nil && meta.ID != "" {
		id = meta.ID
	}

	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(id, bytes.NewReader(body)); err != nil {
		return "", nil, fmt.Errorf("add resource: %w", err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		return "", nil, fmt.Errorf("compile: %w", err)
	}
	return id, schema, nil
}

// Get returns the compiled schema registered under id, or nil
// when no schema is registered. Callers nil-check the result
// before validating — an absent schema falls through to the
// resolve hook's envelope-shape check.
func (r *Registry) Get(id string) *jsonschema.Schema {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.schemas[id]
}

// Validate is the high-level entry point the resolve hook
// calls. Returns:
//   - nil       — schema unknown (caller falls through to
//     envelope-shape check) OR validation passed
//   - non-nil   — schema registered AND validation failed
//
// The caller distinguishes "skipped" vs "passed" via the
// HasSchema helper if needed. Most callers don't care — a nil
// return from this method means "the envelope is acceptable
// either by schema validation or by the absence of a schema".
//
// Compiles validation against the parsed envelope data
// (post-json.Unmarshal). Callers pass the already-decoded
// map[string]any rather than raw bytes to avoid re-parsing.
func (r *Registry) Validate(id string, envelope any) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	schema, ok := r.schemas[id]
	r.mu.RUnlock()
	if !ok || schema == nil {
		return nil
	}
	return schema.Validate(envelope)
}

// HasSchema reports whether a schema is registered under id.
// Used by the resolve hook to decide between "fall through to
// envelope-shape check" (no schema) and "treat validation
// failure as rejection" (schema present + Validate returned
// error).
func (r *Registry) HasSchema(id string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.schemas[id]
	return ok
}

// Count returns the number of schemas currently registered.
// Used by the boot log + the admin /healthz tile so operators
// can confirm the loader picked up their configs/schemas/
// files.
func (r *Registry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.schemas)
}

// IDs returns the sorted list of registered schema ids. Useful
// for /ui/admin diagnostic surfaces. Returned slice is a copy
// so the caller can mutate without holding the lock.
func (r *Registry) IDs() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.schemas))
	for k := range r.schemas {
		out = append(out, k)
	}
	sort.Strings(out) // doc contract: callers (admin diagnostics) get stable, sorted output
	return out
}
