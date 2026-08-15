package speedprofile

import "fmt"

// FactorConfig bounds how far a measured profile may stretch a timeout.
type FactorConfig struct {
	// ReferenceTokensPerSec is the decode rate the shipped base timeouts were
	// calibrated against. DECLARED, never inferred: inferring it from the local
	// fleet would make every deployment its own reference and collapse the
	// factor to 1.0 everywhere — a profile that never fires.
	ReferenceTokensPerSec float64
	MinFactor             float64
	MaxFactor             float64
}

// Factor is how much longer this host needs than the reference did.
//
// Clamped is true when the hardware wanted more than MaxFactor allows. That is
// a reportable event, not a detail: the operator sees an ordinary timeout
// otherwise, with nothing to suggest the budget was never adequate. The measured
// case is real — a 12 tok/s host against a 200 tok/s reference wants ~17x, well
// past any sane ceiling.
func Factor(decodeTokensPerSec float64, cfg FactorConfig) (factor float64, clamped bool, err error) {
	if cfg.ReferenceTokensPerSec <= 0 {
		return 1, false, fmt.Errorf("speed factor: reference throughput must be declared and " +
			"positive; without it there is nothing to be slower THAN")
	}
	if decodeTokensPerSec <= 0 {
		// No usable profile. Behave exactly as an unconfigured deployment:
		// scaling must never be a startup dependency.
		return 1, false, nil
	}
	raw := cfg.ReferenceTokensPerSec / decodeTokensPerSec
	switch {
	case cfg.MaxFactor > 0 && raw > cfg.MaxFactor:
		return cfg.MaxFactor, true, nil
	case cfg.MinFactor > 0 && raw < cfg.MinFactor:
		return cfg.MinFactor, false, nil
	}
	return raw, false, nil
}
