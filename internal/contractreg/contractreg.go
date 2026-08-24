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

	// KindAgentToolAdvertised is entrypoint.sh's BUILTIN_TOOL_NAMES_JSON,
	// which filters which tool schemas are offered to the model.
	KindAgentToolAdvertised Kind = "agent_tool_advertised"

	// KindAgentToolGate is entrypoint.sh's is_builtin_tool() case list — the
	// EXECUTION-time allowlist gate. A name missing here is not gated at all,
	// which is a privilege bypass rather than a cosmetic inconsistency.
	KindAgentToolGate Kind = "agent_tool_gate"

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

	// KindAgentToolAdvertisementFilter records that tool_definitions()'s
	// $extras_ungated append is filtered against the exemption registry rather
	// than concatenated unconditionally.
	//
	// A presence marker, not a vocabulary: the thing worth asserting is that the
	// THIRD PATH is closed. Until 2026-08-22 the append was unconditional, so a
	// definition added to extras_ungated reached every role's model whatever the
	// allowlist said and whatever the registries said — a bypass that no amount
	// of registry agreement would have caught, because it went around the
	// registries rather than disagreeing with them.
	KindAgentToolAdvertisementFilter Kind = "agent_tool_advertisement_filter"

	// KindAgentToolDispatch is entrypoint.sh's exec_tool case list: what can
	// actually run. This is the authoritative set of implemented agent tools.
	KindAgentToolDispatch Kind = "agent_tool_dispatch"

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
