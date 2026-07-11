package projectwizard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"

	"vornik.io/vornik/internal/registry"
)

// removeAllFn is a test seam for the staging temp-dir cleanup call
// (os.RemoveAll in production) — overridden in tests to simulate a
// cleanup failure without relying on filesystem-permission tricks.
var removeAllFn = os.RemoveAll

// stagedValidationResult is the outcome of full-bundle staged
// validation (design §5.2 step 2).
type stagedValidationResult struct {
	OK     bool
	Errors []string
}

// liveEntityIDsFromConfigDir reads the live registry's project/
// swarm/workflow IDs directly off disk — used by the pre-stage
// collision check (bundle_shape.go's collisionCheckBundle). A blank
// configDir (no live tree wired, e.g. a unit test) returns the empty
// set rather than erroring.
func liveEntityIDsFromConfigDir(configDir string) (liveEntityIDs, error) {
	out := liveEntityIDs{Projects: map[string]bool{}, Swarms: map[string]bool{}, Workflows: map[string]bool{}}
	if configDir == "" {
		return out, nil
	}
	projects, err := registry.LoadProjects(configDir)
	if err != nil {
		return out, fmt.Errorf("load live projects: %w", err)
	}
	for id := range projects {
		out.Projects[id] = true
	}
	swarms, err := registry.LoadSwarms(configDir)
	if err != nil {
		return out, fmt.Errorf("load live swarms: %w", err)
	}
	for id := range swarms {
		out.Swarms[id] = true
	}
	workflows, err := registry.LoadWorkflows(configDir)
	if err != nil {
		return out, fmt.Errorf("load live workflows: %w", err)
	}
	for id := range workflows {
		out.Workflows[id] = true
	}
	return out, nil
}

// stageBundleForValidation renders `files` (a materialized bundle's
// project.yaml + swarm.md + workflow(s).md) into a throwaway temp
// directory, then loads a FRESH registry layered as
// [liveConfigDir, tempDir] (later wins — registry.go's LoadFromPaths)
// so cross-references to pre-existing swarms/workflows resolve
// exactly as they would for a hand-authored config change. This reuses
// the identical loaders/validators hand-authored config goes through
// (LoadWorkflows/LoadSwarms call Workflow.Validate/Swarm.Validate
// per-entity; LoadFromPaths' StripInvalidFromStaged enforces the
// cross-reference invariant set — swarmId/defaultWorkflowId
// resolution, step→role references, gate/output-schema compatibility,
// autonomy validation) — identical rules to hand-authored config, no
// bypass (design §5.2).
//
// The temp directory is always removed before returning. Nothing here
// ever touches the live config tree — no commit journal, no staging
// dir; that machinery is task 1.2. This function only ever validates.
func stageBundleForValidation(liveConfigDir string, files map[string]string) (stagedValidationResult, error) {
	tempDir, err := os.MkdirTemp("", "vornik-composer-stage-*")
	if err != nil {
		return stagedValidationResult{}, fmt.Errorf("create staging temp dir: %w", err)
	}
	defer func() {
		// Cleanup runs on every path (success, ValidationError, or the
		// fatal-error branch below) — but a failure here was
		// previously discarded outright, silently accumulating
		// abandoned staging directories with no operational signal.
		// Warn-log rather than ignore: this must never affect the
		// staged-validation OUTCOME already computed above, only be
		// observable after the fact.
		if rmErr := removeAllFn(tempDir); rmErr != nil {
			log.Warn().Err(rmErr).Str("temp_dir", tempDir).
				Msg("composer: failed to remove staged-validation temp dir")
		}
	}()

	for target, body := range files {
		full := filepath.Join(tempDir, target)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return stagedValidationResult{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			return stagedValidationResult{}, fmt.Errorf("write %s: %w", full, err)
		}
	}

	reg := registry.New()
	var paths []string
	if liveConfigDir != "" {
		paths = append(paths, liveConfigDir)
	}
	paths = append(paths, tempDir)

	loadErr := reg.LoadFromPaths(paths...)
	if loadErr == nil {
		return stagedValidationResult{OK: true}, nil
	}
	var verr *registry.ValidationError
	if errors.As(loadErr, &verr) {
		errs := make([]string, 0, len(verr.Errors))
		for _, e := range verr.Errors {
			errs = append(errs, e.Error())
		}
		return stagedValidationResult{OK: false, Errors: errs}, nil
	}
	// A per-entity Workflow.Validate/Swarm.Validate/Project.Validate
	// failure (unreachable entrypoint, invalid leadRole, …) is still a
	// validation failure from the caller's perspective, not an
	// infrastructure error — surface it the same way as the
	// stripped-project case above. These error types carry only a
	// config-file BASENAME + field + message (never a filesystem
	// path), so their own .Error() text is exactly as safe to hand to
	// the operator as the ValidationError branch — but the FULL
	// loadErr chain must not be used here: when >1 path is layered,
	// registry.LoadFromPaths wraps failures as `load layer %q: %w`,
	// which DOES embed the internal staging temp dir once
	// liveConfigDir is non-empty (the normal production case).
	if msg, ok := knownValidationErrorMessage(loadErr); ok {
		return stagedValidationResult{OK: false, Errors: []string{msg}}, nil
	}
	// Anything else here is a genuinely unexpected/fatal registry
	// error (I/O failure, a config path that isn't a directory, …)
	// whose wrapped text may embed the staging temp dir path or other
	// internal detail. Never surface that to the operator-facing
	// envelope (companion review finding 3) — log the raw error
	// server-side and hand the caller a generic, non-leaking message.
	log.Error().Err(loadErr).Msg("composer: staged bundle validation hit an unexpected registry error")
	return stagedValidationResult{OK: false, Errors: []string{"bundle validation failed"}}, nil
}

// knownValidationErrorMessage reports whether err's chain contains one
// of the registry package's structured per-entity validation error
// types (raised by Project/Swarm/Workflow.Validate, before the
// cross-reference pass registry.ValidationError covers), returning
// that error's own message when so. These types are safe to surface
// verbatim — see the comment at the call site.
func knownValidationErrorMessage(err error) (string, bool) {
	var swErr registry.SwarmValidationError
	if errors.As(err, &swErr) {
		return swErr.Error(), true
	}
	var wfErr registry.WorkflowValidationError
	if errors.As(err, &wfErr) {
		return wfErr.Error(), true
	}
	var pErr registry.ProjectValidationError
	if errors.As(err, &pErr) {
		return pErr.Error(), true
	}
	return "", false
}
