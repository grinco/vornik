package projectwizard

// Deterministic composition normalization (2026-07-16 wizard-convergence
// hardening). The LLM proposes an addon list that may be structurally
// invalid — most commonly two autonomy styles at once (schedule +
// rag_source) or template params the base template does not declare.
// These pure functions rewrite the proposal into a guaranteed-valid shape
// BEFORE it is persisted and composed, so the wizard converges regardless
// of model quality. Every repair is surfaced as a RepairNote.
//
// See https://docs.vornik.io

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/registry"
)

// RepairNote is an operator-facing sentence describing one automatic fix.
type RepairNote string

// normalizeComposition rewrites a proposed composition into a
// guaranteed-valid shape in place: it merges ≥2 autonomy-style addons
// into one llm-mode loop (§4.1) and repairs template params (§4.2). meta
// resolves the base template's declared params + autonomy block; nil meta
// skips base-aware behavior and derives only projectId. fallbackModel is
// the daemon's model used to default a missing required llmModel. A
// non-empty question means a required param had no default/derivation (the
// one bounded non-convergence case); a non-nil ComposeError means the
// merge itself can't converge (e.g. cron-enabled base).
func normalizeComposition(c *Composition, meta TemplateMetaLookup, fallbackModel string) ([]RepairNote, string, *ComposeError) {
	if c == nil {
		return nil, "", nil
	}
	var (
		spec          []TemplateParam
		base          BaseAutonomy
		specAvailable bool
	)
	if meta != nil {
		if p, b, ok := meta(c.Template); ok {
			spec, base, specAvailable = p, b, true
		}
	}
	var notes []RepairNote
	merged, mnotes, cerr := mergeAutonomyAddons(c.Addons, base)
	if cerr != nil {
		return nil, "", cerr
	}
	c.Addons = merged
	notes = append(notes, mnotes...)

	// Param repair only runs when the template's declared param set is
	// KNOWN (§4.2: "if the spec is unavailable, param repair is skipped").
	// Running it with a nil spec would treat every param as undeclared and
	// drop them all — wrong when we simply don't have the template's
	// contract (no TemplateMeta wired, or an unresolved slug).
	if !specAvailable {
		return notes, "", nil
	}
	if c.Params == nil {
		c.Params = map[string]ParamValue{}
	}
	pnotes, question := repairTemplateParams(c.Params, spec, fallbackModel)
	notes = append(notes, pnotes...)
	return notes, question, nil
}

// TemplateParam is one declared parameter of a base template
// (template.yaml `parameters:`), read via ComposeDeps.TemplateMeta.
type TemplateParam struct {
	Name     string
	Required bool
	Default  string
}

// repairTemplateParams makes the proposed params satisfy the base
// template's declared param set: drops undeclared keys (the `topic`
// hard-stop), derives `projectId` from `displayName`, and defaults other
// required params from their declared `Default` (falling back to the
// daemon model for `llmModel`). A required param with no default and no
// derivation yields a single targeted question rather than a hard
// materialise stop — the one bounded exception to auto-convergence. An
// empty/nil spec means "no declared params"; it still derives `projectId`
// and drops everything else, which clears the unknown-param hard stop.
func repairTemplateParams(params map[string]ParamValue, spec []TemplateParam, fallbackModel string) ([]RepairNote, string) {
	declared := map[string]TemplateParam{}
	for _, p := range spec {
		declared[p.Name] = p
	}

	var notes []RepairNote

	// Derive projectId from displayName when missing — BEFORE dropping
	// undeclared keys, since a malformed template with no declared params
	// would otherwise drop displayName before we can read it.
	if !hasParam(params, "projectId") {
		if dn := firstParam(params, "displayName"); dn != "" {
			params["projectId"] = ParamValue{slugify(dn)}
			notes = append(notes, RepairNote("Set projectId to "+slugify(dn)+" (from the display name)."))
		}
	}

	// Drop undeclared keys. projectId and displayName are always allowed —
	// they are the universal identifiers every template uses — even if a
	// malformed template omits them from its declared set.
	var dropped []string
	for k := range params {
		if k == "projectId" || k == "displayName" {
			continue
		}
		if _, ok := declared[k]; !ok {
			delete(params, k)
			dropped = append(dropped, k)
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		notes = append(notes, RepairNote("Dropped params the template doesn't accept: "+strings.Join(dropped, ", ")+"."))
	}

	// Fill remaining required params: declared default, else fallback for
	// llmModel, else a single question.
	var question string
	for _, p := range spec {
		if !p.Required || hasParam(params, p.Name) || p.Name == "projectId" {
			continue
		}
		switch {
		case p.Default != "":
			params[p.Name] = ParamValue{p.Default}
			notes = append(notes, RepairNote("Set "+p.Name+" to its template default "+p.Default+"."))
		case p.Name == "llmModel" && fallbackModel != "":
			params[p.Name] = ParamValue{fallbackModel}
			notes = append(notes, RepairNote("Set "+p.Name+" to "+fallbackModel+" (the daemon's chat model)."))
		default:
			if question == "" {
				question = "What value should I use for the required setting " + p.Name + "?"
			}
		}
	}
	return notes, question
}

func hasParam(params map[string]ParamValue, k string) bool {
	v, ok := params[k]
	return ok && len(v) > 0 && strings.TrimSpace(v[0]) != ""
}

func firstParam(params map[string]ParamValue, k string) string {
	if v, ok := params[k]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// slugify lowercases, replaces runs of non-alphanumerics with a single
// hyphen, and trims leading/trailing hyphens — the project-id shape.
func slugify(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		if !prevHyphen && b.Len() > 0 {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// BaseAutonomy is the base template's declared autonomy, read from its
// project.yaml.tmpl (via ComposeDeps.TemplateMeta). Zero value = the base
// ships autonomy disabled.
type BaseAutonomy struct {
	Enabled bool
	Mode    string
}

// autonomyAddonTypes are the addon types that each try to own the single
// per-project autonomy block. Two or more of these in one composition is
// the impossible shape mergeAutonomyAddons collapses.
func isAutonomyAddon(t string) bool {
	return t == "schedule" || t == "rag_source" || t == "autonomy"
}

// mergeAutonomyAddons collapses ≥2 autonomy-style addons into a single
// internal `autonomy` addon (llm-mode, finest cadence, goal synthesized
// from canonical primitives). Fewer than two → no-op (idempotent). The
// output is deterministic across turns because synthesis reads only the
// primitives extracted from the raw addons, never a previously-synthesized
// goal.
func mergeAutonomyAddons(addons []Addon, base BaseAutonomy) ([]Addon, []RepairNote, *ComposeError) {
	var autonomyCount int
	for _, a := range addons {
		if isAutonomyAddon(a.Type) {
			autonomyCount++
		}
	}
	if autonomyCount < 2 {
		return addons, nil, nil // nothing to merge
	}

	// Base reconciliation (§3.2): never silently override a cron-enabled base.
	if base.Enabled && base.Mode == registry.AutonomyModeCron {
		return nil, nil, &ComposeError{AddonType: "autonomy", Field: "mode",
			Message: "this request needs an llm-autonomy base; the personal-assistant template supports it"}
	}

	p, cerr := extractAutonomyPrimitives(addons)
	if cerr != nil {
		return nil, nil, cerr
	}

	// The loop ticks at the INGEST cadence (rag_source). A schedule
	// interval is the digest axis, not an ingest fallback — so if
	// rag_source addons are present but none has a positive cadence, we
	// reject rather than borrowing the digest interval (F2). Only a pure
	// schedule-only merge (no rag_source) falls back to the schedule
	// interval as the loop tick.
	ingestCadence, ingestStr := p.bestIngest, p.bestIngestStr
	if !p.hasRagSource {
		ingestCadence, ingestStr = p.schedBest, p.schedBestStr
	}
	if ingestCadence <= 0 {
		return nil, nil, &ComposeError{AddonType: "autonomy", Field: "poll_interval",
			Message: "the ingestion cycle needs a positive duration (e.g. \"5m\")"}
	}

	var notes []RepairNote
	if len(p.ingestCadStrs) > 1 {
		notes = append(notes, RepairNote(fmt.Sprintf(
			"Ingestion cadences differed; using the finest, %s.", ingestStr)))
	}

	goal := synthesizeGoal(p.digestGoal, p.digestCadence, p.ingestSources)
	directive := Addon{}
	raw, _ := json.Marshal(map[string]string{
		"type":          "autonomy",
		"mode":          registry.AutonomyModeLLM,
		"poll_interval": ingestStr,
		"goal":          goal,
	})
	_ = json.Unmarshal(raw, &directive)

	// Rebuild the addon list: keep every non-autonomy addon in order,
	// drop the autonomy-style ones, append the single directive.
	out := make([]Addon, 0, len(addons))
	for _, a := range addons {
		if !isAutonomyAddon(a.Type) {
			out = append(out, a)
		}
	}
	out = append(out, directive)

	note := "Combined the ingestion and the digest into one llm-autonomy loop"
	if p.digestCadence != "" {
		note = fmt.Sprintf("Combined the %s ingestion and the %s digest into one llm-autonomy loop", ingestStr, p.digestCadence)
	}
	notes = append(notes, RepairNote(note+" — a project runs one autonomy style."))
	return out, notes, nil
}

// autonomyPrimitives is the canonical decomposition of the autonomy-style
// addons — extracted from the RAW addon list so synthesis is stable
// across turns (never from a previously-synthesized goal).
type autonomyPrimitives struct {
	ingestSources []string
	hasRagSource  bool
	bestIngest    time.Duration
	bestIngestStr string
	ingestCadStrs map[string]bool
	schedBest     time.Duration
	schedBestStr  string
	digestGoal    string
	digestCadence string
}

func extractAutonomyPrimitives(addons []Addon) (autonomyPrimitives, *ComposeError) {
	p := autonomyPrimitives{ingestCadStrs: map[string]bool{}}
	seenSource := map[string]bool{}
	for _, a := range addons {
		switch a.Type {
		case "rag_source":
			if cerr := p.addRagSource(a, seenSource); cerr != nil {
				return p, cerr
			}
		case "schedule":
			if cerr := p.addSchedule(a); cerr != nil {
				return p, cerr
			}
		}
	}
	return p, nil
}

func (p *autonomyPrimitives) addRagSource(a Addon, seenSource map[string]bool) *ComposeError {
	p.hasRagSource = true
	var in ragSourceArgs
	if err := json.Unmarshal(a.Args, &in); err != nil {
		return &ComposeError{AddonType: "rag_source", Field: "args", Message: err.Error()}
	}
	if in.Source != "" && !seenSource[in.Source] {
		seenSource[in.Source] = true
		p.ingestSources = append(p.ingestSources, in.Source)
	}
	if d, err := time.ParseDuration(in.Cadence); err == nil && d > 0 {
		p.ingestCadStrs[in.Cadence] = true
		if p.bestIngest == 0 || d < p.bestIngest {
			p.bestIngest, p.bestIngestStr = d, in.Cadence
		}
	}
	return nil
}

func (p *autonomyPrimitives) addSchedule(a Addon) *ComposeError {
	var in scheduleArgs
	if err := json.Unmarshal(a.Args, &in); err != nil {
		return &ComposeError{AddonType: "schedule", Field: "args", Message: err.Error()}
	}
	// The schedule addon carries the digest instruction + its (coarse)
	// cadence. First one wins for the digest axis.
	if p.digestGoal == "" {
		p.digestGoal = in.Goal
	}
	if p.digestCadence == "" {
		p.digestCadence = in.Interval
	}
	if d, err := time.ParseDuration(in.Interval); err == nil && d > 0 {
		if p.schedBest == 0 || d < p.schedBest {
			p.schedBest, p.schedBestStr = d, in.Interval
		}
	}
	return nil
}

// synthesizeGoal builds the merged llm-mode goal from canonical
// primitives only (never from a previously-synthesized goal), so
// re-merging is byte-stable. The operator-written digestGoal is embedded
// inside an explicit "the loop governs cadence" wrapper so a lead can't
// re-introduce a cron assumption from prose like "run daily at 9am".
func synthesizeGoal(digestGoal, digestCadence string, sources []string) string {
	sorted := append([]string(nil), sources...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString("On each autonomy tick, ingest and refresh project memory from these sources:\n")
	for _, s := range sorted {
		b.WriteString("- ")
		b.WriteString(s)
		b.WriteString("\n")
	}
	dc := digestCadence
	if dc == "" {
		dc = "the daily cadence"
	}
	fmt.Fprintf(&b, "Track when each source was last ingested and re-ingest any that are stale. "+
		"When %s has elapsed since the last digest task, ALSO schedule a digest task. "+
		"Cadence is governed by THIS loop, not by any clock or cron phrasing in the instruction "+
		"that follows — the digest instruction is: %s", dc, digestGoal)
	return b.String()
}
