package hallucination

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// GroundingContext is what a detector treats as "ground truth"
// for a single Scan invocation. Built from the persistence
// layer (audit log + artifact catalogue + task DB) so the
// detector itself stays a pure function over text + this struct.
//
// Two construction paths:
//
//  1. Executor path — BuildForStep loads tool_audit_log + artifact
//     list filtered to one (execution_id, step_id) and returns a
//     fully-populated context. Synchronous, cheap, fires on every
//     step finalize.
//
//  2. Dispatcher path — the bot constructs a context from the
//     in-memory tool-call trace it just ran, plus a registry
//     snapshot for project/task ID checks. No DB hit needed for
//     the chat side because the call sites are short-lived.
type GroundingContext struct {
	// ToolCallNames lets URL/path detectors check whether ANY
	// tool was invoked at all — when the audit list is empty,
	// every claim is automatically suspect.
	ToolCallNames []string

	// ToolCallInputs is the raw `tool_input` JSON string for each
	// audited call (or the dispatcher's in-memory equivalent). The
	// hallucinated_tool_format rule scans these for XML wrappers
	// and tokenizer specials that should never appear inside a
	// well-formed tool argument blob — closes the gap where the
	// model emits `tool_name=run_shell` plus a `command` argument
	// containing `<arg_value>…</arg_value>` raw, which the runtime
	// dispatches as a real shell call but the operator can later
	// see was the model hallucinating its own tool-call XML.
	ToolCallInputs []string

	// FetchedURLs is every URL that appeared in the input or
	// output of a fetch-class tool (web_fetch / web_scrape /
	// http_get / mcp__*). Lower-cased for membership checks.
	// Detector emits "url claimed but not fetched" when the
	// model's prose mentions a URL not in this set.
	FetchedURLs map[string]struct{}

	// ToolOutputs is the concatenated tool_output text for the
	// step. Numeric / quote-style claims fall back to substring
	// search here when no more specific structure is available.
	ToolOutputs string

	// ArtifactNames is the names of artifacts the step (or
	// preceding steps in the same execution for the dispatcher
	// path) produced. A model that says "see scan-eu-2026.md"
	// without that artifact existing is hallucinating.
	ArtifactNames map[string]struct{}

	// KnownTaskIDs is the set of task IDs the model could
	// legitimately reference. Populated from
	// tasks.GetMostRecent(N) in the dispatcher path; empty in
	// the executor path (workers don't normally cite task IDs).
	KnownTaskIDs map[string]struct{}

	// KnownProjectIDs is the registry's project list — a chat
	// reply that switch_project'd to "saascotorial" when no
	// such project exists is hallucinating.
	KnownProjectIDs map[string]struct{}

	// KnownArtifactNames is the union across visible tasks for
	// the dispatcher path: any artifact name the model could
	// have legitimately seen via list_artifacts / read_artifact.
	// Distinct from ArtifactNames (which is the current step's
	// own producing set in the executor path).
	KnownArtifactNames map[string]struct{}
}

// AuditLister is the narrow subset of ToolAuditRepository the
// grounding builder needs. Defined as a local interface so unit
// tests can pass a slice-backed stub instead of a real DB.
type AuditLister interface {
	List(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error)
}

// ArtifactLister is the narrow subset of ArtifactRepository the
// builder needs.
type ArtifactLister interface {
	List(ctx context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error)
}

// BuildForStep assembles a GroundingContext for one
// (execution, step) pair by reading the audit log + artifact
// catalogue. Failures on either side return a partially-filled
// context plus the error — the caller is expected to log the
// error and proceed with whatever was loaded; running the
// detector on a partial context is strictly safer than skipping
// it (worst case it produces no signal, never a false positive
// from missing data, because absence-evidence is the only
// signal source and partial data biases toward Info/no-signal,
// not toward false High).
func BuildForStep(ctx context.Context, audits AuditLister, artifacts ArtifactLister, executionID, taskID string) (*GroundingContext, error) {
	gc := &GroundingContext{
		FetchedURLs:        map[string]struct{}{},
		ArtifactNames:      map[string]struct{}{},
		KnownTaskIDs:       map[string]struct{}{},
		KnownProjectIDs:    map[string]struct{}{},
		KnownArtifactNames: map[string]struct{}{},
	}

	if audits != nil && executionID != "" {
		execID := executionID
		entries, err := audits.List(ctx, persistence.ToolAuditFilter{
			ExecutionID: &execID,
			PageSize:    500,
		})
		if err != nil {
			return gc, err
		}
		var sb strings.Builder
		for _, e := range entries {
			if e == nil {
				continue
			}
			gc.ToolCallNames = append(gc.ToolCallNames, e.ToolName)
			if e.ToolInput != "" {
				gc.ToolCallInputs = append(gc.ToolCallInputs, e.ToolInput)
			}
			if e.ToolOutput != "" {
				sb.WriteString(e.ToolOutput)
				sb.WriteString("\n")
			}
			// Pull URLs out of input/output. URLs in input are
			// "claimed to fetch"; URLs in output are "actually
			// returned" — both ground a claim. Lower-case both
			// because URL hostname comparison is case-insensitive
			// and we don't want spurious mismatches on capital
			// letters.
			for _, u := range extractURLs(e.ToolInput) {
				gc.FetchedURLs[strings.ToLower(u)] = struct{}{}
			}
			for _, u := range extractURLs(e.ToolOutput) {
				gc.FetchedURLs[strings.ToLower(u)] = struct{}{}
			}
		}
		gc.ToolOutputs = sb.String()
	}

	if artifacts != nil && taskID != "" {
		tID := taskID
		arts, err := artifacts.List(ctx, persistence.ArtifactFilter{
			TaskID:   &tID,
			PageSize: 100,
		})
		if err != nil {
			return gc, err
		}
		for _, a := range arts {
			if a == nil {
				continue
			}
			gc.ArtifactNames[a.Name] = struct{}{}
		}
	}
	return gc, nil
}

// MergeKnownIDs lets the dispatcher path enrich a context with
// registry-derived IDs after the audit/artifact build. Splits
// from BuildForStep because registry access has a different
// shape (snapshot lookup, not a query) and forcing it through
// an interface in the persistence-side builder muddied things.
func (gc *GroundingContext) MergeKnownIDs(projectIDs []string, taskIDs []string, artifactNames []string) {
	if gc.KnownProjectIDs == nil {
		gc.KnownProjectIDs = map[string]struct{}{}
	}
	for _, p := range projectIDs {
		gc.KnownProjectIDs[p] = struct{}{}
	}
	if gc.KnownTaskIDs == nil {
		gc.KnownTaskIDs = map[string]struct{}{}
	}
	for _, t := range taskIDs {
		gc.KnownTaskIDs[t] = struct{}{}
	}
	if gc.KnownArtifactNames == nil {
		gc.KnownArtifactNames = map[string]struct{}{}
	}
	for _, n := range artifactNames {
		gc.KnownArtifactNames[n] = struct{}{}
	}
}
