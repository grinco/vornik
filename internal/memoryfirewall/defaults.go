package memoryfirewall

// Per-ProvenanceSource default Policy. The memory Indexer
// calls DefaultPolicyForSource when IngestText lands a chunk
// without an explicit Policy; this gives zero-config
// deployments sensible behaviour without operator action.
//
// The defaults table is deliberately permissive — it shapes
// the fallback when nothing else is set. Operators opt into
// stricter policies per-chunk via the admin REST API or per-
// project via the YAML config (Phase D follow-on).
//
// Trust levels are seeded from deposit-time confidence — the
// existing memory.Indexer already passes a producer-role
// confidence score; we mirror it.

// DefaultPolicyForSource returns the Policy the firewall
// applies to a chunk with no explicit metadata. contentClass
// is the existing classifier output (decision / spec /
// credentials / ...) — when it's "credentials" we override the
// source-default sensitivity to Restricted regardless of how
// the chunk was deposited.
func DefaultPolicyForSource(source ProvenanceSource, contentClass string) Policy {
	p := Policy{
		Provenance: Provenance{
			Source:     source,
			TrustLevel: defaultTrustLevelFor(source),
		},
		Sensitivity: defaultSensitivityFor(source),
	}
	// Classifier-driven overrides. Credentials chunks (the
	// classifier flags "api_key=", "password:", oauth tokens
	// in content) MUST land Restricted regardless of source —
	// even an operator-pasted credential is a credential.
	if contentClass == "credentials" {
		p.Sensitivity = SensitivityRestricted
	}
	// Apply tier defaults to permitted_roles / allowed_purposes.
	roles, purposes := DefaultsForTier(p.Sensitivity)
	p.PermittedRoles = roles
	p.AllowedPurposes = purposes
	return p
}

func defaultSensitivityFor(s ProvenanceSource) SensitivityTier {
	switch s {
	case ProvenanceOperatorCorrection,
		ProvenanceWorkflowOutput,
		ProvenanceChatTurn,
		ProvenanceIngestedArtifact,
		ProvenanceSelfConsolidated:
		return SensitivityInternal
	case ProvenanceExternalFetch:
		return SensitivityPublic
	case ProvenanceCompanionRemember:
		return SensitivityConfidential
	default:
		return SensitivityInternal
	}
}

// defaultTrustLevelFor mirrors the deposit-time confidence
// score the existing memory.Indexer attaches as a chunk-level
// "confidence" field (operator corrections 0.95; ingested
// artifacts 0.70; LLM-generated 0.50; external fetches 0.30).
// Scaled to the 0-100 range the firewall's TrustLevel uses.
func defaultTrustLevelFor(s ProvenanceSource) int {
	switch s {
	case ProvenanceOperatorCorrection:
		return 95
	case ProvenanceIngestedArtifact:
		return 70
	case ProvenanceWorkflowOutput, ProvenanceChatTurn, ProvenanceSelfConsolidated:
		return 50
	case ProvenanceCompanionRemember:
		return 60 // operator-deposited via companion plugin; trust > LLM but < direct correction
	case ProvenanceExternalFetch:
		return 30
	default:
		return 50
	}
}
