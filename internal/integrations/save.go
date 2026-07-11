package integrations

// This file implements the Guided Integrations Hub's write path (design
// §5.4, task 5.2): re-probe -> field-split -> place secrets -> transactional
// config patch (with rollback) -> hot-reload -> return the fresh
// ProbeResult. No UI, HTTP, or metrics wiring lives here (5.3/5.4) — Save
// is a plain Go function 5.3's handlers call.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/projectdoctor"
)

// ErrForbidden is returned when the caller lacks scope to save this
// integration (design §6): a non-admin attempting a daemon-scope kind, or a
// project-scoped user targeting a project outside their scope.
var ErrForbidden = errors.New("integrations: caller not authorized for this save")

// projectSecretsFile mirrors projectdoctor.envSecretsFile (unexported in
// that package) — every project-scope secret lands in one file per project
// config directory, alongside the daemon's own secrets (design §5.4 step 3;
// see internal/service/container_http.go's onboardingSecretsDir, which
// points both the daemon's and the project doctor's secrets at the same
// "<configDir>/secrets" directory).
const projectSecretsFile = "project-secrets.env"

// Caller is the authenticated actor attempting a save, decoupled from any
// HTTP/session type so this package stays free of UI wiring (design: 5.2 is
// UI-free; 5.3 builds a Caller from the session/role it already resolves).
type Caller struct {
	// IsAdmin grants every scope (daemon and every project).
	IsAdmin bool
	// ScopedProjectIDs are the project IDs a non-admin caller may save
	// project-scope integrations for.
	ScopedProjectIDs []string
}

// Authorized reports whether c may save an integration in scope for
// projectID (design §6: daemon-scope kinds are admin-only; project-scope
// kinds require scope on the target project).
func (c Caller) Authorized(scope Scope, projectID string) bool {
	if c.IsAdmin {
		return true
	}
	if scope == ScopeDaemon {
		return false
	}
	for _, id := range c.ScopedProjectIDs {
		if id == projectID {
			return true
		}
	}
	return false
}

// ReloadStatusChecker mirrors the capability *config.ConfigReloader exposes
// (internal/ui/config_reload.go's reloadStatusReader) and
// GET /api/v1/config/reload-status (internal/api/config_handlers.go)
// surfaces over HTTP — Save consults it at the Go level (in-process),
// rather than making a self-HTTP-call, to decide whether a reload actually
// landed before declaring the save successful.
type ReloadStatusChecker interface {
	Status() config.ReloadStatus
}

// SaveDeps bundles the injectable seams Save needs. Production wiring
// (5.3) supplies the real ConfigDir + featuredoctor.FileConfigWriter (via
// NewWriter, or nil for the default) + a *config.ConfigReloader adapter;
// tests supply temp dirs and fakes.
type SaveDeps struct {
	// ConfigDir is the daemon's config directory (holds config.yaml,
	// projects/, secrets/).
	ConfigDir string
	// Reloader triggers the hot reload (design §5.4 step 5). Nil skips
	// the reload step entirely (tests that don't care about it).
	Reloader featuredoctor.Reloader
	// ReloadStatus, when non-nil, is polled after Reloader.Reload
	// succeeds until it reports neither Blocked nor HasErrors, or
	// ReloadDeadline elapses (whichever first) — design §5.4 step 5's
	// "poll the existing reload-status path". Nil skips polling: Reload
	// returning nil is treated as success.
	ReloadStatus ReloadStatusChecker
	// ReloadDeadline bounds the poll above. 0 => defaultReloadDeadline.
	ReloadDeadline time.Duration
	// NewWriter constructs the ConfigWriter for a resolved path. Nil =>
	// &featuredoctor.FileConfigWriter{Path: path} (production default).
	// Tests inject a fake to force Write/Validate failures without a
	// real filesystem.
	NewWriter func(path string) featuredoctor.ConfigWriter
}

// newWriter resolves the ConfigWriter for path. scope picks the DEFAULT
// implementation when d.NewWriter is nil: daemon scope gets the plain
// featuredoctor.FileConfigWriter (Validate() checks the daemon config.Config
// schema); project scope gets ProjectConfigWriter, whose Validate() checks
// the project schema instead (task 5.2b, 5.2 review finding 3 —
// FileConfigWriter.Validate() always validated the daemon schema
// regardless of which file it was pointed at, which is wrong for
// projects/<id>.yaml). d.NewWriter, when set, overrides unconditionally
// (test fakes don't need scope-awareness).
func (d SaveDeps) newWriter(path string, scope Scope) featuredoctor.ConfigWriter {
	if d.NewWriter != nil {
		return d.NewWriter(path)
	}
	if scope == ScopeProject {
		return &ProjectConfigWriter{FileConfigWriter: featuredoctor.FileConfigWriter{Path: path}}
	}
	return &featuredoctor.FileConfigWriter{Path: path}
}

func (d SaveDeps) secretsDir() string {
	return d.ConfigDir + "/secrets"
}

const defaultReloadDeadline = 3 * time.Second
const reloadPollInterval = 20 * time.Millisecond

// SaveResult is Save's outcome: the fresh ProbeResult plus whether the
// config/secrets/reload actually committed. Saved is false whenever Probe
// itself failed (a legitimate refusal, not an error — design §5.4 step 1)
// and whenever a later step returned a non-nil error.
type SaveResult struct {
	Probe ProbeResult
	Saved bool
}

// SaveError wraps a save-path failure with which step failed and whether
// rollback (Restore) succeeded, mirroring writeChatConfig's configPatchError
// shape (internal/api/setup_handlers.go) and the design's failure-mode
// table (§7): a secret already placed before a later step fails is never
// rolled back (it is idempotent and inert once the config no longer
// references it — a clean re-save overwrites it), so every message here is
// silent about secrets on purpose; the caller already knows whether a
// secret placement preceded this failure from where Save returned.
type SaveError struct {
	Step     string
	Cause    error
	Restored bool
}

func (e *SaveError) Error() string {
	if e.Restored {
		return fmt.Sprintf("integrations: save failed at %s (config restored from backup): %v", e.Step, e.Cause)
	}
	return fmt.Sprintf("integrations: save failed at %s (RESTORE ALSO FAILED — config may be inconsistent): %v", e.Step, e.Cause)
}

func (e *SaveError) Unwrap() error { return e.Cause }

// fieldPatch is one field's resolved (key, on-disk value) pair after
// field-split (design §5.4 step 2).
type fieldPatch struct {
	key string
	// val is the on-disk value for this field: a string for the common
	// case, or a []string for a CredentialField.List field (e.g.
	// github_app's repo_allowlist) — config.SetYAMLKey/YAMLListField
	// already accept `any` for exactly this reason (task 5.2b).
	val any
}

// Save implements the design's write path (§5.4) end to end. kind and
// target must describe the same integration (target.Scope == kind.Scope);
// callers normally obtain target via SaveTargetForKind(kind.ID).
func Save(ctx context.Context, kind IntegrationKind, target SaveTarget, cand CandidateConfig, caller Caller, deps SaveDeps) (SaveResult, error) {
	if target.Scope != kind.Scope {
		return SaveResult{}, fmt.Errorf("integrations: save target scope %q does not match kind %q scope %q", target.Scope, kind.ID, kind.Scope)
	}
	// Step 0 (design §6): scope enforcement gates everything, including
	// the re-probe — a caller out of scope shouldn't be able to trigger
	// any part of this path.
	if !caller.Authorized(kind.Scope, cand.ProjectID) {
		return SaveResult{}, fmt.Errorf("%w: kind %q scope %q project %q", ErrForbidden, kind.ID, kind.Scope, cand.ProjectID)
	}
	// Security-audit finding F-1 (path-confinement, task 5.2b): reject a
	// malformed project ID before any further work (probing, secret
	// placement, path construction) — every downstream project-scope path
	// this package builds is keyed on cand.ProjectID.
	if kind.Scope == ScopeProject {
		if err := validateProjectIDForPath(cand.ProjectID); err != nil {
			return SaveResult{}, fmt.Errorf("integrations: %w", err)
		}
	}

	// Step 1: re-probe. A failing probe is a hard stop, not an error —
	// the caller gets the failing ProbeResult back to render (§5.4 step 1).
	if kind.Prober == nil {
		return SaveResult{}, fmt.Errorf("integrations: kind %q has no Prober", kind.ID)
	}
	probe := kind.Prober.Probe(ctx, cand)
	if probe.Outcome != OutcomeOK {
		return SaveResult{Probe: probe, Saved: false}, nil
	}

	// Step 2 (field-split) + Step 3 (place secrets first).
	patches, placedSecretFiles, err := splitAndPlaceFields(kind, target, cand, deps)
	if err != nil {
		removeSecretFiles(placedSecretFiles)
		return SaveResult{Probe: probe}, err
	}

	// Step 4: patch config transactionally.
	configPath, err := target.ConfigFile(deps.ConfigDir, cand.ProjectID)
	if err != nil {
		removeSecretFiles(placedSecretFiles)
		return SaveResult{Probe: probe}, fmt.Errorf("integrations: resolve config path: %w", err)
	}
	writer := deps.newWriter(configPath, target.Scope)

	backup, err := writer.Backup()
	if err != nil {
		removeSecretFiles(placedSecretFiles)
		return SaveResult{Probe: probe}, &SaveError{Step: "backup", Cause: err}
	}
	restore := func(step string, cause error) (SaveResult, error) {
		restored := writer.Restore(backup) == nil
		// review-20260709-9160 finding 2: a file-secret (e.g. the github_app
		// PEM) placed by splitAndPlaceFields above must not survive a later
		// config-write failure as an orphan — unlike an env-line secret
		// (left in place; see SaveError's doc), a dedicated per-save file is
		// cleanly removable, so it gets an actual rollback here.
		removeSecretFiles(placedSecretFiles)
		return SaveResult{Probe: probe}, &SaveError{Step: step, Cause: cause, Restored: restored}
	}

	content, err := writer.Read()
	if err != nil {
		return restore("read", err)
	}

	content, err = applyPatches(content, target, patches)
	if err != nil {
		return restore("patch", err)
	}

	if err := writer.Write(content); err != nil {
		return restore("write", err)
	}
	if err := writer.Validate(); err != nil {
		return restore("validate", err)
	}

	// Step 5: hot-reload + poll.
	if deps.Reloader != nil {
		if err := deps.Reloader.Reload(ctx); err != nil {
			return restore("reload", err)
		}
		if deps.ReloadStatus != nil {
			deadline := deps.ReloadDeadline
			if deadline <= 0 {
				deadline = defaultReloadDeadline
			}
			if err := pollReloadStatus(ctx, deps.ReloadStatus, deadline); err != nil {
				return restore("reload", err)
			}
		}
	}

	// Step 6: the ProbeResult from step 1 already reflects this exact
	// candidate succeeding — re-probing again here would be a redundant
	// provider round-trip for identical values (and works against
	// MinProbeInterval-style provider lockout protection, design §5.1).
	return SaveResult{Probe: probe, Saved: true}, nil
}

// splitAndPlaceFields performs design §5.4 steps 2-3: for each of kind's
// Fields, resolve the on-disk value (a Secret field's placeholder/env-name
// after placing its literal in the secret store; a non-secret field's
// value verbatim), asserting the secret-literal boundary (§5.4, §7, §8) at
// each step. Returns as soon as any field fails — a partially-placed
// env-line secret from an earlier field in this same call is left in place
// (idempotent env-file write; see SaveError's doc), but every SecretFile
// path placed so far (this call's, and returned even on error) is handed
// back to the caller so Save can roll those specific files back on any
// later failure (review-20260709-9160 finding 2) — an env-line write and a
// dedicated secret file have different rollback stories, so only the
// latter is tracked here.
func splitAndPlaceFields(kind IntegrationKind, target SaveTarget, cand CandidateConfig, deps SaveDeps) ([]fieldPatch, []string, error) {
	// declaredEnvNames whitelists hasSecretLiteral's non-pattern exemption
	// (review-20260709-cc3e finding 2): only a value that exactly equals
	// one of THIS kind's own declared Secret-field EnvNames is a
	// legitimately-expected env-var-name config value (the project-scope
	// "_env" convention — see hasSecretLiteral's doc); everything else
	// shaped like an env-var name is just an unlucky-looking string and
	// must still be checked as a possible secret literal. For a
	// project-scope kind the declared name must be the ACTUAL (per-project
	// suffixed, task 5.2b) env name, since that's what ends up as the
	// config value — not the catalog's bare per-kind template.
	declaredEnvNames := make([]string, 0, len(kind.Fields))
	for _, ff := range kind.Fields {
		if strings.TrimSpace(ff.EnvName) == "" {
			continue
		}
		name := ff.EnvName
		if kind.Scope == ScopeProject {
			name = projectScopedEnvName(ff.EnvName, cand.ProjectID)
		}
		declaredEnvNames = append(declaredEnvNames, name)
	}

	patches := make([]fieldPatch, 0, len(kind.Fields))
	var placedSecretFiles []string
	for _, f := range kind.Fields {
		raw := cand.Values[f.Key]
		var (
			val any
			err error
		)
		switch {
		case !f.Secret:
			val, err = resolveNonSecretField(kind, f, raw, declaredEnvNames)
		case f.SecretFile:
			val, err = resolveSecretFileField(kind, target, deps, cand, f, raw, declaredEnvNames)
			if err == nil {
				placedSecretFiles = append(placedSecretFiles, val.(string))
			}
		default:
			val, err = resolveEnvSecretField(kind, target, deps, cand, f, raw, declaredEnvNames)
		}
		if err != nil {
			return nil, placedSecretFiles, err
		}
		patches = append(patches, fieldPatch{key: f.Key, val: val})
	}
	return patches, placedSecretFiles, nil
}

// resolveNonSecretField handles one non-Secret CredentialField: the
// hasSecretLiteral boundary assertion (§5.4, §8), then the List/Int/plain
// re-typing (task 5.2b's convertFieldValue).
func resolveNonSecretField(kind IntegrationKind, f CredentialField, raw string, declaredEnvNames []string) (any, error) {
	// Boundary assertion: a field the catalog marked non-secret must never
	// actually hold a bare secret literal — that would be a field-split
	// bug, and it must abort loudly before anything is written, not land
	// on disk.
	if hasSecretLiteral(raw, declaredEnvNames) {
		return nil, fmt.Errorf("integrations: field %q of kind %q is not marked Secret but its value looks like a secret literal — refusing to write (field-split bug)", f.Key, kind.ID)
	}
	val, err := convertFieldValue(f, raw)
	if err != nil {
		return nil, fmt.Errorf("integrations: field %q of kind %q: %w", f.Key, kind.ID, err)
	}
	return val, nil
}

// resolveSecretFileField handles one Secret+SecretFile CredentialField
// (task 5.2b's file-secret placement mode, e.g. github_app's private key):
// resolve the target path, write the literal there, and return the path as
// the field's on-disk config value.
func resolveSecretFileField(kind IntegrationKind, target SaveTarget, deps SaveDeps, cand CandidateConfig, f CredentialField, raw string, declaredEnvNames []string) (any, error) {
	pathFn, ok := target.SecretFilePaths[f.Key]
	if !ok {
		return nil, fmt.Errorf("integrations: field %q of kind %q is SecretFile but its SaveTarget has no SecretFilePaths entry", f.Key, kind.ID)
	}
	path, err := pathFn(deps.secretsDir(), cand.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("integrations: resolve secret file path for %q: %w", f.Key, err)
	}
	if err := placeSecretFile(path, raw); err != nil {
		return nil, fmt.Errorf("integrations: place secret file for %q: %w", f.Key, err)
	}
	if hasSecretLiteral(path, declaredEnvNames) {
		// Defense in depth, mirroring resolveEnvSecretField's check: a
		// broken SecretFilePaths implementation must never silently write
		// the raw secret into the config as a "path" either.
		return nil, fmt.Errorf("integrations: field %q of kind %q: computed secret file path still looks like a secret literal (bug in SecretFilePaths)", f.Key, kind.ID)
	}
	return path, nil
}

// resolveEnvSecretField handles one Secret (non-SecretFile) CredentialField
// — the "secret-as-env" placement mode: place the literal in the scope's
// secret store under a (project-scoped, for project scope) env name, and
// return target.SecretValue's formatted config value.
func resolveEnvSecretField(kind IntegrationKind, target SaveTarget, deps SaveDeps, cand CandidateConfig, f CredentialField, raw string, declaredEnvNames []string) (any, error) {
	if strings.TrimSpace(f.EnvName) == "" {
		return nil, fmt.Errorf("integrations: field %q of kind %q is Secret but has no EnvName", f.Key, kind.ID)
	}
	if target.SecretValue == nil {
		return nil, fmt.Errorf("integrations: kind %q has a Secret field %q but its SaveTarget has no SecretValue", kind.ID, f.Key)
	}
	envName := f.EnvName
	if kind.Scope == ScopeProject {
		envName = projectScopedEnvName(f.EnvName, cand.ProjectID)
	}
	if err := placeSecret(kind.Scope, cand.ProjectID, envName, raw, kind.ID, deps); err != nil {
		return nil, fmt.Errorf("integrations: place secret %q: %w", envName, err)
	}
	val := target.SecretValue(envName)
	if hasSecretLiteral(val, declaredEnvNames) {
		// Defense in depth: SecretValue is production code, not user
		// input, but a broken SecretValue implementation (e.g. returning
		// the raw value instead of a placeholder) must never slip past
		// this guard either.
		return nil, fmt.Errorf("integrations: field %q of kind %q: computed config value still looks like a secret literal (bug in SecretValue)", f.Key, kind.ID)
	}
	return val, nil
}

// projectScopedEnvName derives a per-project-unique secret env-var name for
// a project-scope Secret field: base (the catalog's static EnvName
// template, e.g. "EMAIL_IMAP_PASSWORD") + "_" + an env-var-safe suffix
// derived from projectID. Needed because every project-scope secret lands
// in the SAME shared project-secrets.env file (projectSecretsFile's doc) —
// without a per-project suffix, two projects both configuring (say) email
// through the hub would silently overwrite each other's password under the
// same bare env-var name (task 5.2b).
//
// This intentionally does NOT share a "validate + build slug" helper with
// githubAppPrivateKeyPath (companion review review-20260709-9160 considered
// this): every caller of projectScopedEnvName is reached only after Save's
// own top-of-function validateProjectIDForPath gate for ScopeProject (see
// Save's "Security-audit finding F-1" step), so re-validating here would be
// redundant, and the two callers need differently-shaped output anyway —
// githubAppPrivateKeyPath needs a path-safe slug (the original projectID,
// restricted to [A-Za-z0-9_-]), while this needs an env-var-safe fragment
// (upper-cased, non-[A-Z0-9] runs collapsed to "_" by sanitizeEnvSuffix,
// which would itself mangle a dash into "_" and so isn't path-safe). A
// shared helper would have to paper over that difference rather than
// remove real duplication.
func projectScopedEnvName(base, projectID string) string {
	return base + "_" + sanitizeEnvSuffix(projectID)
}

// sanitizeEnvSuffix upper-cases s and replaces every character outside
// [A-Z0-9] with "_", producing a safe POSIX env-var-name fragment from an
// arbitrary project ID.
func sanitizeEnvSuffix(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// convertFieldValue re-types a non-secret CredentialField's raw candidate
// string into the Go value config.SetYAMLKey must write (task 5.2b): List
// splits a comma-separated string into []string (a real []string-typed
// config field, e.g. github_app.repo_allowlist); Int parses to a Go int (a
// real int/int64-typed config field, e.g. app_id) — writing a numeric field
// as a YAML string scalar would fail to unmarshal back into its int-typed
// struct field at Validate() time. Anything else passes through as a plain
// string (the pre-5.2b behaviour for every existing field).
func convertFieldValue(f CredentialField, raw string) (any, error) {
	switch {
	case f.List:
		return splitCommaList(raw), nil
	case f.Int:
		s := strings.TrimSpace(raw)
		if s == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("field %q must be a whole number, got %q", f.Key, raw)
		}
		return n, nil
	default:
		return raw, nil
	}
}

// splitCommaList splits raw on commas, trims whitespace from each entry,
// and drops empty entries.
func splitCommaList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// placeSecret writes a Secret field's literal value into the scope-
// appropriate secret store (design §5.4 step 3): daemon scope uses
// onboarding.WriteEnvSecret into "<kindID>.env" (one file per integration,
// mirroring the existing chat.env / per-feature convention); project scope
// uses projectdoctor.EnvSecrets.Set, which also os.Setenv's the value so
// the ${ENV}/${envName}-referencing config resolves live without a daemon
// restart.
func placeSecret(scope Scope, projectID, envName, value, kindID string, deps SaveDeps) error {
	if scope == ScopeDaemon {
		_, err := onboarding.WriteEnvSecret(deps.secretsDir(), kindID+".env", envName, value)
		return err
	}
	_ = projectID // project scope shares one secrets dir with the daemon (see projectSecretsFile doc); the project id doesn't select a directory.
	return projectdoctor.NewEnvSecrets(deps.secretsDir()).Set(envName, value)
}

// placeSecretFile writes a Secret+SecretFile field's literal value (e.g. a
// pasted PEM) to a 0600 file at path, creating the parent directory if
// needed. This is the "secret-as-file" placement mode (task 5.2b) —
// distinct from placeSecret's "secret-as-env" mode — for config fields
// that are themselves a filesystem path (e.g. github_app.private_key_path)
// rather than an env-var-name/${ENV}-expandable string. Like placeSecret, a
// re-save overwrites the file in place (idempotent).
func placeSecretFile(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create secret file directory: %w", err)
	}
	return os.WriteFile(path, []byte(value), 0o600)
}

// removeSecretFiles best-effort deletes every path in paths — Save's
// rollback for file-secret placements (review-20260709-9160 finding 2) when
// a step after splitAndPlaceFields fails. Removal errors are swallowed on
// purpose: a cleanup failure must never shadow the real save error the
// caller is about to see, and a leftover file here is still inert (nothing
// in the just-failed/just-restored config references it) — the next
// successful save for this project's file-secret field overwrites it at the
// same deterministic path anyway (see placeSecretFile's doc), same as an
// orphaned env-line secret today.
func removeSecretFiles(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// applyPatches writes the field-split patches into content as N SetYAMLKey
// scalar patches (design §5.4 step 4). Every remaining kind is scalar-keyed;
// the one list-shaped target (MCP's mcp.servers upsert) left with the MCP
// kind's 2026-07-10 removal — see catalog.go's Registry doc.
func applyPatches(content []byte, target SaveTarget, patches []fieldPatch) ([]byte, error) {
	var err error
	for _, p := range patches {
		dotted, ok := target.ScalarKeys[p.key]
		if !ok {
			return nil, fmt.Errorf("integrations: field %q has no ScalarKeys entry in this SaveTarget", p.key)
		}
		content, _, err = config.SetYAMLKey(content, dotted, p.val)
		if err != nil {
			return nil, fmt.Errorf("set key %q: %w", dotted, err)
		}
	}
	return content, nil
}

// pollReloadStatus polls checker until it reports neither Blocked nor
// HasErrors, or deadline elapses.
func pollReloadStatus(ctx context.Context, checker ReloadStatusChecker, deadline time.Duration) error {
	deadlineAt := time.Now().Add(deadline)
	for {
		st := checker.Status()
		if !st.Blocked && !st.HasErrors {
			return nil
		}
		if time.Now().After(deadlineAt) {
			if st.Blocked {
				return fmt.Errorf("reload blocked: %s", st.BlockedReason)
			}
			return fmt.Errorf("reload reported errors: %v", st.Errors)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reloadPollInterval):
		}
	}
}

// hasSecretLiteral is the write-boundary guard (design §5.4, §6, §8):
// a cheap heuristic for "this value looks like a bare secret rather than an
// ${ENV} placeholder or a legitimately-expected env-var-name config value".
// Deliberately replicated from internal/ui/admin_control_plane_mcp.go's
// (*Server).hasSecretLiteral rather than imported: that method lives on the
// UI Server type and this package stays UI-free (5.1's package doc); the
// two copies are small and conceptually identical, so keeping this one here
// avoids either an import-direction violation (integrations importing ui)
// or exporting a free function out of ui for a single caller.
//
// allowedEnvNames whitelists the second exemption: the project-scope "_env"
// convention (ProjectGitHubApp.WebhookSecretEnv, ProjectSlack.SigningSecretEnv,
// ProjectEmail.IMAPPasswordEnv, ...; see save_targets.go's doc) writes the
// bare env-var NAME as the config value, not a "${NAME}" placeholder — but
// that's only legitimate when it's THIS kind's own declared EnvName
// (splitAndPlaceFields builds allowedEnvNames from kind.Fields). Exempting
// by shape alone (review-20260709-cc3e finding 2) let an actual
// env-shaped-looking secret literal (e.g. "GITHUB_TOKEN_ABC123DEF456") slip
// past the guard; an exact-match whitelist against the kind's own declared
// names closes that hole while still allowing the legitimate convention.
func hasSecretLiteral(v string, allowedEnvNames []string) bool {
	if v == "" || strings.Contains(v, "${") {
		return false
	}
	for _, name := range allowedEnvNames {
		if v == name {
			return false
		}
	}
	return len(v) >= 24 && !strings.ContainsAny(v, " /:")
}
