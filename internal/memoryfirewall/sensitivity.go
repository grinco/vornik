package memoryfirewall

// SensitivityDefaults maps a tier to the permitted_roles +
// allowed_purposes the firewall applies when the chunk's own
// policy doesn't set them. Operators can override per-chunk;
// these are the tier-level baselines.
//
// The tier defaults are deliberately permissive — single-tenant
// deployments need the firewall to act as audit-only without
// breaking workflows. The opt-in path is per-chunk
// permitted_roles / allowed_purposes; the tier just shapes the
// fallback.

// DefaultsForTier returns the (permittedRoles, allowedPurposes)
// pair the evaluator uses when the chunk's policy leaves both
// nil. Nil-slice semantics: nil means "no restriction" (i.e.
// every role / every purpose is allowed); a non-nil empty slice
// means "no role / no purpose is allowed" — used by tests to
// pin the deny-all corner case.
//
// Trust note: the SensitivityRestricted tier is special-cased
// in the evaluator (it requires an operator_id on the request),
// not in the defaults — the role / purpose dimensions stay
// permissive even for restricted chunks; the gate is at the
// operator-presence check.
func DefaultsForTier(t SensitivityTier) (permittedRoles []string, allowedPurposes []Purpose) {
	switch t {
	case SensitivityPublic:
		return nil, nil
	case SensitivityInternal:
		return nil, nil
	case SensitivityConfidential:
		// All roles can read, but only operational purpose by
		// default. Training-data pipelines must opt in per-chunk.
		return nil, []Purpose{PurposeOperational, PurposeAuditReview}
	case SensitivityRestricted:
		// All roles can read (the operator-presence check is
		// the real gate); only operational + audit purposes.
		// Training-data + compliance-export require explicit
		// per-chunk allow.
		return nil, []Purpose{PurposeOperational, PurposeAuditReview}
	default:
		// Unknown tier — apply most-restrictive defaults.
		// Better to false-block than false-allow on an
		// unrecognised tier; operators see the block in audit.
		return nil, []Purpose{PurposeOperational}
	}
}
