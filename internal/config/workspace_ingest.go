package config

import (
	"fmt"
	"sort"
	"strings"
)

// Google Workspace ingestion source rules.
//
// see LLD § https://docs.vornik.io §4.1

// WorkspaceSourceKind is the closed set of ways a source may be named.
//
// Closed deliberately. A free-text Drive query is NOT a kind, because it would
// make the reachable set depend on Drive's search behaviour rather than on
// something the operator can read back and reason about — which is how "narrow by
// construction" quietly stops being true.
const (
	WorkspaceSourceFolder      = "folder"
	WorkspaceSourceSharedDrive = "shared_drive"
	WorkspaceSourceLabel       = "label"
)

var workspaceSourceKinds = map[string]string{
	WorkspaceSourceFolder:      "a Drive folder ID",
	WorkspaceSourceSharedDrive: "a shared-drive ID",
	WorkspaceSourceLabel:       "a Drive label",
}

// WorkspaceSource is one ingestion rule: what to ingest, and what to accept.
type WorkspaceSource struct {
	// Kind is one of folder | shared_drive | label.
	Kind string `yaml:"kind" json:"kind" doc:"Source type: folder, shared_drive, or label. A free-text query is deliberately not permitted."`
	// ID is the folder ID, shared-drive ID, or label ID.
	ID string `yaml:"id" json:"id" doc:"The folder ID, shared-drive ID, or label ID this rule covers."`
	// Recursive descends into sub-folders. Meaningful for folder rules only.
	Recursive bool `yaml:"recursive" json:"recursive" doc:"For folder rules, also ingest sub-folders."`
	// MIMETypes is the set of Drive MIME types this rule accepts.
	//
	// Required, and the reason is a failure mode rather than tidiness: a rule
	// meaning "the minutes in this folder" would otherwise silently start
	// ingesting whatever else someone drops there — a spreadsheet of salaries,
	// a scan of a passport.
	MIMETypes []string `yaml:"mime_types" json:"mime_types" doc:"Drive MIME types this rule accepts. Required: without it the rule ingests anything dropped into the source."`
}

// WorkspaceIngestConfig governs automatic ingestion of Workspace documents.
//
// Ingest-on-request (a human pasting a link) is deliberately NOT governed here:
// that is one person designating one document with a human in the loop, and it
// needs no standing permission. Everything below is about the unattended path.
type WorkspaceIngestConfig struct {
	Enabled bool              `yaml:"enabled" json:"enabled" doc:"Turn on automatic ingestion of Workspace documents. Off by default; ingest-on-request works regardless."`
	Sources []WorkspaceSource `yaml:"sources" json:"sources" doc:"Explicit source rules. No rules means nothing is ingested automatically."`
}

// Active reports whether automatic ingestion would actually do anything.
//
// Enabled-but-ruleless is a no-op rather than an error: a customer who configures
// nothing ingests nothing, which is the correct failure direction for a feature
// that reads other people's documents.
func (c WorkspaceIngestConfig) Active() bool {
	return c.Enabled && len(c.Sources) > 0
}

// Validate checks the ingestion rules and the two preconditions that make
// unattended ingestion defensible.
//
// retentionMemoryChunkDays is `retention.memory_chunks_days`; redactionAvailable
// reports whether Art 17 shared-row redaction (erasure slice 5c) has shipped.
//
// BOTH PRECONDITIONS ARE CODE GATES RATHER THAN DOCUMENTED ADVICE, and they are
// checked at config load so the operator learns at startup instead of on the first
// unattended cycle at 03:00.
func (c WorkspaceIngestConfig) Validate(retentionMemoryChunkDays int, redactionAvailable bool) error {
	// Disabled is inert. An operator drafting a rule they have not switched on
	// must not be blocked from starting the daemon.
	if !c.Enabled {
		return nil
	}
	for i, s := range c.Sources {
		if err := s.validate(i); err != nil {
			return err
		}
	}
	// Enabled with no rules ingests nothing, which is a no-op rather than a
	// misconfiguration — so the preconditions below do not apply.
	if !c.Active() {
		return nil
	}

	// Precondition 1 — retention. Automatically ingesting meeting records into a
	// store with no window is indefinite retention of the most sensitive content
	// the deployment holds. The doctor already warns about the unset window; this
	// feature would turn that warning into an Art 5(1)(e) breach by default.
	if retentionMemoryChunkDays <= 0 {
		return fmt.Errorf(
			"workspace_ingest: refusing to enable automatic ingestion while " +
				"retention.memory_chunks_days is unset (0 = keep forever). Ingested meeting " +
				"records would be retained indefinitely, which is a storage-limitation breach " +
				"(GDPR Art 5(1)(e)) rather than a default. Set a window and restart")
	}

	// Precondition 2 — shared-row redaction (erasure slice 5c).
	//
	// SCOPED TO ALL AUTOMATIC INGESTION, not to transcripts specifically, and the
	// reason is that the distinction cannot be made: a Google Meet transcript IS a
	// Google Doc, so no MIME type separates it from any other document — and
	// meeting minutes are just as multi-person as a transcript anyway. What CAN be
	// distinguished is unattended versus human-designated: ingest-on-request keeps
	// a person in the loop who can accept the narrower guarantee, while an
	// unattended rule cannot.
	if !redactionAvailable {
		return fmt.Errorf(
			"workspace_ingest: refusing to enable automatic ingestion until Art 17 " +
				"shared-row redaction is available. Every ingested meeting record concerns " +
				"several people, so an erasure request over them would report almost all as " +
				"deferred rather than erased — 'reported but not erased' as the normal case. " +
				"Ingest-on-request is unaffected and works today")
	}
	return nil
}

func (s WorkspaceSource) validate(i int) error {
	if _, ok := workspaceSourceKinds[s.Kind]; !ok {
		return fmt.Errorf(
			"workspace_ingest.sources[%d]: kind %q is not permitted — use one of %s. "+
				"A free-text query is deliberately excluded: it would make the set of "+
				"ingested documents depend on Drive's search behaviour rather than on "+
				"something you can read back",
			i, s.Kind, strings.Join(sortedSourceKinds(), ", "))
	}
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("workspace_ingest.sources[%d]: id is required (%s)", i, workspaceSourceKinds[s.Kind])
	}
	if len(s.MIMETypes) == 0 {
		return fmt.Errorf(
			"workspace_ingest.sources[%d]: mime_types is required. Without it this rule "+
				"ingests anything dropped into the source, not only the documents you meant",
			i)
	}
	for _, m := range s.MIMETypes {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("workspace_ingest.sources[%d]: mime_types contains a blank entry", i)
		}
	}
	return nil
}

func sortedSourceKinds() []string {
	out := make([]string, 0, len(workspaceSourceKinds))
	for k := range workspaceSourceKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
