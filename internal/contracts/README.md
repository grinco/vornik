# `internal/contracts` — the CE↔EE published seam

This package is the **stable, versioned API boundary** between the public
Community-Edition module and the proprietary Enterprise overlay. It contains
only neutral interfaces and plain data DTOs — no IP, no heuristics, no engine
code. The Community build depends on these interfaces; the Enterprise overlay
supplies the concrete implementations at construction time via the provider
seam (`internal/service` `ProviderSet` / `WithProviders`).

It is a CE package and lives in the public CE module (post-split:
`github.com/grinco/vornik/internal/contracts`). The EE overlay
(`github.com/grinco/vornik`) imports it as a normal module dependency and
implements its interfaces. See
[`https://docs.vornik.io`](../../https://docs.vornik.io)
§4 and
[`https://docs.vornik.io`](../../https://docs.vornik.io)
§2 for the decoupling rationale.

## Current surface

Interfaces (implemented by EE, consumed by CE):

- `ReplaySafetyClassifier` — deny-by-default gate for which tools may run during
  a counterfactual replay.
- `InstinctBudgetResolver` — resolves a learned tool-budget tier.
- `HealingApplier` / `HealingObserver` — hand a counterfactual plan to the EE
  replay engine and get a trace back.

Plain DTOs that cross the seam by value (no behaviour):
`BlackBoxReplayPlan`, `LearnedTierResult`, `CounterfactualVariable`,
`CounterfactualPlan`, `ExecutionEvent`, `TraceCounts`, `ExecutionTrace`.

## Compatibility policy (treat as a published API)

1. **Versioned public API.** This package's exported surface is part of the
   Community module's public API. Treat every exported identifier as a
   compatibility commitment, not an internal detail.

2. **Breaking changes require a CE version bump.** Any
   backwards-incompatible change — removing/renaming an exported symbol,
   changing an interface method signature, removing or retyping a DTO field —
   requires a **major/minor CE module version bump** (per the CE module's
   semver). Additive changes (new optional DTO fields, new interfaces) are
   minor/patch. When in doubt, bump.

3. **The EE overlay pins a CE tag.** `github.com/grinco/vornik` `require`s a
   specific Community-module tag and updates on its **own schedule** — it is not
   forced to track CE `main`. A CE contracts change therefore does not break the
   EE build until EE deliberately bumps the pinned tag, at which point the EE
   side is updated to match in a **coordinated EE release**.

4. **Anti-leak rule (from Phase 1c).** Every interface method is defined from an
   exact CE call site, and **no method returns a broader manager / repo / engine
   object**. Keep the surface minimal: if a new need arises, add the narrowest
   method or DTO that satisfies the call site — never expose an EE engine type.

## Practical checklist when editing this package

- Adding a field to a DTO? Additive — safe; bump patch/minor.
- Changing an interface method? Breaking — bump CE minor/major, and update the
  EE implementation when EE next bumps its pinned CE tag.
- Tempted to return an engine/manager type from a method? Stop — that violates
  the anti-leak rule (§2 of the 1c design). Return a DTO or a narrow interface.
