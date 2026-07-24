// Package taintlineage classifies autonomous-agent steps by whether they
// consumed untrusted (third-party) content, from the tool-audit list the
// executor already parses — with NO agent-image / result.json contract change
// (LLD 2026-07-24-taint-lineage-tracking-design.md §1.2/D0).
//
// The package is deliberately pure: no I/O, no persistence dependency. The
// executor marshals StepTaint.Sources at its boundary (exactly like
// HallucinationSignals) so internal/persistence gains no dependency on this
// package (D2). Both the executor (the SOURCE) and the api/executor write
// gates (the CONSUMER) import it, keeping the classification and the taint
// decision in one tested place.
//
// Correctness anchors (do not weaken without re-reading the LLD):
//   - Used = MaxSeverity != SeverityNone — Unknown counts as used (F3), so an
//     Unknown-only step is covered by the partial index and trips HasUnknown.
//   - RequiresReview (stored on the row) = MaxSeverity == SeverityHigh —
//     Unknown does NOT set it (kept quiet in advisory; the GATE escalates
//     Unknown in enforce, D8).
//   - SourceSetHash is SHA-256 over the FULL, pre-cap, deduped lineage source
//     set (F-cap/F2) — a safety control, never a fast hash, never the capped
//     16.
package taintlineage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Severity grades a step's untrusted-content exposure. A step's severity is
// the max across its tool calls (D1).
type Severity int

const (
	// SeverityNone — no untrusted content touched (first-party builtins only).
	SeverityNone Severity = iota
	// SeverityLow — background retrieval (memory_search): audit-only, never
	// trips requires_review (explorer Q6 — a flat boolean would flag nearly
	// every research task and become noise).
	SeverityLow
	// SeverityHigh — direct external fetch/scrape/API/recognized-remote MCP.
	SeverityHigh
	// SeverityUnknown — an unrecognized tool: neither in the first-party
	// allowlist nor a recognized-remote prefix. Counts as used (F3); the gate
	// escalates it only in enforce (D8).
	SeverityUnknown
)

// String renders the severity for metric labels / logs.
func (s Severity) String() string {
	switch s {
	case SeverityNone:
		return "none"
	case SeverityLow:
		return "low"
	case SeverityHigh:
		return "high"
	case SeverityUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

// Bounds on the stored / hashed source set.
const (
	// MaxSources caps the deduped source list for STORAGE/DISPLAY (the per-step
	// blob and the checkpoint list). It is NOT a hash constraint — the latch
	// hash is over the full pre-cap set (F-cap).
	MaxSources = 16
	// MaxRefLen truncates each source Ref. Applied BEFORE hashing so a re-run
	// changing only a beyond-MaxRefLen tail does not falsely change the hash.
	MaxRefLen = 256
)

// Source is one untrusted reference a step touched.
type Source struct {
	Tool     string   `json:"tool"`
	Ref      string   `json:"ref,omitempty"` // URL / provider·path / query text / qualified MCP name (truncated)
	Severity Severity `json:"severity"`
}

// StepTaint is the per-step classification result. Sources is bounded
// (MaxSources), deduped by (tool,ref).
type StepTaint struct {
	Used           bool     `json:"used"`            // Used = MaxSeverity != SeverityNone (F3)
	MaxSeverity    Severity `json:"max_severity"`    //
	RequiresReview bool     `json:"requires_review"` // MaxSeverity == SeverityHigh (Unknown handled at gate, D8)
	Sources        []Source `json:"sources"`         // bounded, deduped (display/storage)
	// DroppedSources is the count of distinct sources dropped by the MaxSources
	// cap, so the caller can log the drop (never silent, §4.1).
	DroppedSources int `json:"dropped_sources,omitempty"`
	// FullHash is the SHA-256 over the step's FULL, pre-cap, deduped source set
	// (M1). The lineage latch key folds these per-step hashes, so a re-run that
	// adds a source dropped by THIS step's MaxSources cap still changes the
	// latch key — closing the per-step-grain C1/F-cap hole. Empty for an
	// untainted (SeverityNone) step.
	FullHash string `json:"full_hash,omitempty"`
}

// ToolCall is the minimal shape Classify needs — the audited tool name and
// its raw input string (the same pair the degenerate-loop detector reads).
type ToolCall struct {
	Tool  string
	Input string
}

// firstPartyBuiltins is the explicit first-party (dispatcher/agent builtin)
// allowlist: local tools that never return third-party content, so they map
// to SeverityNone. Anything NOT here and NOT a recognized-remote pattern maps
// to SeverityUnknown (D2/§4.1) — the fail-closed seam the gate escalates in
// enforce. memory_search (Low), and the external-content tools web_fetch /
// web_search / web_scrape / http_get / query_api (High) are deliberately ABSENT
// — they are graded by classifyTool's rules 1a/1b/2/3 above the allowlist.
var firstPartyBuiltins = map[string]struct{}{
	"file_read":   {},
	"read_file":   {},
	"file_write":  {},
	"write_file":  {},
	"file_edit":   {},
	"edit_file":   {},
	"str_replace": {},
	"apply_patch": {},
	"run_shell":   {},
	"shell":       {},
	"bash":        {},
	"run_command": {},
	"grep":        {},
	"glob":        {},
	"ls":          {},
	"cat":         {},
	"view":        {},
	"think":       {},
	"todo_write":  {},
	"todowrite":   {},
	"list_files":  {},
	"plan":        {},
	"finish":      {},
	"task_done":   {},
}

// urlRe extracts the first http(s) URL from a tool input (best-effort ref for
// fetch/scrape tools).
var urlRe = regexp.MustCompile(`https?://[^\s"'<>)\]}]+`)

// Classify computes a StepTaint from a step's tool-audit calls. Pure; no I/O.
func Classify(calls []ToolCall) StepTaint {
	st := StepTaint{MaxSeverity: SeverityNone}
	// dedup by (tool,ref); preserve first-seen order for stable display.
	seen := make(map[string]struct{}, len(calls))
	full := make([]Source, 0, len(calls))
	for _, c := range calls {
		sev, ref := classifyTool(c.Tool, c.Input)
		if sev > st.MaxSeverity {
			st.MaxSeverity = sev
		}
		if sev == SeverityNone {
			continue // first-party builtins contribute no source entry
		}
		ref = truncateRef(ref)
		key := c.Tool + "\x00" + ref
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		full = append(full, Source{Tool: c.Tool, Ref: ref, Severity: sev})
	}
	st.Used = st.MaxSeverity != SeverityNone
	st.RequiresReview = st.MaxSeverity == SeverityHigh
	// FullHash is over the FULL pre-cap deduped set — computed BEFORE the
	// MaxSources cap so the latch key reflects sources this step dropped (M1).
	st.FullHash = HashSources(full)
	if len(full) > MaxSources {
		st.DroppedSources = len(full) - MaxSources
		st.Sources = append([]Source(nil), full[:MaxSources]...)
	} else {
		st.Sources = full
	}
	return st
}

// classifyTool maps a single tool name + input to a severity and a reference
// string. First match wins; the classifier is TOTAL (every input maps to a
// severity). See §4.1.
func classifyTool(tool, input string) (Severity, string) {
	name := strings.TrimSpace(tool)
	lower := strings.ToLower(name)

	// 1a. Direct external fetch / scrape / browse (URL ref). web_scrape and
	// http_get are real swarm tools (blackbox side-effects, narrator, and the
	// hallucination rules all recognize them) — they return third-party content
	// by definition and must be High, not Unknown.
	if lower == "web_fetch" || lower == "fetch" || lower == "web_scrape" || lower == "http_get" ||
		strings.HasPrefix(lower, "mcp__scraper__") ||
		(strings.HasPrefix(lower, "mcp__") && (strings.Contains(lower, "fetch") || strings.Contains(lower, "browse"))) {
		if u := urlRe.FindString(input); u != "" {
			return SeverityHigh, u
		}
		return SeverityHigh, name
	}

	// 1b. web_search — external search (returns third-party snippets). High with
	// the search query as the ref (URL-free, so it's the search-text analogue of
	// the fetch URL). In advisory (the default ship mode) Unknown does NOT set
	// requires_review, so the most common external tool would otherwise be
	// invisible to the High calibration signal (review I1).
	if lower == "web_search" {
		if q := searchQueryRef(input); q != "" {
			return SeverityHigh, q
		}
		return SeverityHigh, name
	}

	// 2. query_api write/read to a third-party provider.
	if lower == "query_api" {
		if ref := queryAPIRef(input); ref != "" {
			return SeverityHigh, ref
		}
		return SeverityHigh, name // malformed/non-JSON input → tool-name fallback
	}

	// 3. memory_search — background retrieval (Low). Must precede the
	// allowlist/unknown fall-through; it is NOT an mcp__ tool so rule 5 is safe.
	if lower == "memory_search" {
		if q := memorySearchRef(input); q != "" {
			return SeverityLow, q
		}
		return SeverityLow, name
	}

	// 4. Explicit first-party builtins → None.
	if _, ok := firstPartyBuiltins[lower]; ok {
		return SeverityNone, ""
	}

	// 5. Any other external MCP tool → High, qualified name as ref.
	if strings.HasPrefix(lower, "mcp__") {
		return SeverityHigh, name
	}

	// 6. Anything else (unrecognized) → Unknown.
	return SeverityUnknown, name
}

// queryAPIRef pulls a "provider·path" ref from a query_api input JSON.
// Returns "" on malformed/non-JSON input so the caller falls back to the tool
// name.
func queryAPIRef(input string) string {
	var m struct {
		Provider string `json:"provider"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		return ""
	}
	prov := strings.TrimSpace(m.Provider)
	path := strings.TrimSpace(m.Path)
	switch {
	case prov != "" && path != "":
		return prov + "·" + path
	case prov != "":
		return prov
	case path != "":
		return path
	default:
		return ""
	}
}

// memorySearchRef pulls the search query text from a memory_search input JSON
// (I3 — gives the laundered-via-memory step some audit attribution). Returns
// "" when no query text is present.
func memorySearchRef(input string) string {
	return searchQueryRef(input)
}

// searchQueryRef extracts a search query from a tool input: the "query" or "q"
// JSON field, else a raw non-JSON input treated as the query text. Returns ""
// when nothing usable is present (caller falls back to the tool name).
func searchQueryRef(input string) string {
	var m struct {
		Query string `json:"query"`
		Q     string `json:"q"`
	}
	if err := json.Unmarshal([]byte(input), &m); err == nil {
		if q := strings.TrimSpace(m.Query); q != "" {
			return q
		}
		if q := strings.TrimSpace(m.Q); q != "" {
			return q
		}
	}
	if trimmed := strings.TrimSpace(input); trimmed != "" && !strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	return ""
}

func truncateRef(ref string) string {
	if len(ref) > MaxRefLen {
		return ref[:MaxRefLen]
	}
	return ref
}

// SourcesBlob is the persisted untrusted_sources JSONB shape (M1). It carries
// the capped display list PLUS a full_hash over the step's FULL pre-cap deduped
// source set and the drop count, so the lineage latch key can reflect sources
// dropped by the per-step MaxSources cap. Serialized by NewSourcesBlob and read
// back by StepTaintFromBlob, which also accepts the legacy bare-[]Source shape.
type SourcesBlob struct {
	FullHash string   `json:"full_hash,omitempty"`
	Dropped  int      `json:"dropped,omitempty"`
	Sources  []Source `json:"sources"`
}

// NewSourcesBlob builds the persisted blob for a classified step (M1). The
// executor marshals this into the untrusted_sources JSONB column.
func NewSourcesBlob(st StepTaint) SourcesBlob {
	return SourcesBlob{FullHash: st.FullHash, Dropped: st.DroppedSources, Sources: st.Sources}
}

// StepTaintFromBlob reconstructs the rollup-relevant StepTaint from a persisted
// row's untrusted_sources JSONB blob plus its stored requires_review flag. Used
// by the write gate to turn TaintedStepsForTasks rows back into rollup inputs
// without the persistence layer knowing the classifier types (D2). A row
// returned by the taint partial index is always "used"; its MaxSeverity is the
// max across its recorded sources — so an Unknown-only row (requires_review=
// false, a SeverityUnknown source) reconstructs MaxSeverity=Unknown and
// correctly trips HasUnknown (F3).
//
// It accepts BOTH the current SourcesBlob object shape (with full_hash) and the
// legacy bare-[]Source array; when no full_hash is present it computes one over
// the available (capped) sources as a fallback, so the latch remains
// deterministic (M1).
func StepTaintFromBlob(blob []byte, requiresReview bool) StepTaint {
	st := StepTaint{Used: true, RequiresReview: requiresReview, MaxSeverity: SeverityNone}
	sources, fullHash, dropped := decodeSourcesBlob(blob)
	st.Sources = sources
	st.DroppedSources = dropped
	for _, s := range sources {
		if s.Severity > st.MaxSeverity {
			st.MaxSeverity = s.Severity
		}
	}
	if fullHash != "" {
		st.FullHash = fullHash
	} else {
		st.FullHash = HashSources(sources) // legacy/no-header fallback (M1)
	}
	// Defensive: a stored requires_review implies at least High.
	if requiresReview && st.MaxSeverity < SeverityHigh {
		st.MaxSeverity = SeverityHigh
	}
	return st
}

// decodeSourcesBlob parses either the SourcesBlob object shape or the legacy
// bare-[]Source array. Returns (sources, fullHash, dropped).
func decodeSourcesBlob(blob []byte) ([]Source, string, int) {
	if len(blob) == 0 {
		return nil, "", 0
	}
	var obj SourcesBlob
	if err := json.Unmarshal(blob, &obj); err == nil && (obj.FullHash != "" || obj.Sources != nil) {
		return obj.Sources, obj.FullHash, obj.Dropped
	}
	var arr []Source
	if err := json.Unmarshal(blob, &arr); err == nil {
		return arr, "", 0
	}
	return nil, "", 0
}

// TaskTaint is the lineage rollup a write gate consults (§4.1).
type TaskTaint struct {
	Tainted        bool
	RequiresReview bool     // any lineage step SeverityHigh
	HasUnknown     bool     // any lineage step SeverityUnknown (gate escalates in enforce, D8)
	WalkComplete   bool     // false → enforce fail-closed (D6)
	Sources        []Source // lineage union, re-bounded to MaxSources — DISPLAY only
	TotalSources   int      // FULL pre-cap distinct source count (surfaced when > len(Sources), F-cap)
	SourceSetHash  string   // SHA-256 over the FULL pre-cap distinct source set (D7 latch key)
}

// Rollup unions the writing task's own step taints with its walked ancestors'
// and produces the task-lineage rollup. Sources is re-bounded to MaxSources for
// display; TotalSources + SourceSetHash are computed over the FULL pre-cap
// deduped set (F-cap).
func Rollup(ownSteps, ancestorSteps []StepTaint, walkComplete bool) TaskTaint {
	tt := TaskTaint{WalkComplete: walkComplete}
	seen := make(map[string]struct{})
	full := make([]Source, 0)
	droppedSum := 0
	// stepHashes collects each used step's FULL pre-cap source-set hash (M1).
	// The lineage latch key folds these, not the (per-step-capped) display
	// union, so a source dropped by a single step's MaxSources cap still moves
	// the key.
	stepSeen := make(map[string]struct{})
	stepHashes := make([]string, 0)
	fold := func(steps []StepTaint) {
		for _, s := range steps {
			if s.Used {
				tt.Tainted = true
			}
			switch s.MaxSeverity {
			case SeverityHigh:
				tt.RequiresReview = true
			case SeverityUnknown:
				tt.HasUnknown = true
			}
			droppedSum += s.DroppedSources
			h := s.FullHash
			if h == "" {
				h = HashSources(s.Sources) // fallback for directly-built StepTaints
			}
			if h != "" {
				if _, dup := stepSeen[h]; !dup {
					stepSeen[h] = struct{}{}
					stepHashes = append(stepHashes, h)
				}
			}
			for _, src := range s.Sources {
				ref := truncateRef(src.Ref)
				key := src.Tool + "\x00" + ref
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				full = append(full, Source{Tool: src.Tool, Ref: ref, Severity: src.Severity})
			}
		}
	}
	fold(ownSteps)
	fold(ancestorSteps)

	// TotalSources reflects the truncation surfaced to the operator: the
	// distinct display union PLUS sources dropped by per-step caps, so
	// source_count > shown_count whenever ANY source was hidden (F-cap display).
	tt.TotalSources = len(full) + droppedSum
	// SourceSetHash (D7 latch key) folds the per-step FULL-set hashes (M1),
	// order-independent + collision-resistant.
	tt.SourceSetHash = hashStepHashes(stepHashes)
	if len(full) > MaxSources {
		display := sortSources(full)
		tt.Sources = display[:MaxSources]
	} else {
		tt.Sources = full
	}
	return tt
}

// HashSources computes the SHA-256 latch key over a source set (F2/F-cap). The
// encoding is canonical + order-independent:
//   - each source's Ref is truncated to MaxRefLen BEFORE hashing (so a re-run
//     that changes only a beyond-MaxRefLen tail does not change the hash);
//   - entries are sorted by (Tool, Ref);
//   - each field is length-prefixed so ("ab","c") and ("a","bc") never collide.
//
// The hash is over the FULL pre-cap deduped set the caller passes — never the
// MaxSources-capped display list — so an overflow source still re-parks (F-cap).
// SHA-256 (not a fast hash) because a collision would auto-approve a genuinely
// different source set (F2).
func HashSources(sources []Source) string {
	if len(sources) == 0 {
		return ""
	}
	// Dedup + truncate defensively, then sort canonically.
	seen := make(map[string]Source, len(sources))
	for _, s := range sources {
		ref := truncateRef(s.Ref)
		key := s.Tool + "\x00" + ref
		if _, dup := seen[key]; !dup {
			seen[key] = Source{Tool: s.Tool, Ref: ref, Severity: s.Severity}
		}
	}
	canon := make([]Source, 0, len(seen))
	for _, s := range seen {
		canon = append(canon, s)
	}
	sortSourcesInPlace(canon)
	h := sha256.New()
	for _, s := range canon {
		writeLenPrefixed(h, s.Tool)
		writeLenPrefixed(h, s.Ref)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashStepHashes folds a set of per-step FULL-set hashes into the lineage latch
// key (M1): deduped, sorted, length-prefixed, SHA-256. Order-independent, so
// the lineage key is stable regardless of step order and changes whenever ANY
// step's full source set changes. Empty input → "".
func hashStepHashes(hashes []string) string {
	if len(hashes) == 0 {
		return ""
	}
	uniq := make(map[string]struct{}, len(hashes))
	canon := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if _, dup := uniq[h]; dup {
			continue
		}
		uniq[h] = struct{}{}
		canon = append(canon, h)
	}
	sort.Strings(canon)
	hh := sha256.New()
	for _, h := range canon {
		writeLenPrefixed(hh, h)
	}
	return hex.EncodeToString(hh.Sum(nil))
}

type hashWriter interface{ Write([]byte) (int, error) }

func writeLenPrefixed(h hashWriter, s string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(s))))
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(s))
}

func sortSources(in []Source) []Source {
	out := append([]Source(nil), in...)
	sortSourcesInPlace(out)
	return out
}

func sortSourcesInPlace(s []Source) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Tool != s[j].Tool {
			return s[i].Tool < s[j].Tool
		}
		return s[i].Ref < s[j].Ref
	})
}
