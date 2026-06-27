package api

// Doctor check: model_route_coverage.
//
// Every model a swarm role pins (`model` + `modelFallback`) must be reachable
// by the chat router AND have a pricing entry. A model that matches no
// configured `model_route` prefix would fail to route at call time (or fall
// through to an unintended catch-all); a model absent from pricing.yaml has
// its cost silently estimated at the `default` rate. Both are config rot —
// usually a typo or a vendor-prefix rename that wasn't propagated to the
// routes/pricing tables. This is a static check (no DB, no --fix): the
// remedy is always an operator config edit.

import (
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
)

// modelRef is one (model, swarm, role) reference, tagged with whether it came
// from the role's primary `model` or its `modelFallback`.
type modelRef struct {
	model      string
	swarm      string
	role       string
	isFallback bool
}

// checkModelRouteCoverage asserts every model referenced by a swarm role
// resolves to a configured chat route prefix and has a pricing entry.
func (h *DoctorHandlers) checkModelRouteCoverage() DoctorCheck {
	name := "model_route_coverage"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config dir; skipping"}
	}
	if len(h.chatRoutePrefixes) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no chat model_route prefixes configured; skipping (router not in use)"}
	}

	reg := registry.New()
	if err := reg.Load(h.configDir); err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("registry load failed: %v", err)}
	}

	var table *pricing.Table
	if h.pricingPath == "" {
		// No pricing file means every model is "unpriced" — surface it as a
		// WARNING on the pricing axis rather than skipping silently.
		table = pricing.Empty()
	} else {
		t, err := pricing.Load(h.pricingPath)
		if err != nil {
			return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("load pricing table: %v", err)}
		}
		table = t
	}

	refs := collectModelRefs(reg.ListSwarms())
	findings := evalModelRouteCoverage(refs, h.chatRoutePrefixes, table)
	if len(findings) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "all swarm-role models are routed and priced"}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d model reference(s) unrouted or unpriced — fix chat routes / pricing.yaml", len(findings)),
		Items:   findings,
	}
}

// collectModelRefs flattens every role's primary + fallback model into a
// deduplicated, deterministically-ordered slice of refs. Empty model names
// (role inherits the daemon default) are skipped — they aren't role-pinned.
func collectModelRefs(swarms []*registry.Swarm) []modelRef {
	seen := map[string]bool{}
	var refs []modelRef
	add := func(model, swarm, role string, fallback bool) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := model + "\x00" + fmt.Sprint(fallback)
		if seen[key] {
			return
		}
		seen[key] = true
		refs = append(refs, modelRef{model: model, swarm: swarm, role: role, isFallback: fallback})
	}
	for _, s := range swarms {
		if s == nil {
			continue
		}
		for _, role := range s.Roles {
			add(role.Model, s.ID, role.Name, false)
			add(role.ModelFallback, s.ID, role.Name, true)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].model != refs[j].model {
			return refs[i].model < refs[j].model
		}
		return !refs[i].isFallback && refs[j].isFallback
	})
	return refs
}

// evalModelRouteCoverage returns a finding string per model that fails to
// route or fails to price. Pure — no I/O — so it's directly unit-testable.
func evalModelRouteCoverage(refs []modelRef, routePrefixes []string, table *pricing.Table) []string {
	var findings []string
	for _, r := range refs {
		routed := modelMatchesAnyPrefix(r.model, routePrefixes)
		_, priced := table.Lookup(r.model)
		if routed && priced {
			continue
		}
		var problems []string
		if !routed {
			problems = append(problems, "unrouted (no model_route prefix matches)")
		}
		if !priced {
			problems = append(problems, "unpriced (no pricing.yaml entry)")
		}
		kind := "model"
		if r.isFallback {
			kind = "modelFallback"
		}
		findings = append(findings, fmt.Sprintf("%s %q (role %q in swarm %q): %s",
			kind, r.model, r.role, r.swarm, strings.Join(problems, ", ")))
	}
	return findings
}

// modelMatchesAnyPrefix mirrors the chat router's prefix-match rule: a route
// with an empty prefix is a catch-all that matches every model.
func modelMatchesAnyPrefix(model string, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "" || strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}
