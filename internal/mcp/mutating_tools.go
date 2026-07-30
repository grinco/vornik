package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Classification of an advertised tool as read-only or mutating, and the
// connect-time gate that uses it.
//
// WHY THIS EXISTS. A third-party MCP server's tool set belongs to the third
// party. `allowed_tools` in our YAML is an allowlist we maintain against a
// catalogue they can change at any release — so a static list cannot, on its
// own, notice that the upstream started advertising a destructive tool. The
// list is ours; the tool set is theirs. Classification therefore happens at
// connect time, against what the server actually advertises.
//
// see LLD § https://docs.vornik.io §10.2

// ToolAnnotations is the MCP tool-annotation block. Both hints are pointers
// because "absent" and "false" mean different things here: absent means the
// server said nothing and the heuristic decides, false means the server
// asserted the tool is not read-only.
type ToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
}

// readVerbs is an ALLOWLIST of leading verbs that mark a tool as read-only.
//
// Deliberately an allowlist rather than a denylist of mutating verbs. A denylist
// fails open on the verb nobody anticipated — and the verb nobody anticipated is
// precisely the one that matters, because it is the one a future release
// introduces. So an unrecognised name is classified MUTATING and has to be
// declared explicitly. The cost is a line of config for an oddly-named read
// tool; the alternative cost is silently exposing a destructive one.
var readVerbs = map[string]struct{}{
	"get": {}, "list": {}, "read": {}, "search": {}, "find": {}, "fetch": {},
	"query": {}, "describe": {}, "show": {}, "view": {}, "count": {},
	"export": {}, "download": {}, "lookup": {}, "check": {}, "resolve": {},
}

// hostPathParams are input-parameter names that mean "a path on OUR filesystem".
//
// A tool taking one of these is mutating whatever its verb suggests, because what
// it changes is this machine rather than the remote service. Found in Google's own
// Workspace MCP server (v0.0.8): `drive_downloadFile`, `gmail_downloadAttachment`,
// `slides_getImages` and `slides_getSlideThumbnail` all take an absolute
// `localPath` and write to it, and every one of them begins with a verb in the
// read allowlist above. `gmail_createDraft` is the mirror case — it READS
// `attachments[].filePath` — which is equally disqualifying, because the process
// runs as the daemon user and can therefore read the daemon's secrets.
//
// Generic names (`path`, `dir`) are included deliberately. A remote-path parameter
// that happens to be called `path` will be classified mutating and cost the
// operator one line of `allowed_tools`; the opposite error costs arbitrary file
// access as the daemon user.
var hostPathParams = map[string]struct{}{
	"localpath": {}, "filepath": {}, "outputpath": {}, "savepath": {},
	"targetpath": {}, "destinationpath": {}, "downloadpath": {}, "localfile": {},
	"localdir": {}, "localdirectory": {}, "outputdir": {}, "outputdirectory": {},
	"destination": {}, "path": {}, "dir": {}, "directory": {}, "folder": {},
}

// maxSchemaDepth bounds the schema walk. Schemas come from a third party and may
// be generated, deeply nested, or hostile; the walker stops rather than risking a
// stack overflow in the daemon's connect path.
const maxSchemaDepth = 12

// IsMutating reports whether this tool may change state.
//
// Precedence, strongest first:
//
//  1. **A host filesystem parameter.** Not overridable by anything the server
//     says — see hostPathParams.
//  2. **destructiveHint: true.** A server that sets both hints has contradicted
//     itself, and a contradiction reads destructive.
//  3. **readOnlyHint.** Authoritative where present; the server knows its own
//     semantics better than a verb table does.
//  4. **The name.** Anything not recognisably a read verb is treated as mutating.
func (t Tool) IsMutating() bool {
	mutating, _ := t.classify()
	return mutating
}

// HostFilesystemTools names the advertised tools that take a path on this host,
// with the offending parameter, sorted for stable diagnostics.
//
// Surfaced at connect time because nothing in a name like `slides_getImages` tells
// an operator it writes to their disk, and the OAuth scope list does not either.
func HostFilesystemTools(tools []Tool) []string {
	var out []string
	for _, t := range tools {
		if params := hostPathProperties(t.InputSchema); len(params) > 0 {
			out = append(out, fmt.Sprintf("%s (host path parameter: %s)",
				t.Name, strings.Join(params, ", ")))
		}
	}
	sort.Strings(out)
	return out
}

// hostPathProperties returns the sorted input-schema property names that denote a
// path on this host. An absent or unparseable schema yields nothing: we cannot
// assert a host path we could not read, and the name heuristic still applies.
func hostPathProperties(schema json.RawMessage) []string {
	if len(schema) == 0 {
		return nil
	}
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil
	}
	found := map[string]struct{}{}
	walkSchemaProperties(root, 0, func(name string) {
		if _, ok := hostPathParams[normaliseParamName(name)]; ok {
			found[name] = struct{}{}
		}
	})
	if len(found) == 0 {
		return nil
	}
	out := make([]string, 0, len(found))
	for n := range found {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// walkSchemaProperties visits every property name reachable in a JSON Schema,
// descending through `properties`, array `items`, and the composition keywords.
// Nested descent is required, not thorough-for-its-own-sake: the real
// `gmail_createDraft` hides its host path in `attachments[].items.filePath`.
func walkSchemaProperties(node any, depth int, visit func(string)) {
	if depth > maxSchemaDepth {
		return
	}
	obj, ok := node.(map[string]any)
	if !ok {
		return
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		for name, sub := range props {
			visit(name)
			walkSchemaProperties(sub, depth+1, visit)
		}
	}
	for _, key := range []string{"items", "additionalProperties", "$defs", "definitions"} {
		walkSchemaProperties(obj[key], depth+1, visit)
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if variants, ok := obj[key].([]any); ok {
			for _, v := range variants {
				walkSchemaProperties(v, depth+1, visit)
			}
		}
	}
}

// normaliseParamName folds case and drops separators so `local_path`, `localPath`
// and `local-path` are one name.
func normaliseParamName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// classify returns the verdict and whether a server annotation OVERRODE what the
// name alone would have concluded.
//
// The override flag exists for observability, not for logic. A server asserting
// readOnlyHint on a tool called `drive_delete` is believed — it knows its own
// semantics — but that is exactly the claim an operator would want to see in a log
// rather than discover afterwards, so the caller records it.
//
// DestructiveHint outranks ReadOnlyHint: a server that sets both has contradicted
// itself, and the safe reading of a contradiction is the destructive one.
func (t Tool) classify() (mutating, overrodeName bool) {
	byName := t.mutatingByName()
	// A host filesystem path outranks every annotation. readOnlyHint is a claim
	// about the REMOTE service; a third party has no standing to declare a write
	// to our disk safe. Not reported as a name override — HostFilesystemTools
	// names these separately, with the parameter, which is the more useful signal.
	if len(hostPathProperties(t.InputSchema)) > 0 {
		return true, false
	}
	if a := t.Annotations; a != nil {
		switch {
		case a.DestructiveHint != nil && *a.DestructiveHint:
			return true, !byName
		case a.ReadOnlyHint != nil:
			annotated := !*a.ReadOnlyHint
			return annotated, annotated != byName
		}
	}
	return byName, false
}

// mutatingByName applies the read-verb allowlist to every word in the name.
func (t Tool) mutatingByName() bool {
	for _, seg := range nameSegments(t.Name) {
		if _, ok := readVerbs[seg]; ok {
			return false
		}
	}
	return true
}

// nameSegments splits a tool name into lower-case words.
//
// Splits on any non-alphanumeric separator AND on camelCase boundaries, because
// real MCP servers use several conventions at once and we do not get to pick. The
// Workspace server alone advertises `drive_search` (snake), `calendar_listEvents`
// (namespace plus camelCase), and `calendar.listEvents` when launched with
// `--use-dot-names`. Splitting on "_" alone — the original implementation — left
// `getSuggestions` as a single unrecognised word, classifying ordinary read tools
// as mutating.
func nameSegments(name string) []string {
	runes := []rune(strings.TrimSpace(name))
	var segs []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			segs = append(segs, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	for i, r := range runes {
		switch {
		case unicode.IsUpper(r):
			// Break before an upper-case rune that starts a new word: either the
			// previous rune was lower-case or a digit ("listEvents"), or this is
			// the last capital of an acronym run followed by lower-case
			// ("exportPDFFile" → export, pdf, file).
			prevLower := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			acronymEnd := i > 0 && i+1 < len(runes) &&
				unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1])
			if prevLower || acronymEnd {
				flush()
			}
			cur.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return segs
}

// AnnotationOverrides returns the tools whose server-declared annotation
// contradicted the name heuristic, with the direction it moved them.
//
// Surfaced so a third-party server cannot quietly reclassify a destructive-looking
// tool as safe without leaving a trace. Raised in review of the Phase 0
// implementation: the override behaviour is correct and was unobservable.
func AnnotationOverrides(tools []Tool) []string {
	var out []string
	for _, t := range tools {
		mutating, overrode := t.classify()
		if !overrode {
			continue
		}
		direction := "name suggests mutating, server declares read-only"
		if mutating {
			direction = "name suggests read-only, server declares destructive"
		}
		out = append(out, fmt.Sprintf("%s (%s)", t.Name, direction))
	}
	sort.Strings(out)
	return out
}

// undeclaredMutatingTools returns every advertised mutating tool that the
// operator's allowlist does not name, sorted for stable diagnostics.
//
// Returns all offenders rather than the first: fixing one and rediscovering the
// next on the following connect wastes a cycle, and an operator reading a
// warning deserves the whole list.
func undeclaredMutatingTools(allowed map[string]struct{}, tools []Tool) []string {
	var out []string
	for _, t := range tools {
		if !t.IsMutating() {
			continue
		}
		if _, declared := allowed[t.Name]; declared {
			continue
		}
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// gateAdvertisedTools decides whether a server may register, given what it
// advertises and what the operator declared.
//
// THE RULE, in two parts:
//
//  1. **Opt-in per server.** Refusal happens only when the server sets
//     `require_declared_tools`. This is NOT a global default, and the reason is
//     concrete rather than cautious: on the reference deployment four servers run
//     expose-all with legitimately mutating tool sets — a page publisher, a
//     home-automation bridge, a scraper with submit actions. A retroactive global
//     rule would have refused to register integrations that work today, which is a
//     self-inflicted outage dressed up as a security control.
//
//  2. **Where opted in: refuse when `allowed_tools` is empty and a mutating tool
//     is advertised.** That configuration says expose-everything, and
//     expose-everything now includes something destructive. Where an allowlist
//     DOES exist, the filter already keeps the undeclared tool away from agents,
//     so registration proceeds and the caller warns instead — taking the whole
//     integration down because the upstream gained a tool would be an outage on a
//     schedule the third party controls.
//
// The asymmetry is the point: refuse where the change would actually widen what an
// agent can reach, warn where it would not.
func gateAdvertisedTools(server string, requireDeclared bool, allowed map[string]struct{}, tools []Tool) error {
	if !requireDeclared || len(allowed) > 0 {
		return nil
	}
	offenders := undeclaredMutatingTools(nil, tools)
	if len(offenders) == 0 {
		return nil
	}
	return fmt.Errorf(
		"mcp: refusing to register server %q: it advertises mutating tool(s) %s and "+
			"allowed_tools is empty, which would expose them to agents. List the tools this "+
			"server may expose in allowed_tools — including any mutating tool you intend to "+
			"permit — and reconnect (this server sets require_declared_tools)",
		server, strings.Join(offenders, ", "))
}

// allowedSubset returns the advertised tools an allowlist actually exposes. A nil
// allowlist means expose-everything, so every tool is returned — the distinction
// that makes the difference between "advertised" and "reachable by an agent".
func allowedSubset(allowed map[string]struct{}, tools []Tool) []Tool {
	if allowed == nil {
		return tools
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if _, ok := allowed[t.Name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// allowedToolSet builds the lookup set for a configured allowlist. Returns nil
// for an empty list, which the gate reads as "expose everything" — the
// distinction the gate turns on, so it is expressed once here rather than
// re-derived at each call site.
func allowedToolSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}
