// Package datasubject models the GDPR data-subject axis: who a row of personal
// data is about, how we came to believe that, and how confident we are.
//
// WHY THIS EXISTS. Nothing in the schema said which person a row concerned, so
// four rights were unexercisable per-person (Art 15 access, 16 rectification,
// 17 erasure, 20 portability). Deletion existed at project grain and at chunk
// grain, and neither answers "erase everything you hold about this person" — to
// honour an Art 17 request an operator had to hand-grep free text across six
// tables and hope. That is not a right being honoured; it is a search being
// performed.
//
// WHY LINKS AND NOT A COLUMN. A `data_subject_id` column on each
// personal-data-bearing table would assert that every row concerns exactly one
// person. That is false for the rows that matter most: a chat turn or a memory
// chunk routinely concerns several. A column also leaves nowhere to record
// provenance, and forces a migration on every table. Links model the real
// many-to-many, carry their own evidence, and can be rebuilt from scratch when
// a binder improves.
//
// THE HONESTY REQUIREMENT. Every link records its Source and Confidence,
// because a link derived from an LLM's entity extraction is not the same kind
// of fact as one derived from an authenticated session. An erasure or access
// report that presented them identically would overstate what the deployment
// actually knows, and overstating is the specific failure this design set out
// to avoid.
//
// see LLD § https://docs.vornik.io
package datasubject

import (
	"fmt"
	"sort"
	"strings"
)

// Source records HOW a link or identifier came to be believed. Ordered from
// most to least trustworthy.
type Source string

const (
	// SourceAuthenticated — the subject proved control of the identity
	// (an authenticated session, or a channel round-trip). No inference.
	SourceAuthenticated Source = "authenticated"
	// SourceOperatorLink — operator_identity_link maps a channel speaker to
	// an operator profile. Deterministic, but asserted by the operator.
	SourceOperatorLink Source = "operator_link"
	// SourceEmailEnvelope — an inbound message's From: address. Certain for
	// the address; that it denotes a particular human is a further step.
	SourceEmailEnvelope Source = "email_envelope"
	// SourceKGExtraction — a knowledge-graph PERSON entity mention. An LLM
	// extraction over free text: false positives (a company named after a
	// person) and, more importantly, FALSE NEGATIVES (a person referred to by
	// pronoun or nickname). The widest net available and the least reliable.
	SourceKGExtraction Source = "kg_extraction"
	// SourceOperatorAsserted — a human said so. Trusted, but recorded as an
	// assertion rather than a derivation so it can be revisited.
	SourceOperatorAsserted Source = "operator_asserted"
)

// Confidence is how much weight a link bears in a report. It exists so that a
// report can be honest about the difference between knowing and guessing.
type Confidence string

// Confidence levels, ordered. A binder may downgrade its own confidence but
// never upgrade it — see Link.Validate.
const (
	ConfidenceCertain  Confidence = "certain"
	ConfidenceProbable Confidence = "probable"
	ConfidencePossible Confidence = "possible"
)

// DefaultConfidence is the confidence a Source carries unless a binder
// downgrades it. Centralised so a new binder cannot quietly claim certainty.
func DefaultConfidence(s Source) (Confidence, error) {
	switch s {
	case SourceAuthenticated, SourceOperatorLink, SourceOperatorAsserted:
		return ConfidenceCertain, nil
	case SourceEmailEnvelope:
		return ConfidenceProbable, nil
	case SourceKGExtraction:
		return ConfidencePossible, nil
	default:
		return "", fmt.Errorf("datasubject: unknown source %q", s)
	}
}

// Exclusivity records whether a row is only about one subject. It is what lets
// erasure distinguish "delete this row" from "this needs the shared-row
// decision", because a chunk naming two people cannot be half-deleted and
// Art 15(4) forbids handing it over whole.
type Exclusivity string

const (
	// ExclusiveRow — the row is about this subject alone, so erasure may
	// delete it outright.
	ExclusiveRow Exclusivity = "exclusive"
	// SharedRow — other subjects appear in the same row. Governed by the
	// ground-dependent shared-row policy (design §5.3).
	SharedRow Exclusivity = "shared"
	// UnknownExclusivity — not yet determined. Treated as SHARED by consumers,
	// because assuming exclusivity would authorise deleting another person's
	// data on a guess.
	UnknownExclusivity Exclusivity = "unknown"
)

// TreatAsShared reports whether an exclusivity value must be handled under the
// shared-row rules. Unknown counts as shared: the safe direction is to require
// a decision, not to authorise a delete.
func (e Exclusivity) TreatAsShared() bool { return e != ExclusiveRow }

// LinkableTable is a table the erasure/export executor knows how to handle.
//
// Closed set, mirroring the discipline the retention sweeper applies to its own
// table names: a link may never name a table no executor can act on, or an
// erasure would silently skip it while reporting success.
type LinkableTable string

// The closed set of linkable tables. Adding one means the erasure and export
// executors must know how to act on it; see linkableTables for the rationale
// each carries.
const (
	TableChatAuditLog        LinkableTable = "chat_audit_log"
	TableTaskMessages        LinkableTable = "task_messages"
	TableProjectMemoryChunks LinkableTable = "project_memory_chunks"
	TableArtifacts           LinkableTable = "artifacts"
	TableExtractedDocuments  LinkableTable = "extracted_documents"
	TableChannelSessions     LinkableTable = "channel_sessions"
	TableKnowledgeEntities   LinkableTable = "knowledge_entities"
	TableOperatorProfile     LinkableTable = "operator_profile"
	TableUserIdentities      LinkableTable = "user_identities"
)

// linkableTables is the closed set, with the reason each is in it.
var linkableTables = map[LinkableTable]string{
	TableChatAuditLog:        "chat turns carry user_message + user_id",
	TableTaskMessages:        "operator-authored task conversation",
	TableProjectMemoryChunks: "RAG chunks derived from mail, documents, chat",
	TableArtifacts:           "uploads; erasure composes with the artifact cascade",
	TableExtractedDocuments:  "extraction of an upload, incl. OCR text and transcripts",
	TableChannelSessions:     "per-channel session state keyed by speaker",
	TableKnowledgeEntities:   "a PERSON entity IS data about that person",
	TableOperatorProfile:     "structured profile the assistant keeps on a person",
	TableUserIdentities:      "the identity mapping itself",
}

// UncoveredTable is a personal-data-bearing table the subject axis does NOT
// link, with the reason.
//
// This map is the point of the coverage test. A table that is neither linkable
// nor listed here is a gap nobody can see — an erasure would silently miss it
// while the report claimed success. Forcing every ROPA-inventoried table into
// one of the two sets means the omission has to be argued for in writing.
//
// THREE DISTINCT REASONS live here, and conflating them would hide a gap — so
// each entry's text says which one it is:
//
//  1. RETAINED under a legal exemption (Art 17(3)(b), Art 32) — the data stays,
//     lawfully, and the erasure report names it as a retained category.
//  2. ERASED TRANSITIVELY — no exemption at all; an ON DELETE CASCADE from a
//     linkable parent already erases it in satisfaction of Art 17(1). Not
//     linkable because a link to a row that cannot outlive its parent would
//     index nothing.
//  3. OWED but NOT YET DISCHARGED — erasure is required and the mechanism is
//     not wired up. `embedding_cache` is the live example. Recorded here rather
//     than omitted, precisely so it cannot be mistaken for category 1.
//
// The 2026-07-29 additions came from extending the ROPA inventory: the coverage
// test reads that document, so tables it never named were tables nothing checked.
var UncoveredTable = map[string]string{
	"tool_audit_log": "Art 17(3)(b) — security/accountability record. Personal data appears only incidentally in tool arguments; retained under Art 32(1)(c) and reported as a retained category rather than erased.",
	"admin_audit":    "Art 17(3)(b) — administrative accountability. Same reasoning as tool_audit_log.",
	"security_incidents": "Art 17(3)(b) — the record that a personal-data breach was assessed and handled (Art 33(5)). " +
		"Erasing it on request would destroy the controller's own accountability evidence.",
	"data_subject_requests": "Art 5(2) accountability — the record that rights WERE honoured, including this subject's own requests. " +
		"Erasing it on request would destroy the evidence that the erasure happened, so it is retained under Art 17(3)(b) " +
		"and reported as a retained category.",
	"channel_disclosure_log": "AI Act Art 50/99 conformity evidence. Protected by the retention denylist " +
		"(internal/retention evidenceTables) and NOT erasable on request; reported as a retained category with its ground.",

	// --- added 2026-07-29 when the ROPA inventory was extended ---
	//
	// These were personal-data-bearing tables that the ROPA did not name, so the
	// coverage test could not see them. Classifying them is the point of that
	// test; each needed a decision, not a default.

	"entity_mentions": "Erased in satisfaction of Art 17(1) TRANSITIVELY, not retained under an exemption: " +
		"ON DELETE CASCADE from both project_memory_chunks and knowledge_entities, so it goes when either " +
		"parent is erased. Not linkable because a link to a row that cannot outlive its parents would index nothing.",

	"memory_embed_queue": "Erased in satisfaction of Art 17(1) transitively — transient work queue, " +
		"ON DELETE CASCADE from project_memory_chunks. Holds a chunk id and a timestamp, no content.",

	"memory_embed_dlq": "Erased in satisfaction of Art 17(1) transitively — dead-letter queue for failed " +
		"embeds, ON DELETE CASCADE from project_memory_chunks. Holds the chunk id and the last error, no content.",

	"embedding_cache": "Owed erasure under Art 17(1) but NOT via a link: keyed by (content_hash, model) with " +
		"no subject or project column, so it is erased by content hash rather than by row identity. " +
		"NOT AN EXEMPTION. Discharged on the erasure paths as of 2026-08-21: DeleteByArtifact, " +
		"DeleteByExtractedDocument, the slice-5c redaction transaction and Repository.HardEvict all collect " +
		"the chunk's cache keys before the delete and evict them in the same transaction. One gap REMAINS and " +
		"is filed rather than implied here — deleting a whole PROJECT does not reach it, because " +
		"ProjectDataTables is project-scoped and this table has no project column.",

	"memory_retrieval_audit": "Art 17(3)(b) accountability + Art 32(1) security record of what was retrieved " +
		"and by whom. The `query` column holds search text that routinely names a person, so this is retained " +
		"personal data, reported as a retained category rather than erased — the same treatment as tool_audit_log.",

	"memory_ingest_audit": "Art 17(3)(b) accountability record of what was admitted to memory and on whose " +
		"authority. Holds a content hash and gate decisions, not content.",

	"memory_eviction_audit": "Art 17(3)(b) accountability record that a chunk WAS removed, including by an " +
		"erasure. Erasing it on request would destroy the evidence that the erasure happened. Holds a content " +
		"hash, not content.",

	"memory_eviction_runs": "Art 17(3)(b) accountability record, same ground as memory_eviction_audit and " +
		"added with it (2026-08-21): the per-operation header recording what an eviction removed BEYOND the " +
		"chunks — knowledge-graph entities and edges, quarantined pre-ingest copies, cached embeddings. " +
		"Erasing it would destroy the only evidence that the derived data was covered. Holds counts, an " +
		"operator identifier and the operator's free-text reason; no content.",

	"data_subjects": "The subject axis itself. Pinned by data_subject_requests.subject_id ON DELETE RESTRICT, " +
		"so it cannot be deleted while any request references it — deliberately: the request ledger is the " +
		"Art 5(2) evidence that rights were honoured, and it must keep resolving to a subject. Retained under " +
		"Art 17(3)(b) and reported as a retained category.",

	"data_subject_identifiers": "Retained under Art 17(3)(b) on the same ground as data_subjects: " +
		"ON DELETE CASCADE from it, and it is RESTRICT-pinned by the request ledger. An identifier is what " +
		"makes a ledger entry resolvable to a person, so erasing it would blind the accountability record.",

	"data_subject_links": "The working index, not the record. Links to an erased row are DELETED with that row " +
		"(DataSubjectRepository.DeleteRow), because a link asserting 'this person appears in this row' is stale " +
		"personal data once the row is gone. What was erased is preserved instead in " +
		"data_subject_requests.report_json, which is retained under Art 17(3)(b).",
}

// ValidateTable reports whether a table may appear in a link.
func ValidateTable(name string) error {
	if _, ok := linkableTables[LinkableTable(name)]; ok {
		return nil
	}
	if reason, ok := UncoveredTable[name]; ok {
		return fmt.Errorf("datasubject: table %q is deliberately not linkable: %s", name, reason)
	}
	return fmt.Errorf("datasubject: table %q is not in the closed linkable set and is not listed as uncovered — "+
		"add it to one, with a reason, before linking to it", name)
}

// LinkableTables returns the closed set, sorted, for docs and diagnostics.
func LinkableTables() []string {
	out := make([]string, 0, len(linkableTables))
	for t := range linkableTables {
		out = append(out, string(t))
	}
	sort.Strings(out)
	return out
}

// Identifier is something we believe identifies a subject.
type Identifier struct {
	Kind       string // 'user_id' | 'operator_id' | 'channel' | 'email' | 'kg_entity'
	Value      string
	Source     Source
	Confidence Confidence
}

// Link says: this subject appears in this row of this table.
type Link struct {
	Table       LinkableTable
	RowID       string
	ProjectID   string // empty for global tables
	Source      Source
	Confidence  Confidence
	Exclusivity Exclusivity
}

// Subject is a person the deployment holds data about.
type Subject struct {
	ID          string
	DisplayName string
	// RequestState is non-empty while a rights request is being served, so a
	// half-finished erasure is visible rather than silently inconsistent.
	RequestState string
}

// Validate checks a link is well-formed before it reaches the database.
//
// Deliberately strict about Source/Confidence pairing: a binder that claimed
// `certain` for a KG extraction would poison every downstream report, and the
// report is the whole product here.
func (l Link) Validate() error {
	if err := ValidateTable(string(l.Table)); err != nil {
		return err
	}
	if strings.TrimSpace(l.RowID) == "" {
		return fmt.Errorf("datasubject: link row id is required")
	}
	want, err := DefaultConfidence(l.Source)
	if err != nil {
		return err
	}
	if l.Confidence == "" {
		return fmt.Errorf("datasubject: link confidence is required (source %q defaults to %q)", l.Source, want)
	}
	if !confidenceAtMost(l.Confidence, want) {
		return fmt.Errorf("datasubject: source %q cannot claim confidence %q (maximum %q) — "+
			"a binder may downgrade its confidence but never upgrade it", l.Source, l.Confidence, want)
	}
	switch l.Exclusivity {
	case ExclusiveRow, SharedRow, UnknownExclusivity:
	default:
		return fmt.Errorf("datasubject: invalid exclusivity %q", l.Exclusivity)
	}
	return nil
}

// confidenceRank orders confidence so a binder cannot upgrade its own.
func confidenceRank(c Confidence) int {
	switch c {
	case ConfidenceCertain:
		return 3
	case ConfidenceProbable:
		return 2
	case ConfidencePossible:
		return 1
	default:
		return 0
	}
}

func confidenceAtMost(got, ceiling Confidence) bool {
	g, c := confidenceRank(got), confidenceRank(ceiling)
	return g != 0 && g <= c
}
