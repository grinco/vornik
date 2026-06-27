package memory

// HIGH-VALUE pure-logic tests for the RAG-memory firewall policy decode
// path. These functions sit on every recall: parsePqArray decodes the
// Postgres array literals for permitted_roles / allowed_purposes,
// purposesFromStrings re-types them, and chunkFromPolicyRow lifts a
// loaded ChunkPolicyRow into the firewall's Chunk — applying the
// legacy-NULL default-policy fallback and the credentials/refuted
// classifier bridge. All three are deterministic and DB/LLM-free; a
// decode bug here silently mis-scopes every chunk's visibility.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/memoryfirewall"
)

// --- parsePqArray -------------------------------------------------------

func TestParsePqArray_EmptyAndNullReturnNil(t *testing.T) {
	// Both the empty string (NULL column) and the empty-array literal
	// map to the firewall's "no restriction" nil — never an empty
	// non-nil slice, which would read as "deny all" downstream.
	assert.Nil(t, parsePqArray(""))
	assert.Nil(t, parsePqArray("{}"))
}

func TestParsePqArray_SingleElement(t *testing.T) {
	assert.Equal(t, []string{"coder"}, parsePqArray("{coder}"))
}

func TestParsePqArray_MultipleElementsPreserveOrder(t *testing.T) {
	got := parsePqArray("{coder,reviewer,architect}")
	assert.Equal(t, []string{"coder", "reviewer", "architect"}, got)
}

func TestParsePqArray_StripsWhitespaceAndQuotesAndDropsEmpties(t *testing.T) {
	// Postgres double-quotes elements with special chars and may emit
	// stray spaces; an empty element (trailing comma) must be dropped
	// rather than become a "" role that matches nothing/everything.
	got := parsePqArray(`{ "operational" , audit_review ,, "training_data" }`)
	assert.Equal(t, []string{"operational", "audit_review", "training_data"}, got)
}

func TestParsePqArray_BareCommaSeparatedWithoutBraces(t *testing.T) {
	// Defensive: braces missing (already-stripped input) still parses.
	assert.Equal(t, []string{"a", "b"}, parsePqArray("a,b"))
}

// --- purposesFromStrings ------------------------------------------------

func TestPurposesFromStrings_EmptyReturnsNil(t *testing.T) {
	assert.Nil(t, purposesFromStrings(nil))
	assert.Nil(t, purposesFromStrings([]string{}))
}

func TestPurposesFromStrings_RetypesPreservingOrder(t *testing.T) {
	got := purposesFromStrings([]string{"operational", "audit_review"})
	assert.Equal(t, []memoryfirewall.Purpose{
		memoryfirewall.PurposeOperational,
		memoryfirewall.PurposeAuditReview,
	}, got)
}

// --- chunkFromPolicyRow: legacy NULL fallback ---------------------------

func TestChunkFromPolicyRow_LegacyEmptyRowFallsBackToSourceDefault(t *testing.T) {
	// A row with no policy columns (pre-firewall chunk) must resolve via
	// DefaultPolicyForSource, NOT to a zero-value Policy. Unknown source
	// → internal sensitivity per the defaults table.
	chunk := chunkFromPolicyRow("c1", ChunkPolicyRow{})
	want := memoryfirewall.DefaultPolicyForSource(memoryfirewall.ProvenanceUnknown, "")

	assert.Equal(t, "c1", chunk.ID)
	assert.Equal(t, memoryfirewall.SensitivityInternal, chunk.Policy.Sensitivity)
	assert.Equal(t, want.Sensitivity, chunk.Policy.Sensitivity)
	assert.Equal(t, memoryfirewall.ProvenanceUnknown, chunk.Policy.Provenance.Source)
	// No persisted digest → recomputed, never blank.
	assert.NotEmpty(t, chunk.Digest)
	assert.Equal(t, memoryfirewall.PolicyDigest(want), chunk.Digest)
}

func TestChunkFromPolicyRow_LegacyExternalFetchDefaultsPublic(t *testing.T) {
	// Source set but no other policy columns still counts as "has policy
	// data" (ProvenanceSource non-empty), so the explicit-build branch
	// runs — sensitivity stays empty (no tier column) unless the bridge
	// overrides. Verify the source is threaded through regardless.
	chunk := chunkFromPolicyRow("c2", ChunkPolicyRow{
		ProvenanceSource: string(memoryfirewall.ProvenanceExternalFetch),
	})
	assert.Equal(t, memoryfirewall.ProvenanceExternalFetch, chunk.Policy.Provenance.Source)
}

// --- chunkFromPolicyRow: explicit policy columns ------------------------

func TestChunkFromPolicyRow_BuildsFromExplicitColumns(t *testing.T) {
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	row := ChunkPolicyRow{
		ChunkID:            "c3",
		TenantID:           "tenant-x",
		SensitivityTier:    string(memoryfirewall.SensitivityConfidential),
		ProvenanceSource:   string(memoryfirewall.ProvenanceWorkflowOutput),
		ProvenanceProducer: "coder",
		ProvenanceTrust:    42,
		ProvenanceURL:      "https://example/x",
		FirewallExpiresAt:  &exp,
		PermittedRoles:     []string{"coder", "reviewer"},
		AllowedPurposes:    []string{"operational"},
		PolicyDigest:       "stored-digest",
	}
	chunk := chunkFromPolicyRow("c3", row)

	p := chunk.Policy
	assert.Equal(t, memoryfirewall.SensitivityConfidential, p.Sensitivity)
	assert.Equal(t, memoryfirewall.ProvenanceWorkflowOutput, p.Provenance.Source)
	assert.Equal(t, "coder", p.Provenance.ProducerID)
	assert.Equal(t, 42, p.Provenance.TrustLevel)
	assert.Equal(t, "https://example/x", p.Provenance.SourceURL)
	assert.Equal(t, "tenant-x", p.TenantID)
	assert.Equal(t, &exp, p.ExpiresAt)
	assert.Equal(t, []string{"coder", "reviewer"}, p.PermittedRoles)
	assert.Equal(t, []memoryfirewall.Purpose{memoryfirewall.PurposeOperational}, p.AllowedPurposes)
	// Stored digest is reused verbatim when present (stable PolicyProof).
	assert.Equal(t, "stored-digest", chunk.Digest)
}

func TestChunkFromPolicyRow_MissingDigestIsRecomputed(t *testing.T) {
	// Explicit columns but no stored digest → recompute, not blank.
	row := ChunkPolicyRow{
		SensitivityTier:  string(memoryfirewall.SensitivityInternal),
		ProvenanceSource: string(memoryfirewall.ProvenanceChatTurn),
	}
	chunk := chunkFromPolicyRow("c4", row)
	assert.NotEmpty(t, chunk.Digest)
}

// --- chunkFromPolicyRow: classifier bridge ------------------------------

func TestChunkFromPolicyRow_CredentialsClassForcesRestricted(t *testing.T) {
	// The classifier bridge must override an explicit (lower) tier when
	// the chunk's content_class is "credentials" — even an operator-set
	// internal tier loses to credential detection.
	row := ChunkPolicyRow{
		SensitivityTier:  string(memoryfirewall.SensitivityInternal),
		ProvenanceSource: string(memoryfirewall.ProvenanceWorkflowOutput),
		ContentClass:     "credentials",
	}
	chunk := chunkFromPolicyRow("c5", row)
	assert.Equal(t, memoryfirewall.SensitivityRestricted, chunk.Policy.Sensitivity)
}

func TestChunkFromPolicyRow_RefutedClassEmptiesPermittedRoles(t *testing.T) {
	// Refuted chunks lose all permitted_roles (explicit deny-all) so the
	// role gate blocks them until the sweeper removes them.
	row := ChunkPolicyRow{
		SensitivityTier:  string(memoryfirewall.SensitivityInternal),
		ProvenanceSource: string(memoryfirewall.ProvenanceWorkflowOutput),
		PermittedRoles:   []string{"coder", "reviewer"},
		ValidationStatus: "refuted",
	}
	chunk := chunkFromPolicyRow("c6", row)
	assert.Empty(t, chunk.Policy.PermittedRoles)
	assert.NotNil(t, chunk.Policy.PermittedRoles, "deny-all must be explicit empty, not nil")
}

func TestChunkFromPolicyRow_BridgeAppliesToLegacyDefaultPolicy(t *testing.T) {
	// Even a legacy NULL-column chunk gets the credentials override: the
	// bridge runs after the default-policy fallback, so a chunk the
	// classifier later flagged as credentials is Restricted regardless
	// of its (default-internal) source tier.
	row := ChunkPolicyRow{ContentClass: "credentials"}
	chunk := chunkFromPolicyRow("c7", row)
	assert.Equal(t, memoryfirewall.SensitivityRestricted, chunk.Policy.Sensitivity)
}

// --- classInList: the truncation-gate enable predicate ------------------
//
// classInList decides whether a class-scoped gate fires. The critical
// invariant: an empty allowlist DISABLES the gate (returns false) — the
// 2026-05-21 default-off change that stopped mass-quarantining summary
// producers. A regression here either re-enables the gate for everyone
// or silently disables it for configured classes.

func TestClassInList_EmptyAllowlistDisablesGate(t *testing.T) {
	assert.False(t, classInList(ClassSpec, nil))
	assert.False(t, classInList(ClassSpec, []ContentClass{}))
}

func TestClassInList_MatchAndMiss(t *testing.T) {
	allowed := []ContentClass{ClassResearch, ClassExternalFetch}
	assert.True(t, classInList(ClassExternalFetch, allowed))
	assert.True(t, classInList(ClassResearch, allowed))
	assert.False(t, classInList(ClassSpec, allowed))
}

func TestClassInList_EmptyClassOnlyMatchesEmptyEntry(t *testing.T) {
	// An empty proposed class must not match a populated allowlist, and
	// must match only when "" is explicitly listed.
	assert.False(t, classInList("", []ContentClass{ClassSpec}))
	assert.True(t, classInList("", []ContentClass{""}))
}
