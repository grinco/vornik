package datasubject

import (
	"fmt"
	"sort"
	"strings"
)

// Item is one piece of data held about a subject, as collected from a linked
// row.
type Item struct {
	Table      LinkableTable
	RowID      string
	ProjectID  string
	Source     Source
	Confidence Confidence
	// Exclusivity decides whether Content may be emitted verbatim.
	Exclusivity Exclusivity
	// Content is the row's personal data as collected. For a shared row this is
	// NOT emitted; see Export.
	Content string
	// Context is a non-disclosing description (date, kind, origin) that can be
	// emitted even for a shared row, so the subject learns the item exists.
	Context string
	// Origin answers Art 14(2)(f) — where the data came from.
	Origin string
	// ProvidedBySubject marks data the subject themselves provided under
	// consent or contract, which is the only data Art 20 portability covers.
	ProvidedBySubject bool
}

// ExportItem is one entry in the produced report.
type ExportItem struct {
	Table      string `json:"table"`
	RowID      string `json:"row_id"`
	ProjectID  string `json:"project_id,omitempty"`
	Source     string `json:"source"`
	Confidence string `json:"confidence"`
	Origin     string `json:"origin,omitempty"`
	// Content is present only where it could be disclosed without affecting
	// another person's rights.
	Content string `json:"content,omitempty"`
	// Withheld is set when Content was suppressed, with the reason. A subject
	// is told the item EXISTS even when its content cannot be handed over.
	Withheld string `json:"withheld,omitempty"`
	Context  string `json:"context,omitempty"`
	InArt20  bool   `json:"portable"`
}

// Export is the Art 15 / Art 20 report.
type Export struct {
	SubjectID   string       `json:"subject_id"`
	RequestID   string       `json:"request_id"`
	Kind        string       `json:"kind"`
	Identifiers []Identifier `json:"-"`
	Items       []ExportItem `json:"items"`
	// MethodsRun names the binders that contributed, so the subject learns HOW
	// the search was performed rather than being handed a bare result.
	MethodsRun []string `json:"identification_methods"`
	// Limitations is the honest statement of what the search does not cover.
	Limitations []string `json:"limitations"`
	// RetainedCategories names data NOT produced or erased, with its ground.
	RetainedCategories map[string]string `json:"retained_categories,omitempty"`
}

// ErrNotVerified is returned when an export is attempted before the identity
// gate has been cleared.
var ErrNotVerified = fmt.Errorf("datasubject: request identity is not verified — " +
	"producing data for an unverified requester would itself be a personal-data breach (Art 12(6))")

// BuildExport assembles the report for a verified request.
//
// TWO PROPERTIES CARRY THIS FUNCTION, and both are the difference between a
// compliance feature and a breach:
//
//  1. The identity gate. An unverified request produces nothing at all. Handing
//     an Art 15 export to whoever asked is a disclosure of the subject's data to
//     a stranger, which makes the request mechanism an attack surface.
//
//  2. Art 15(4). A row that also concerns other people is NOT emitted verbatim.
//     The subject is told the item exists, with its date and origin, and the
//     content is withheld. Emitting a memory chunk naming three people because
//     one of them asked would trade one Art 15 obligation for two breaches.
//     `unknown` exclusivity is treated as shared, because guessing wrong in the
//     other direction discloses.
//
// Art 20 (portability) narrows the same collection to data the subject provided,
// because "export everything" would erase a distinction the article makes.
func BuildExport(req Request, items []Item, methodsRun []string) (*Export, error) {
	if !req.MayProduceData() {
		return nil, ErrNotVerified
	}
	if req.Kind != RequestAccess && req.Kind != RequestPortability {
		return nil, fmt.Errorf("datasubject: BuildExport handles access and portability, not %q", req.Kind)
	}
	portabilityOnly := req.Kind == RequestPortability

	out := &Export{
		SubjectID:  req.SubjectID,
		RequestID:  req.ID,
		Kind:       string(req.Kind),
		MethodsRun: dedupeSorted(methodsRun),
		RetainedCategories: map[string]string{
			"tool_audit_log":         UncoveredTable["tool_audit_log"],
			"admin_audit":            UncoveredTable["admin_audit"],
			"channel_disclosure_log": UncoveredTable["channel_disclosure_log"],
		},
	}

	for _, it := range items {
		if portabilityOnly && !it.ProvidedBySubject {
			continue
		}
		ei := ExportItem{
			Table: string(it.Table), RowID: it.RowID, ProjectID: it.ProjectID,
			Source: string(it.Source), Confidence: string(it.Confidence),
			Origin: it.Origin, Context: it.Context, InArt20: it.ProvidedBySubject,
		}
		if it.Exclusivity.TreatAsShared() {
			ei.Withheld = "content withheld under Art 15(4): this record also concerns other people, " +
				"and disclosing it whole would adversely affect their rights"
			if ei.Context == "" {
				// Never emit an item with neither content nor context — the
				// subject would learn nothing at all from the entry.
				ei.Context = fmt.Sprintf("a %s record", it.Table)
			}
		} else {
			ei.Content = it.Content
		}
		out.Items = append(out.Items, ei)
	}

	out.Limitations = limitationsFor(out.MethodsRun)
	return out, nil
}

// limitationsFor states what the search did NOT cover.
//
// This is in the artefact rather than a runbook because the data subject is the
// person entitled to know it. An export that reads as exhaustive when it is
// best-effort misleads precisely the person the right exists to protect.
func limitationsFor(methods []string) []string {
	lims := []string{
		"This report covers everything the deployment could IDENTIFY as being about you. " +
			"It is not a guarantee that no further data exists.",
		"Where you are named in free text, identification relies on automated entity " +
			"extraction, which misses references by pronoun, nickname, or indirect description.",
	}
	if !containsMethod(methods, string(SourceKGExtraction)) {
		lims = append(lims, "Free-text mention search did NOT run for this report, so records that "+
			"name you only in prose were not searched.")
	}
	lims = append(lims, "Records that also concern other people are listed but their content is "+
		"withheld, under Art 15(4).")
	return lims
}

func containsMethod(methods []string, want string) bool {
	for _, m := range methods {
		if m == want {
			return true
		}
	}
	return false
}

// LeaksForeignContent reports whether any emitted item carries content from a
// row that is not exclusively about this subject.
//
// A self-check, meant to be asserted in tests and callable before a report is
// handed over: the Art 15(4) failure is silent, produces a plausible-looking
// export, and turns a compliance feature into a disclosure. Cheap to verify, so
// there is no reason to rely on the loop above being right by inspection.
func (e *Export) LeaksForeignContent(items []Item) bool {
	shared := map[string]bool{}
	for _, it := range items {
		if it.Exclusivity.TreatAsShared() {
			shared[string(it.Table)+"\x00"+it.RowID] = true
		}
	}
	for _, ei := range e.Items {
		if ei.Content != "" && shared[ei.Table+"\x00"+ei.RowID] {
			return true
		}
	}
	return false
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
