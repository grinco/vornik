package featuredoctor

// ComputeStatus maps the live signals to one of the five feature states.
// verify is nil when gates are off (not run) or when the verify check
// errored while gates were on (treated as degraded — we can't confirm it
// works). gatesOn means every Gate is at its EnableTo value.
func ComputeStatus(gatesOn bool, prereqs []PrereqResult, verify *PrereqResult) Status {
	anyUnfixableUnmet := false
	allPrereqOK := true
	for _, p := range prereqs {
		if !p.OK {
			allPrereqOK = false
			if !p.Fixable {
				anyUnfixableUnmet = true
			}
		}
	}
	if gatesOn {
		if allPrereqOK && verify != nil && verify.OK {
			return StatusOK
		}
		return StatusDegraded
	}
	if anyUnfixableUnmet {
		return StatusBlocked
	}
	return StatusReady
}
