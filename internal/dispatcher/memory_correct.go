package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/untrusted"
)

// MemoryCorrector is the narrow surface the dispatcher's
// memory_correct tool needs. Decoupled from memory.Corrector so
// tests can supply a stub and the dispatcher doesn't take a
// hard build-time dep on the full memory subsystem.
type MemoryCorrector interface {
	RefuteByClaim(ctx context.Context, projectID, wrongClaim string, maxRefutes int) ([]memory.RefutedChunk, error)
	// InsertCorrection accepts repoScope (added 2026-05-29):
	// empty preserves the legacy NULL-scoped behaviour. The
	// dispatcher tool doesn't know its caller's repo scope today,
	// so it passes empty; future per-request context plumbing
	// can supply the active scope.
	InsertCorrection(ctx context.Context, projectID, content, repoScope string) (string, error)
}

// memoryCorrectName is the LLM-visible tool name.
const memoryCorrectName = "memory_correct"

// memoryCorrectDescriptor is the chat.Tool definition the
// dispatcher registers in DispatcherTools(). The description is
// the LLM's only signal for when to fire — it must be
// unambiguous about the trigger condition ("user just told you
// a stored fact is wrong") so the model doesn't fire it on
// every disagreement.
func memoryCorrectDescriptor() chat.Tool {
	return chat.Tool{
		Type: "function",
		Function: chat.ToolFunction{
			Name: memoryCorrectName,
			Description: "Refute a wrong fact in project memory and store the correction. " +
				"Call this WHEN: the user has just told you a stored fact is wrong AND given the correct version. " +
				"DO NOT call this for: opinions, preferences, or general feedback — only when a verifiable factual claim " +
				"in memory needs to be replaced. The tool runs hybrid search for the wrong claim, marks the top matches " +
				"as refuted (the retrieval layer auto-excludes them from subsequent memory_search calls), and stores " +
				"the correction as a verified high-confidence chunk so future tasks pick up the corrected fact. " +
				"Returns the count of refuted chunks plus the new correction chunk's ID.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"wrong_claim":{"type":"string","description":"The factual claim that is wrong, phrased the way it currently appears in memory (e.g. 'Janka was born in 1985')."},
					"correction":{"type":"string","description":"The correct fact, phrased as it should appear going forward (e.g. 'Janka was born in 1990'). One to three sentences; include context that distinguishes this claim from similar ones."},
					"project_id":{"type":"string","description":"Project ID (uses active project if omitted)."},
					"max_refutes":{"type":"integer","description":"Cap on chunks to refute in one call. Default 3, max 20. Use 1 when correcting a specific stored claim; raise when the wrong fact is repeated across many sources."}
				},
				"required":["wrong_claim","correction"]
			}`),
		},
	}
}

// memoryCorrect is the handler invoked when the LLM calls
// memory_correct. Three side effects (in order):
//
//  1. Hybrid-search wrong_claim and flip the top-N matches to
//     validation_status='refuted'. Retrieval already filters
//     refuted rows out, so the next memory_search call won't
//     return them.
//  2. Insert a new chunk with the correction at content_class=
//     'decision', validation_status='verified',
//     producer_role='operator_correction', confidence=0.95. The
//     embed worker picks it up on its next tick; FTS search
//     sees it immediately.
//  3. Return a brief summary to the LLM listing what was
//     refuted + the new chunk ID so the model can include
//     "I've corrected memory" in its reply.
//
// Re-validation task enqueue is a follow-up improvement — left
// out of this slice to keep the surface focused on the
// in-the-moment correction.
func (te *ToolExecutor) memoryCorrect(ctx context.Context, argsJSON, activeProject string, allowedProjects []string) ToolResult {
	var args struct {
		WrongClaim string `json:"wrong_claim"`
		Correction string `json:"correction"`
		ProjectID  string `json:"project_id"`
		MaxRefutes int    `json:"max_refutes"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	args.WrongClaim = strings.TrimSpace(args.WrongClaim)
	args.Correction = strings.TrimSpace(args.Correction)
	if args.WrongClaim == "" {
		return ToolResult{Content: "wrong_claim is required."}
	}
	if args.Correction == "" {
		return ToolResult{Content: "correction is required."}
	}
	if te.memoryCorrector == nil {
		return ToolResult{Content: "Memory correction is not enabled on this daemon (memory subsystem disabled?)."}
	}
	project, err := resolveProjectAllowed(args.ProjectID, activeProject, allowedProjects)
	if err != nil {
		return ToolResult{Content: err.Error()}
	}
	maxRefutes := args.MaxRefutes
	if maxRefutes <= 0 {
		maxRefutes = 3
	}

	refuted, err := te.memoryCorrector.RefuteByClaim(ctx, project, args.WrongClaim, maxRefutes)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Refute failed: %v", err)}
	}

	chunkID, insErr := te.memoryCorrector.InsertCorrection(ctx, project, args.Correction, "")
	if insErr != nil {
		// The correction insert failed but refutes may have
		// landed. Tell the LLM what state we're in so it can
		// surface it to the user — partial success is better
		// than silent half-application.
		return ToolResult{Content: fmt.Sprintf(
			"Refuted %d chunk(s) for %q, but the correction insert failed: %v. The wrong fact won't surface in future searches, but the new fact wasn't recorded — ask the user to repeat the correction in a moment.",
			len(refuted), args.WrongClaim, insErr,
		)}
	}

	var b strings.Builder
	if len(refuted) == 0 {
		fmt.Fprintf(&b, "No memory chunks matched the wrong claim %q in project %s — nothing refuted.\n", args.WrongClaim, project)
	} else {
		fmt.Fprintf(&b, "Refuted %d memory chunk(s) for %q in project %s:\n", len(refuted), args.WrongClaim, project)
		for i, r := range refuted {
			fmt.Fprintf(&b, "  [%d] %s  (score=%.2f, source=%s)\n", i+1, r.ID, r.Score, r.SourceName)
			if r.Preview != "" {
				b.WriteString("       ")
				b.WriteString(untrusted.WrapLabeled("refuted_preview", r.Preview))
				b.WriteString("\n")
			}
		}
	}
	fmt.Fprintf(&b, "\nStored correction as chunk %s (validation_status=verified, content_class=decision). Future memory_search calls will return this corrected fact.\n", chunkID)
	return ToolResult{Content: b.String(), Provenance: outputguard.ProvenanceFirstParty}
}
