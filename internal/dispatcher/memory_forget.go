package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/untrusted"
)

// memoryForgetName is the LLM-visible tool name.
const memoryForgetName = "memory_forget"

// memoryForgetDescriptor is the chat.Tool definition registered in
// DispatcherTools(). memory_forget is the DETERMINISTIC counterpart to
// memory_correct: it evicts exactly one chunk by the id the model read
// off a memory_search result, with no fuzzy search and so no risk of
// down-weighting the wrong chunk. The description steers the model to
// use the id from memory_search's "id=" field, and to reach for this
// tool (not memory_correct) when it already knows the precise chunk.
func memoryForgetDescriptor() chat.Tool {
	return chat.Tool{
		Type: "function",
		Function: chat.ToolFunction{
			Name: memoryForgetName,
			Description: "Deterministically evict ONE specific memory chunk by its id. " +
				"Call this WHEN: you (or the user) have identified a specific stored chunk that should no longer surface — " +
				"and you have its id from a prior memory_search result (each hit prints an 'id=<chunk id>' handle). " +
				"Unlike memory_correct (which fuzzy-searches a claim and may match the wrong chunk), memory_forget targets " +
				"exactly the id you pass and nothing else. It soft-evicts (marks the chunk refuted): it stops appearing in " +
				"future memory_search / recall, and the change is reversible — this is NOT a permanent/GDPR deletion. " +
				"Use memory_correct instead when you want to both refute a wrong fact AND store the corrected version. " +
				"Returns the evicted chunk's source + a preview, or a clear notice when the id matches no chunk in this project.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"chunk_id":{"type":"string","description":"The id of the chunk to evict, taken verbatim from an 'id=<chunk id>' field in a memory_search result (e.g. 'chunk_ab12cd34')."},
					"project_id":{"type":"string","description":"Project ID (uses active project if omitted)."}
				},
				"required":["chunk_id"]
			}`),
		},
	}
}

// memoryForget is the handler invoked when the LLM calls memory_forget.
// It resolves the calling project (session-scoped — a chunk id can only
// be forgotten within an allowed project), then soft-evicts the single
// chunk by id via the corrector's deterministic ForgetByID. The result
// is truthful: it reports exactly what was evicted, or that the id
// matched nothing in this project — it never implies an eviction that
// did not happen.
func (te *ToolExecutor) memoryForget(ctx context.Context, argsJSON, activeProject string, allowedProjects []string) ToolResult {
	var args struct {
		ChunkID   string `json:"chunk_id"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	args.ChunkID = strings.TrimSpace(args.ChunkID)
	if args.ChunkID == "" {
		return ToolResult{Content: "chunk_id is required."}
	}
	if te.memoryCorrector == nil {
		return ToolResult{Content: "Memory correction is not enabled on this daemon (memory subsystem disabled?)."}
	}
	project, err := resolveProjectAllowed(args.ProjectID, activeProject, allowedProjects)
	if err != nil {
		return ToolResult{Content: err.Error()}
	}

	evicted, err := te.memoryCorrector.ForgetByID(ctx, project, args.ChunkID)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Forget failed: %v", err)}
	}
	if evicted == nil {
		// The id resolved to no chunk in this project — say so plainly
		// rather than pretending something was evicted.
		return ToolResult{
			Content: fmt.Sprintf(
				"No chunk with id %q exists in project %s — nothing was evicted. (The id may be wrong, already evicted, or belong to another project.)",
				args.ChunkID, project),
			Provenance: outputguard.ProvenanceFirstParty,
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Evicted memory chunk %s (source=%s) from project %s — it will no longer surface in memory_search or recall. The soft-evict is reversible.\n",
		evicted.ID, evicted.SourceName, project)
	if evicted.Preview != "" {
		b.WriteString("  evicted content: ")
		b.WriteString(untrusted.WrapLabeled("evicted_preview", evicted.Preview))
		b.WriteString("\n")
	}
	return ToolResult{Content: b.String(), Provenance: outputguard.ProvenanceFirstParty}
}
