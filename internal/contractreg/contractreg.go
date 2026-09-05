// Package contractreg enumerates every surface in this repo that names an
// entry point by STRING rather than calling it, into one table.
//
// Why this exists: most of vornik's interesting entry points are dispatched by
// name, not by call site — agent tools from images/vornik-agent/entrypoint.sh,
// system step handlers from workflow YAML, roles from configs, extractors by
// MIME type. A Go call graph cannot see any of them, so any analysis built on
// reachability alone reports live code as dead. This table is the second source
// of truth that makes such analysis correct.
//
// Two consumers read it in opposite directions
// (https://docs.vornik.io §4):
//
//   - DRIFT asks "does everything the contracts name exist?"
//   - REACHABILITY asks "is everything that exists either called or named?"
//
// Extraction is BY IMPORT wherever the surface exposes an enumerator, because a
// regex over Go source breaks the moment someone reformats. Only the shell
// surfaces in entrypoint.sh need a text parse, and those are parsed against the
// real file in tests so a refactor of the shell fails loudly rather than
// silently extracting nothing.
package contractreg

import (
	"fmt"
	"sort"
	"strings"
)

// Kind identifies which surface declared a name. Several kinds can carry the
// same Name — that is the point: comparing kinds is how disagreement between
// registries is detected (see CheckAgentToolAgreement).
type Kind string

const (
	// KindAgentToolGo is internal/agenttools.builtinTools — the list the
	// linters and the role-library validator accept.
	KindAgentToolGo Kind = "agent_tool_go"

	// KindAgentToolAdvertised is BUILTIN_TOOL_NAMES_JSON — since 2026-09-03
	// generated into images/vornik-agent/tool_registry.generated.sh from
	// agenttools.Offerable() and sourced by the entrypoint. The "declared" set
	// the fail-closed advertisement filter consults and is_builtin_tool() reads.
	KindAgentToolAdvertised Kind = "agent_tool_advertised"

	// KindAgentToolRegistry is every entry of TOOL_REGISTRY_JSON in the
	// generated registry — name plus definition. What tool_definitions() can
	// offer at all.
	KindAgentToolRegistry Kind = "agent_tool_registry"

	// KindAgentToolNeverAdvertised is the registry entries whose advertise token
	// is "never": tool_definitions() skips them and another path must append
	// them by name, or they are declared and unreachable.
	KindAgentToolNeverAdvertised Kind = "agent_tool_never_advertised"

	// KindAgentToolAdvertiseToken is ADVERTISE_TOKENS_JSON — the closed set of
	// advertise conditions the generator emits (agenttools.AdvertiseTokens()).
	KindAgentToolAdvertiseToken Kind = "agent_tool_advertise_token"

	// KindAgentToolAdvertiseCase is the case labels of the entrypoint's
	// tool_advertised_now() — the one function that maps a token to an
	// environment test. Compared against the tokens in both directions, the
	// same shape as dispatch: a token with no case advertises nothing, a case
	// with no token is dead code that reads like a rule.
	KindAgentToolAdvertiseCase Kind = "agent_tool_advertise_case"

	// KindAgentToolAppendedByName is every name the entrypoint passes to
	// tool_definition_for — the path a never-advertised tool reaches the model
	// by (tool_search from rebuild_tools_file). Presence only; whether the call
	// sits behind a live condition is what the shell tests exercise.
	KindAgentToolAppendedByName Kind = "agent_tool_appended_by_name"

	// KindAgentToolInlineExempt is the set of tool names the execution gate
	// exempts INLINE, e.g.
	//   [ "$name" != "tool_search" ] && [ "$name" != "tool_result_read" ] && …
	// It exists so those names can be compared against UngatedByDesign, the Go
	// registry that is supposed to be the single source of the exemption
	// vocabulary. Two hand-maintained copies of one list is the exact fault
	// contractreg was introduced to end.
	KindAgentToolInlineExempt Kind = "agent_tool_inline_exempt"

	// KindAgentToolUngatedPrefix is entrypoint.sh's UNGATED_TOOL_PREFIXES_JSON —
	// name prefixes the container deliberately does not gate because something
	// else does. Compared against UngatedPrefixesByDesign; a prefix is a wider
	// grant than a name, so it carries the same recorded-reason requirement.
	KindAgentToolUngatedPrefix Kind = "agent_tool_ungated_prefix"

	// KindAgentToolAdvertisementFilter holds presence MARKERS, not a vocabulary:
	// structural facts about the entrypoint whose ABSENCE is the finding.
	//
	//   fail-closed-filter        tool_definitions() keeps a definition only if exempt,
	//                             or declared AND on the role's allowlist (2026-08-20)
	//   definitions-registry-only tool_definitions() reads TOOL_REGISTRY_JSON and carries
	//                             no inline definition and no heredoc — there is no
	//                             append step, so the 2026-08-22 class (a definition
	//                             reaching the model without consulting a registry)
	//                             has no place to happen
	//   registry-sourced          the entrypoint sources tool_registry.generated.sh
	//                             before its first use of a registry variable
	//   registry-declared-inline  the entrypoint ALSO declares a registry variable —
	//                             a second copy; its PRESENCE is the finding
	//   gate-reads-registry       is_builtin_tool() consults BUILTIN_TOOL_NAMES_JSON
	//                             rather than a hand-written case list
	//   advertise-default-refuses tool_advertised_now()'s default arm returns 1
	//
	// The 2026-08-22 marker ("ungated-append") is retired with the append.
	KindAgentToolAdvertisementFilter Kind = "agent_tool_advertisement_filter"

	// KindAgentToolDispatch is entrypoint.sh's exec_tool case list: what can
	// actually run. This is the authoritative set of implemented agent tools.
	KindAgentToolDispatch Kind = "agent_tool_dispatch"

	// KindAgentToolHelperDispatch is internal/agentloop.Handlers: the tools
	// whose dispatch case is the Go helper rather than a bash case
	// (agent-tool dispatch design §4). Added by the lint from HandlerNames().
	KindAgentToolHelperDispatch Kind = "agent_tool_helper_dispatch"

	// KindAgentToolHelperListed is the generated HELPER_TOOL_NAMES_JSON — what
	// exec_tool actually delegates. Compared against the declaration.
	KindAgentToolHelperListed Kind = "agent_tool_helper_listed"

	// KindAgentToolHelperBranch carries the line-order markers of exec_tool's
	// helper branch relative to its gate ("gate", "gate-end",
	// "helper-branch#N", "case"), each with the line number in Status, for
	// CheckHelperBranchIsGated.
	KindAgentToolHelperBranch Kind = "agent_tool_helper_branch"

	// KindSystemHandler is a workflow system-step handler name.
	KindSystemHandler Kind = "system_handler"

	// KindRole is a role-library archetypeId.
	KindRole Kind = "role"

	// KindExtractor is a document-extractor MIME type.
	KindExtractor Kind = "extractor"

	// KindMCPGrant is an mcp__<server>__<tool> name appearing in a role or
	// swarm allowlist. Server-side existence needs a live daemon, so these are
	// CONTRACT-ONLY: they can make an implementation live-by-contract but are
	// never themselves reported as phantom.
	KindMCPGrant Kind = "mcp_grant"

	// KindDeclared is an LLD `delivers:` entry — a documented promise to
	// create something. Status carries the declaring doc's implementation
	// status so the drift consumer can filter to shipped docs while the
	// reachability consumer ignores these rows entirely (a promise in prose is
	// not evidence that code is live).
	KindDeclared Kind = "declared"
)

// Entry is one named declaration found on one surface.
type Entry struct {
	Name string
	Kind Kind
	// Sources are "file:line" locations, at least one, sorted.
	Sources []string
	// Status is the declaring document's implementation status. Set only for
	// KindDeclared; empty otherwise.
	Status string
}

// Table holds every extracted declaration, keyed by (Kind, Name).
type Table struct {
	entries map[Kind]map[string]*Entry
}

// New returns an empty Table.
func New() *Table {
	return &Table{entries: map[Kind]map[string]*Entry{}}
}

// Add records a declaration. Repeated (kind, name) pairs merge their sources
// rather than duplicating, so a name declared on three lines of one file is one
// entry with three sources.
func (t *Table) Add(kind Kind, name, source string) {
	t.AddWithStatus(kind, name, source, "")
}

// AddWithStatus is Add plus the KindDeclared status column.
func (t *Table) AddWithStatus(kind Kind, name, source, status string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if t.entries == nil {
		t.entries = map[Kind]map[string]*Entry{}
	}
	if t.entries[kind] == nil {
		t.entries[kind] = map[string]*Entry{}
	}
	e := t.entries[kind][name]
	if e == nil {
		e = &Entry{Name: name, Kind: kind}
		t.entries[kind][name] = e
	}
	if status != "" {
		e.Status = status
	}
	if source != "" {
		for _, s := range e.Sources {
			if s == source {
				return
			}
		}
		e.Sources = append(e.Sources, source)
		sort.Strings(e.Sources)
	}
}

// Names returns the sorted names declared on one surface. An unpopulated kind
// returns an empty slice, never nil — an absent surface is not an error here,
// it is the caller's job to decide whether that matters.
func (t *Table) Names(kind Kind) []string {
	out := make([]string, 0, len(t.entries[kind]))
	for name := range t.entries[kind] {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Set returns the names of one surface as a set, for membership tests.
func (t *Table) Set(kind Kind) map[string]bool {
	out := make(map[string]bool, len(t.entries[kind]))
	for name := range t.entries[kind] {
		out[name] = true
	}
	return out
}

// Get returns the entry for (kind, name), or nil.
func (t *Table) Get(kind Kind, name string) *Entry {
	if t.entries[kind] == nil {
		return nil
	}
	return t.entries[kind][name]
}

// Kinds returns every populated kind, sorted, for reporting.
func (t *Table) Kinds() []Kind {
	out := make([]Kind, 0, len(t.entries))
	for k := range t.entries {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AnyNamed reports whether name is declared on ANY contract surface, ignoring
// KindDeclared (see its doc: a prose promise is not evidence of liveness). This
// is the live-by-contract predicate the reachability consumer uses.
func (t *Table) AnyNamed(name string) bool {
	for kind := range t.entries {
		if kind == KindDeclared {
			continue
		}
		if t.entries[kind][name] != nil {
			return true
		}
	}
	return false
}

// Finding is one contract defect.
type Finding struct {
	Check string
	Name  string
	// Detail explains the specific disagreement or absence.
	Detail string
	// Sources locates the declaration(s) involved.
	Sources []string
}

func (f Finding) String() string {
	s := fmt.Sprintf("%s: %s — %s", f.Check, f.Name, f.Detail)
	if len(f.Sources) > 0 {
		s += " [" + strings.Join(f.Sources, ", ") + "]"
	}
	return s
}
