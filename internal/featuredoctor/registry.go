package featuredoctor

import (
	"context"
	"reflect"
)

// Registry is the declared set of features. Adding one is a code change.
func Registry() []Feature {
	return []Feature{instinctFeature(), authFeature(), memoryRAGFeature(), clusterFeature(), tradingFeature()}
}

// Diagnosis is the computed report for one feature.
type Diagnosis struct {
	Feature Feature
	Status  Status
	GatesOn bool
	Prereqs []NamedResult
	Verify  *PrereqResult
}

// NamedResult pairs a prereq name with its check result.
type NamedResult struct {
	Name string
	PrereqResult
}

func gatesOn(f Feature, cfg ConfigReader) bool {
	for _, g := range f.Gates {
		v, ok := cfg.GateValue(g.Key)
		// reflect.DeepEqual instead of == to avoid a panic when EnableTo is
		// an uncomparable type (slice/map) — behaviour is identical for the
		// current bool/string values.
		if !ok || !reflect.DeepEqual(v, g.EnableTo) {
			return false
		}
	}
	return true
}

// Diagnose runs a feature's prereqs (+ verify when gates are on) and
// computes its status. Fail-soft: a check is run inside the slice; the
// caller's recover is not needed because checks return results, not panics.
func Diagnose(ctx context.Context, f Feature, deps Deps) Diagnosis {
	// Without a ConfigReader we cannot evaluate gates or prereq closures that
	// read config; return StatusUnknown rather than panic.
	if deps.Config == nil {
		return Diagnosis{Feature: f, Status: StatusUnknown}
	}
	on := gatesOn(f, deps.Config)
	var results []NamedResult
	var prs []PrereqResult
	for _, p := range f.Prereqs {
		r := p.Check(ctx, deps)
		results = append(results, NamedResult{Name: p.Name, PrereqResult: r})
		prs = append(prs, r)
	}
	var verify *PrereqResult
	if on && f.Verify != nil {
		v := f.Verify(ctx, deps)
		verify = &v
	}
	return Diagnosis{
		Feature: f, Status: ComputeStatus(on, prs, verify),
		GatesOn: on, Prereqs: results, Verify: verify,
	}
}
