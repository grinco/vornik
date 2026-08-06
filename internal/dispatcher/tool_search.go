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
	"regexp"
	"sort"
	"strconv"
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

// mcpToolNameRe matches a fully-qualified MCP tool name
// (mcp__<server>__<tool>) mentioned in prose. Wildcards like
// "mcp__*" don't match — '*' is outside the class — so allowlist
// snippets pasted into a prompt can't pin the whole catalog.
var mcpToolNameRe = regexp.MustCompile(`\bmcp__[A-Za-z0-9-]+__[A-Za-z0-9_-]+`)

// extractPinnedMCPTools scans a chat system prompt for explicitly
// documented MCP tool names. Tools the operator names in the prompt are
// treated as pinned: deferred loading never hides them.
//
// Regression guard for the 2026-07-15 pagedrop incident: the assistant
// project's system prompt documented mcp__pagedrop__pagedrop_protect by
// name, but the catalog had crossed the deferral threshold so the tool
// was absent from the advertised function list — and the model refused
// ("I don't have access to the MCP") without calling tool_search. A
// prompt that names a tool is an operator statement that the model must
// see it.
func extractPinnedMCPTools(systemPrompt string) map[string]struct{} {
	matches := mcpToolNameRe.FindAllString(systemPrompt, -1)
	if len(matches) == 0 {
		return nil
	}
	pinned := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		pinned[m] = struct{}{}
	}
	return pinned
}

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
	// keys[sessionKey] -> set of fully-qualified MCP tool names.
	//
	// Keyed by an OPAQUE session string, not a Telegram chat id. It was
	// map[int64] until 2026-08-05, which silently disabled deferred loading —
	// and with it tool_search — on every channel that has a session but no
	// numeric chat id: Slack, email, GitHub. See deferralSessionKey.
	keys map[string]map[string]struct{}
}

// deferralSessionKey is the session identity deferred loading anchors
// expansions to. It is NOT the same thing as Request.ChatID.
//
// ChatID is documented as the platform's NUMERIC chat identifier, used by tools
// that send files back, and channels without one are told to leave it 0 —
// GitHub, Slack and email all do. Deferred loading then read that same field as
// "is there a session here at all?", so those channels were misclassified as
// sub-agent invocations: deferral switched itself off and tool_search was never
// advertised. On a project with 33 MCP tools that meant the full catalog on
// every turn AND no way for the model to search it — reported 2026-08-05 as
// "the dispatcher says it has no tooling for mcp search".
//
// Order matters: a channel session id is the more specific identity, and
// Telegram (which sets ChatID and no OriginatingSessionID) falls through to the
// numeric form. Empty means genuinely no session — sub-agent and per-task
// paths — where skipping deferral is correct.
func deferralSessionKey(req Request) string {
	if s := strings.TrimSpace(req.OriginatingSessionID); s != "" {
		return req.OriginatingChannel + ":" + s
	}
	if req.ChatID != 0 {
		return strconv.FormatInt(req.ChatID, 10)
	}
	return ""
}

func newExpandedToolStore() *expandedToolStore {
	return &expandedToolStore{keys: make(map[string]map[string]struct{})}
}

func (s *expandedToolStore) expand(sessionKey string, names []string) {
	if s == nil || sessionKey == "" || len(names) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.keys[sessionKey]
	if !ok {
		set = make(map[string]struct{}, len(names))
		s.keys[sessionKey] = set
	}
	for _, n := range names {
		set[n] = struct{}{}
	}
}

func (s *expandedToolStore) contains(sessionKey string, name string) bool {
	if s == nil || sessionKey == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.keys[sessionKey]
	if !ok {
		return false
	}
	_, found := set[name]
	return found
}

// reset drops one session's expanded set (e.g. /new wipes
// conversation state). Currently unused — kept for the
// future Telegram /new wiring.
func (s *expandedToolStore) reset(sessionKey string) {
	if s == nil || sessionKey == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, sessionKey)
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
			Description: "Search the project's MCP tool catalog by topic. Use this whenever you suspect an external integration exists for what the user asked but you don't see it in the visible tool list yet (your catalog is intentionally trimmed when many MCP servers are wired). Returns matching tools and unlocks them for direct call in subsequent turns of THIS conversation — along with every other tool on the same server, because a server's tools share one account and one tenant. So if a call fails asking for an id or tenant you don't have (a cloudId, a workspace id, an account id), the tool that returns it is almost certainly already unlocked on that same server: look there before asking the user for it.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"query":{"type":"string","description":"Free-text topic (e.g. 'gmail send', 'calendar list events', 'place a stock order'). Match-by-overlap; doesn't have to be exact."},
					"limit":{"type":"integer","description":"Max matching tools to DESCRIBE in detail. Default 8, max 20. Raise it when the result says more matched than were shown. This caps the detailed matches only — every tool on a matched server is unlocked either way."}
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
func applyDeferredLoading(builtin, mcp []chat.Tool, store *expandedToolStore, sessionKey string, threshold int, pinned map[string]struct{}) []chat.Tool {
	if threshold <= 0 {
		threshold = DefaultDeferredToolThreshold
	}
	if sessionKey == "" || len(mcp) <= threshold {
		// An empty session key means "no session to anchor expansions
		// to" — sub-agent / per-task paths. Without a session there's no
		// place to track expansions, so we fall back to legacy
		// "everything visible". A CHANNEL session is never empty here;
		// it was, while this keyed on a Telegram chat id, which is how
		// Slack/email/GitHub lost tool_search entirely.
		//
		// Below threshold: deferral overhead isn't worth it.
		return append(append(make([]chat.Tool, 0, len(builtin)+len(mcp)), builtin...), mcp...)
	}
	// Above threshold: hide MCP tools by default, surface the
	// search helper, expand whatever the session has uncovered —
	// plus any tool the operator pinned by naming it in the system
	// prompt (see extractPinnedMCPTools; 2026-07-15 pagedrop
	// incident). Pinned visibility must not depend on the store:
	// it applies even when no expansion tracking is wired.
	out := make([]chat.Tool, 0, len(builtin)+1+len(mcp))
	out = append(out, builtin...)
	out = append(out, toolSearchDescriptor())
	for _, t := range mcp {
		_, isPinned := pinned[t.Function.Name]
		if isPinned || store.contains(sessionKey, t.Function.Name) {
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
func (te *ToolExecutor) toolSearch(argsJSON string, activeProject string, sessionKey string) ToolResult {
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
	if len(scored) == 0 {
		return ToolResult{Content: fmt.Sprintf("No tools matched %q.", query)}
	}
	total := len(scored)
	if len(scored) > limit {
		scored = scored[:limit]
	}

	// Unlock the WHOLE server behind every match, not just the matched tools.
	// Tools inside one MCP server share its tenant and auth context, and often
	// require an argument only a sibling can produce — so a lexically-selected
	// subset can be a palette that cannot work. Atlassian is the worked example
	// that forced this (2026-08-05): every Jira tool REQUIRES a cloudId, and the
	// only tool that yields one is getAccessibleAtlassianResources, whose name
	// and description contain neither "jira" nor any other word an operator
	// would search. tool_search("Jira") returned 8 Jira-named tools and the
	// model, correctly reasoning from what it could see, concluded no
	// site-enumeration tool existed and asked the user for a cloudId it had no
	// way to supply. tool_search("atlassian") had worked minutes earlier in a
	// DM, purely because that query happens to rank the bootstrap tool inside
	// the cut — which is luck, not a contract.
	matched := make(map[string]struct{}, len(scored))
	servers := make([]string, 0, 4)
	seenServer := make(map[string]struct{}, 4)
	for _, hit := range scored {
		matched[hit.tool.Function.Name] = struct{}{}
		if srv, ok := mcpServerOfTool(hit.tool.Function.Name); ok {
			if _, dup := seenServer[srv]; !dup {
				seenServer[srv] = struct{}{}
				servers = append(servers, srv)
			}
		}
	}
	siblings := make(map[string][]chat.Tool, len(servers))
	for _, t := range catalog {
		if _, already := matched[t.Function.Name]; already {
			continue
		}
		srv, ok := mcpServerOfTool(t.Function.Name)
		if !ok {
			continue
		}
		if _, wanted := seenServer[srv]; wanted {
			siblings[srv] = append(siblings[srv], t)
		}
	}

	names := make([]string, 0, len(scored))
	var b strings.Builder
	// Report the TOTAL, not the truncated count. Saying "found 8" when 9 matched
	// tells the model it has seen everything and there is nothing left to look
	// for — the cap has to be visible to be worked around.
	if total > len(scored) {
		fmt.Fprintf(&b, "Found %d tool(s) matching %q; showing the top %d (raise `limit`, max 20, to see more). Now callable in this conversation:\n\n",
			total, query, len(scored))
	} else {
		fmt.Fprintf(&b, "Found %d matching tool(s) for %q. They are now callable in this conversation:\n\n", total, query)
	}
	for _, hit := range scored {
		names = append(names, hit.tool.Function.Name)
		fmt.Fprintf(&b, "• %s\n  %s\n\n", hit.tool.Function.Name, hit.tool.Function.Description)
	}
	for _, srv := range servers {
		rest := siblings[srv]
		if len(rest) == 0 {
			continue
		}
		shown := rest
		if len(shown) > maxSiblingToolsListed {
			shown = shown[:maxSiblingToolsListed]
		}
		fmt.Fprintf(&b, "Also unlocked — the rest of the %q server, since its tools share one account and often need an argument only a sibling returns:\n", srv)
		for _, t := range shown {
			names = append(names, t.Function.Name)
			fmt.Fprintf(&b, "• %s — %s\n", t.Function.Name, firstSentence(t.Function.Description))
		}
		if len(rest) > len(shown) {
			// Unlock them anyway: callable beats listed, and the model can find
			// the name with a narrower query.
			for _, t := range rest[len(shown):] {
				names = append(names, t.Function.Name)
			}
			fmt.Fprintf(&b, "  (+%d more from %q, unlocked but not listed — search a narrower term to see them)\n", len(rest)-len(shown), srv)
		}
		b.WriteString("\n")
	}
	if te.expanded != nil {
		te.expanded.expand(sessionKey, names)
	}
	return ToolResult{Content: b.String(), Provenance: outputguard.ProvenanceFirstParty}
}

// maxSiblingToolsListed bounds how many same-server tools are DESCRIBED after
// the matches. All of them are unlocked regardless; this only caps the prose, so
// a 57-tool server does not bury the actual answer.
const maxSiblingToolsListed = 12

// mcpServerOfTool returns the server segment of an mcp__{server}__{tool} name.
// Only the server is returned because that is all the sibling grouping needs;
// internal/mcp has its own full parser, and the dispatcher must not import that
// package for one string split (the dependency arrow points the other way).
func mcpServerOfTool(name string) (server string, ok bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	parts := strings.SplitN(name[len(prefix):], "__", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0], true
}

// firstSentence trims a tool description to its first sentence so the
// sibling list stays scannable. Atlassian's descriptions are terse ("Get issue
// details") but some servers ship paragraphs.
func firstSentence(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return "(no description)"
	}
	if i := strings.IndexByte(desc, '.'); i > 0 && i < 160 {
		return desc[:i+1]
	}
	if len(desc) > 160 {
		return desc[:157] + "..."
	}
	return desc
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
