package config

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
)

// Validation for the Workspace ingestion source rules (LLD §4.1).
//
// Three design properties are enforced here rather than left to documentation,
// because all three fail silently otherwise: a source rule must come from a
// CLOSED set (an open-ended matcher is how "narrow by construction" stops being
// true), and automatic ingestion must not start without the retention window and
// the shared-row redaction that make it defensible to hold other people's
// meeting records.
//
// `redactionAvailable` is false until erasure slice 5c ships, so the second
// precondition currently refuses automatic ingestion outright — which is the
// designed behaviour, not a gap. Ingest-on-request is unaffected.

const (
	retentionSet   = 365
	retentionUnset = 0
	withRedaction  = true
	noRedaction    = false
)

func enabledSource(s WorkspaceSource) WorkspaceIngestConfig {
	return WorkspaceIngestConfig{Enabled: true, Sources: []WorkspaceSource{s}}
}

func validFolder() WorkspaceSource {
	return WorkspaceSource{
		Kind:      "folder",
		ID:        "1AbCdEf",
		MIMETypes: []string{"application/vnd.google-apps.document"},
	}
}

// --- the closed set ---

func TestWorkspaceIngest_KindMustBeInTheClosedSet(t *testing.T) {
	for _, kind := range []string{"folder", "shared_drive", "label"} {
		t.Run("valid/"+kind, func(t *testing.T) {
			src := validFolder()
			src.Kind = kind
			if err := enabledSource(src).Validate(retentionSet, withRedaction); err != nil {
				t.Errorf("%q is a valid kind: %v", kind, err)
			}
		})
	}
	for _, kind := range []string{"query", "search", "everything", "", "Folder"} {
		t.Run("invalid/"+kind, func(t *testing.T) {
			src := validFolder()
			src.Kind = kind
			err := enabledSource(src).Validate(retentionSet, withRedaction)
			if err == nil {
				t.Fatalf("%q must be rejected — an open-ended matcher is how 'narrow by "+
					"construction' quietly stops being true", kind)
			}
			if !strings.Contains(err.Error(), "folder") {
				t.Errorf("the error should list the permitted kinds, got %v", err)
			}
		})
	}
}

// A free-text query is deliberately not a rule type: it would make the reachable
// set depend on Drive's search behaviour rather than on something the operator
// can read back.
func TestWorkspaceIngest_FreeTextQueryIsExplicitlyRefused(t *testing.T) {
	src := validFolder()
	src.Kind = "query"
	if err := enabledSource(src).Validate(retentionSet, withRedaction); err == nil {
		t.Fatal("a query rule must be refused")
	}
}

func TestWorkspaceIngest_IDIsRequired(t *testing.T) {
	for _, id := range []string{"", "   "} {
		src := validFolder()
		src.ID = id
		if err := enabledSource(src).Validate(retentionSet, withRedaction); err == nil {
			t.Errorf("a blank id must be refused (%q) — it would match nothing or everything", id)
		}
	}
}

// Without declared MIME types, "the minutes in this folder" silently starts
// ingesting whatever else gets dropped there.
func TestWorkspaceIngest_MIMETypesAreRequired(t *testing.T) {
	src := validFolder()
	src.MIMETypes = nil
	err := enabledSource(src).Validate(retentionSet, withRedaction)
	if err == nil {
		t.Fatal("a source with no mime_types must be refused")
	}
	if !strings.Contains(err.Error(), "mime_types") {
		t.Errorf("the error should name mime_types, got %v", err)
	}

	blank := validFolder()
	blank.MIMETypes = []string{" "}
	if err := enabledSource(blank).Validate(retentionSet, withRedaction); err == nil {
		t.Error("a blank mime type entry must be refused")
	}
}

// --- precondition 1: retention ---

// Automatic ingestion into a store with no retention window is indefinite
// retention of the most sensitive content the system holds. A code gate, not a
// doctor warning — the warning already exists, and this feature would turn it
// into an Art 5(1)(e) breach by default.
func TestWorkspaceIngest_RefusesWhenRetentionWindowUnset(t *testing.T) {
	err := enabledSource(validFolder()).Validate(retentionUnset, withRedaction)
	if err == nil {
		t.Fatal("automatic ingestion must refuse when memory_chunks_days is unset")
	}
	for _, want := range []string{"memory_chunks_days", "retention"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must tell the operator what to set (%q): %v", want, err)
		}
	}
}

// --- precondition 2: shared-row redaction (erasure slice 5c) ---

// Scoped to ALL automatic ingestion rather than to transcripts, because the
// distinction cannot be made: a Meet transcript IS a Google Doc, so no MIME type
// separates it from any other document, and minutes are equally multi-person.
// What can be distinguished is unattended versus human-designated.
func TestWorkspaceIngest_RefusesWithoutSharedRowRedaction(t *testing.T) {
	err := enabledSource(validFolder()).Validate(retentionSet, noRedaction)
	if err == nil {
		t.Fatal("automatic ingestion must refuse until shared-row redaction is available")
	}
	if !strings.Contains(err.Error(), "redaction") {
		t.Errorf("the error should name the missing capability, got %v", err)
	}
	// And it must point out that the on-request path still works, or an operator
	// reads this as "the feature is broken" rather than "this half is gated".
	if !strings.Contains(err.Error(), "request") {
		t.Errorf("the error should say ingest-on-request is unaffected, got %v", err)
	}
}

// Both preconditions satisfied → a well-formed rule is accepted. This is the test
// that will start passing for real when 5c lands.
func TestWorkspaceIngest_PermittedWhenBothPreconditionsMet(t *testing.T) {
	if err := enabledSource(validFolder()).Validate(retentionSet, withRedaction); err != nil {
		t.Fatalf("a valid rule with retention set and redaction available must pass: %v", err)
	}
}

// The retention gate is checked before the redaction gate only by accident of
// order; neither may be skipped because the other fails. Pinned so a refactor
// cannot make one gate shadow the other.
func TestWorkspaceIngest_BothPreconditionsFailingStillRefuses(t *testing.T) {
	if err := enabledSource(validFolder()).Validate(retentionUnset, noRedaction); err == nil {
		t.Fatal("both preconditions unmet must refuse")
	}
}

// --- inertness ---

// A disabled config must not fail startup, even if a half-drafted rule is present.
func TestWorkspaceIngest_DisabledIsInertEvenWhenIncomplete(t *testing.T) {
	cfg := WorkspaceIngestConfig{
		Enabled: false,
		Sources: []WorkspaceSource{{Kind: "nonsense"}},
	}
	if err := cfg.Validate(retentionUnset, noRedaction); err != nil {
		t.Fatalf("a disabled ingest config must not fail startup: %v", err)
	}
	if cfg.Active() {
		t.Error("disabled must never be active")
	}
}

// Enabled with no sources ingests nothing — a no-op, not an error. A customer who
// configures nothing gets nothing, which is the correct failure direction here.
func TestWorkspaceIngest_EnabledWithNoSourcesIngestsNothing(t *testing.T) {
	cfg := WorkspaceIngestConfig{Enabled: true}
	if err := cfg.Validate(retentionUnset, noRedaction); err != nil {
		t.Fatalf("enabled-but-ruleless must be a no-op, not an error: %v", err)
	}
	if cfg.Active() {
		t.Error("no sources means nothing is ingested — Active() must be false")
	}
}

func TestWorkspaceIngest_ActiveOnlyWhenEnabledAndRuled(t *testing.T) {
	if (WorkspaceIngestConfig{Enabled: false, Sources: []WorkspaceSource{validFolder()}}).Active() {
		t.Error("disabled must not be active however many rules exist")
	}
	if !(WorkspaceIngestConfig{Enabled: true, Sources: []WorkspaceSource{validFolder()}}).Active() {
		t.Error("enabled with a rule must be active")
	}
}

// --- the gate through the REAL loader path ---
//
// The unit tests above exercise Validate directly. This one proves the gate is
// actually WIRED into Config.Validate, because a precondition that is never
// called is indistinguishable from one that does not exist — the failure mode
// this session kept finding.

func TestConfigValidate_RefusesEnabledIngestWithoutItsPreconditions(t *testing.T) {
	// minimalValidConfig (model_capabilities_test.go) is the smallest Config that
	// clears the earlier checks, so this test isolates the ingest gate instead of
	// tripping over "server address is required" — which is what a bare &Config{}
	// does, and which would have made this test pass for the wrong reason.
	cfg := minimalValidConfig()
	cfg.Retention.MemoryChunksDays = 0 // unset — the live deployment's state
	cfg.WorkspaceIngest = WorkspaceIngestConfig{
		Enabled: true,
		Sources: []WorkspaceSource{{
			Kind: "folder", ID: "1AbC",
			MIMETypes: []string{"application/vnd.google-apps.document"},
		}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Config.Validate must refuse enabled automatic ingestion with no retention window — " +
			"if this passes, the §4.1 gate is not wired into the loader")
	}
	// Either precondition may be the one reported first; both must be reachable.
	msg := err.Error()
	if !strings.Contains(msg, "memory_chunks_days") && !strings.Contains(msg, "redaction") {
		t.Errorf("the refusal should name a precondition, got %v", err)
	}
}

// A default config must still load. The gate must not become a startup blocker
// for every deployment that has never heard of Workspace ingestion.
func TestConfigValidate_DefaultConfigIsUnaffectedByTheIngestGate(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Retention.MemoryChunksDays = 0 // deliberately the unset, live-deployment state
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a deployment that never configured Workspace ingestion must still "+
			"load with no retention window — the gate must not become a universal "+
			"startup blocker: %v", err)
	}
}

// The gate's real-world composition, asserted against the SHIPPED constant rather
// than a parameter — the parameterised tests above prove the logic, this proves the
// deployment. Slice 5c flipped datasubject.SharedRowRedactionAvailable to true on
// 2026-07-30, so from now on the only thing standing between an operator and
// unattended Workspace ingestion is a retention period.
func TestWorkspaceIngest_GateNowTurnsOnlyOnRetention(t *testing.T) {
	if !datasubject.SharedRowRedactionAvailable {
		t.Fatal("slice 5c has shipped; if this is false again, unattended ingestion must " +
			"stay refused and this test should be the thing that says so")
	}
	cfg := WorkspaceIngestConfig{
		Enabled: true,
		Sources: []WorkspaceSource{{
			Kind: WorkspaceSourceFolder, ID: "folder-1",
			MIMETypes: []string{"application/vnd.google-apps.document"},
		}},
	}
	// Retention unset: still refused, because ingested meeting records would be kept
	// forever with no Art 5(1)(e) storage limit.
	if err := cfg.Validate(0, datasubject.SharedRowRedactionAvailable); err == nil {
		t.Error("with no retention period, unattended ingestion must still refuse")
	}
	// Retention set: permitted.
	if err := cfg.Validate(365, datasubject.SharedRowRedactionAvailable); err != nil {
		t.Errorf("with redaction shipped and a retention period set, ingestion should be "+
			"permitted: %v", err)
	}
}
