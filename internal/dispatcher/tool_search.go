// 2026.7.0 F12 — Tool deferred loading + tool_search.
//
// Most MCP catalogs grow past 20 tools the moment a tenant
// wires a single broker + scraper + gmail integration. Loading
// every tool descriptor into the LLM's tools array on every
// turn burns prompt tokens and dilutes the schema's signal
// (the model has to scan a longer menu to find anything).
//
// Pattern borrowed from Turnstone (docs/tools.md "MCP Deferred
// Loading"): hide MCP tools by default when total >
// threshold. Surface a built-in `tool_search` so the model can
// discover what's available. Matches expand into the
// per-session visible set and remain visible for subsequent
// turns of the same session.
//
// Implementation: ranking is a cheap lexical score (term
// overlap + descriptor-text contains) — full BM25 is overkill
// for a 100-tool catalog. The expanded-set lives on the Agent
// keyed by ChatID; tests can pass a zero ChatID for stateless
// rigs.

package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/outputguard"
)

// DefaultDeferredToolThreshold is the total-tool count above
// which MCP tools get hidden by default. Built-in dispatcher
// tools (~10 today) plus a handful of MCP tools stays under
// it; once an operator wires a scraper + broker + memory
// MCP, total climbs past 20 and deferred loading kicks in.
const DefaultDeferredToolThreshold = 20

// DegradedDeferredToolThreshold is the effective threshold used when
// the chat session's context-budget tier is DEGRADING or worse.
// Setting it to 1 forces deferred loading regardless of catalog size,
// shrinking the visible tool schema to the bare minimum + tool_search.
// Prompt token pressure during context exhaustion is exactly the
// situation where every saved descriptor matters.
const DegradedDeferredToolThreshold = 1

// effectiveDeferralThreshold collapses the (configured threshold,
// context tier) pair into the single threshold value
// applyDeferredLoading consumes. When the tier is DEGRADING or POOR,
// the threshold is forced down so deferral kicks in on catalogs that
// would otherwise stay below the cap.
func effectiveDeferralThreshold(threshold int, tier chat.ContextTier) int {
	if tier.IsDegraded() {
		return DegradedDeferredToolThreshold
	}
	return threshold
}

// ToolSearchName is the tool-call name the model uses to
// search the MCP catalog. Exported because the Telegram
// onboarding hints reference it.
const ToolSearchName = "tool_search"

// expandedToolStore is the per-session "I've already
// uncovered these MCP tools via tool_search" set. Lives on
// the Agent; reset implicitly when the Agent is recreated
// (e.g. daemon restart) — the model's next conversation turn
// will re-call tool_search to re-expand the names it needs.
//
// Keyed by ChatID. A zero ChatID is treated as "no session"
// — every call sees the empty set. Useful for the per-task
// agent code path that doesn't carry a chat session.
type expandedToolStore struct {
	mu sync.Mutex
	// keys[chatID] -> set of fully-qualified MCP tool names
	keys map[int64]map[string]struct{}
}

func newExpandedToolStore() *expandedToolStore {
	return &expandedToolStore{keys: make(map[int64]map[string]struct{})}
}

func (s *expandedToolStore) expand(chatID int64, names []string) {
	if s == nil || chatID == 0 || len(names) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.keys[chatID]
	if !ok {
		set = make(map[string]struct{}, len(names))
		s.keys[chatID] = set
	}
	for _, n := range names {
		set[n] = struct{}{}
	}
}

func (s *expandedToolStore) contains(chatID int64, name string) bool {
	if s == nil || chatID == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.keys[chatID]
	if !ok {
		return false
	}
	_, found := set[name]
	return found
}

// reset drops one session's expanded set (e.g. /new wipes
// conversation state). Currently unused — kept for the
// future Telegram /new wiring.
func (s *expandedToolStore) reset(chatID int64) {
	if s == nil || chatID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, chatID)
}

// toolSearchDescriptor is the chat.Tool definition the
// dispatcher injects into the visible set when deferred
// loading is active. Kept in a function so tests can rebuild
// the JSON payload without depending on tools.go's globals.
func toolSearchDescriptor() chat.Tool {
	return chat.Tool{
		Type: "function",
		Function: chat.ToolFunction{
			Name:        ToolSearchName,
			Description: "Search the project's MCP tool catalog by topic. Use this whenever you suspect an external integration exists for what the user asked but you don't see it in the visible tool list yet (your catalog is intentionally trimmed when many MCP servers are wired). Returns matching tools and unlocks them for direct call in subsequent turns of THIS conversation.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"query":{"type":"string","description":"Free-text topic (e.g. 'gmail send', 'calendar list events', 'place a stock order'). Match-by-overlap; doesn't have to be exact."},
					"limit":{"type":"integer","description":"Max matching tools to return. Default 8, max 20."}
				},
				"required":["query"]
			}`),
		},
	}
}

// applyDeferredLoading is the function `allTools` delegates
// to. When totalTools <= threshold, returns (builtin + every
// MCP tool) unchanged. When > threshold, returns (builtin +
// tool_search + only the MCP tools the session has previously
// expanded). The threshold counts MCP tools only — the
// built-in slice is always visible regardless.
//
// Pure-ish: reads from the expanded-set store but never
// writes. tool_search execution does the writing.
func applyDeferredLoading(builtin, mcp []chat.Tool, store *expandedToolStore, chatID int64, threshold int) []chat.Tool {
	if threshold <= 0 {
		threshold = DefaultDeferredToolThreshold
	}
	if chatID == 0 || len(mcp) <= threshold {
		// chatID=0 means "no session to anchor expansions to"
		// — sub-agent / per-task paths. Without a session
		// there's no place to track expansions, so we fall
		// back to legacy "everything visible".
		//
		// Below threshold: deferral overhead isn't worth it.
		return append(append(make([]chat.Tool, 0, len(builtin)+len(mcp)), builtin...), mcp...)
	}
	// Above threshold: hide MCP tools by default, surface the
	// search helper, expand whatever the session has uncovered.
	out := make([]chat.Tool, 0, len(builtin)+1+len(mcp))
	out = append(out, builtin...)
	out = append(out, toolSearchDescriptor())
	if store == nil {
		return out
	}
	for _, t := range mcp {
		if store.contains(chatID, t.Function.Name) {
			out = append(out, t)
		}
	}
	return out
}

// toolSearch is the dispatcher handler invoked when the model
// calls `tool_search`. Scores every MCP tool against the
// query, returns the top-N with their names + descriptions,
// AND records the matches in the session's expanded set so
// subsequent turns see them in the visible schema.
//
// The MCPExecutor.Tools(project) is called fresh per search
// — the catalog can change mid-session (operator added an
// MCP, fsnotify reloaded) and we want the search to see the
// current state.
func (te *ToolExecutor) toolSearch(argsJSON string, activeProject string, chatID int64) ToolResult {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return ToolResult{Content: "query is required."}
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}
	if te.mcpManager == nil || activeProject == "" {
		return ToolResult{Content: "Tool search is not available — no MCP servers are configured for this project."}
	}
	catalog := te.mcpManager.Tools(activeProject)
	if len(catalog) == 0 {
		return ToolResult{Content: "No MCP tools are configured for this project."}
	}
	scored := scoreTools(catalog, query)
	if len(scored) > limit {
		scored = scored[:limit]
	}
	if len(scored) == 0 {
		return ToolResult{Content: fmt.Sprintf("No tools matched %q.", query)}
	}
	names := make([]string, 0, len(scored))
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matching tool(s) for %q. They are now callable in this conversation:\n\n", len(scored), query)
	for _, hit := range scored {
		names = append(names, hit.tool.Function.Name)
		fmt.Fprintf(&b, "• %s\n  %s\n\n", hit.tool.Function.Name, hit.tool.Function.Description)
	}
	if te.expanded != nil {
		te.expanded.expand(chatID, names)
	}
	return ToolResult{Content: b.String(), Provenance: outputguard.ProvenanceFirstParty}
}

// toolHit pairs a chat.Tool with its computed score so the
// caller can sort and truncate without re-running the scorer.
type toolHit struct {
	tool  chat.Tool
	score float64
}

// scoreTools ranks the catalog against the query using a
// cheap lexical score: tokens from the query that appear in
// the tool's qualified name or description contribute to its
// score, weighted toward the name (where authors usually
// encode the topic verbatim).
//
// Not BM25 — for a 100-tool catalog the cost difference is
// trivial and the qualitative ranking is close enough. If
// false-positive recall ever becomes a problem we can swap
// in a real BM25 (the memory package already has the
// primitives via consolidate.go).
//
// Returns the catalog sorted by score descending, alphabetic
// tiebreak so output is deterministic across runs. Zero-
// score tools are excluded.
func scoreTools(catalog []chat.Tool, query string) []toolHit {
	terms := tokeniseSearchQuery(query)
	if len(terms) == 0 {
		return nil
	}
	hits := make([]toolHit, 0, len(catalog))
	for _, t := range catalog {
		name := strings.ToLower(t.Function.Name)
		desc := strings.ToLower(t.Function.Description)
		var score float64
		for _, term := range terms {
			if strings.Contains(name, term) {
				score += 3.0 // name match wins
			}
			if strings.Contains(desc, term) {
				score += 1.0
			}
		}
		if score > 0 {
			hits = append(hits, toolHit{tool: t, score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].tool.Function.Name < hits[j].tool.Function.Name
	})
	return hits
}

// tokeniseSearchQuery is the tool_search-side tokeniser.
// Lower-case, alphanumeric runs only, drops 1-char tokens
// (which match too eagerly). Distinct from
// memory/consolidate.go's tokeniser because tool descriptions
// are short and the stopword list there would over-prune
// (we want "send" / "list" / "get" as discriminators here,
// which the memory stopword list discards).
func tokeniseSearchQuery(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	out := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) < 2 {
			continue
		}
		out = append(out, w)
	}
	return out
}

// Compile-time guard that ToolExecutor really does carry the
// expanded-tool store. Keeps the wiring honest if a refactor
// drops the field; the tests would fail to compile rather
// than silently regress to "deferred loading never expands".
var _ = func(te *ToolExecutor) *expandedToolStore { return te.expanded }

// Ensure context is imported even when only some paths use
// it; the dispatcher loop calls toolSearch indirectly from
// Execute which holds the ctx — keep the package import in
// case a future caller threads ctx into toolSearch itself
// (e.g. a tool-catalog query that hits the MCP servers
// live).
var _ = context.Background
