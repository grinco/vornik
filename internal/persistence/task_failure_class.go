package persistence

// IsKnownTaskFailureClass reports whether s is a class the playbook corpus can resolve.
//
// INCIDENT 2026-07-30: a FAILED task carried last_error but an EMPTY last_error_class, so
// `vornikctl task explain` had nothing to explain and `vornikctl playbook show` had nothing
// to look up — the corpus the troubleshooting guide calls "the highest-value and
// least-known path in the product" was unreachable from the failure it was written for.
//
// Writers use this to avoid the opposite mistake: stamping a class nobody has written a
// playbook entry for just relocates the dead end.
func IsKnownTaskFailureClass(s string) bool {
	switch s {
	case TaskFailureClassBudgetBlocked,
		TaskFailureClassCancelled,
		TaskFailureClassChildFailed,
		TaskFailureClassDelegationGuard,
		TaskFailureClassGateFailed,
		TaskFailureClassHallucinatedPlacement,
		TaskFailureClassInvalidOutput,
		TaskFailureClassInvalidOutputLoop,
		TaskFailureClassLeaseExpired,
		TaskFailureClassLLMError,
		TaskFailureClassMergeFailed,
		TaskFailureClassOrphaned,
		TaskFailureClassRateLimited,
		TaskFailureClassRuntimeError,
		TaskFailureClassSecretLeak,
		TaskFailureClassStuckExecution,
		TaskFailureClassTimeout,
		TaskFailureClassToolError,
		TaskFailureClassToolIterationLimit,
		TaskFailureClassUnknown,
		TaskFailureClassWorkflowCfg,
		TaskFailureClassWorkflowDrift,
		TaskFailureClassWorkflowRole:
		return true
	}
	return false
}
