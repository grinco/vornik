package memoryfirewall

// Bridge between the existing memory classifier
// (internal/memory/class.go) and the firewall's policy model.
// The classifier stamps content_class + validation_status on
// each chunk; the firewall consumes both as inputs to policy
// inference.
//
// This file deliberately doesn't import the memory package —
// it takes the classifier's outputs as string arguments so
// future classifier changes don't ripple here. The bridge is
// a one-way translation; the firewall doesn't write back.

// ApplyClassifierSignal adjusts a Policy based on the
// classifier's downstream metadata. Called by the Repository
// when it loads a chunk: the persisted Policy is the
// authoritative one, but the classifier may have decided
// "this is credentials" after ingest — the bridge ensures the
// effective policy reflects that even if nobody re-stamped
// the policy_digest column.
//
// Why bridge at read-time rather than re-stamping at classify-
// time: classifier runs async + may revise multiple times.
// Re-stamping on every classifier tick would invalidate every
// downstream PolicyProof. Bridging at read-time keeps the
// policy_digest stable while letting the effective sensitivity
// follow classifier updates.
func ApplyClassifierSignal(p Policy, contentClass, validationStatus string) Policy {
	// Credentials content class always wins — override
	// sensitivity to Restricted regardless of the persisted
	// tier. The classifier signals "I detected
	// credentials/PII in the content" — even an operator-
	// pasted credential is still a credential.
	if contentClass == "credentials" {
		p.Sensitivity = SensitivityRestricted
	}
	// Refuted chunks lose all permitted_roles — they should
	// not surface in recall regardless of their original
	// policy. The retention sweeper eventually removes them;
	// until then the firewall blocks them via the role gate.
	if validationStatus == "refuted" {
		p.PermittedRoles = []string{} // explicit empty = deny-all
	}
	return p
}
