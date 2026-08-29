package agentbench

import "testing"

// Batches of ONE arm observe different ROLE SETS: a batch only sees the roles
// its five tasks actually invoked. Observed 2026-08-29 on a completed 30-task
// arm — batch 0's tasks never used analyst or tester, batch 1's did — and the
// merge refused, naming `models` and `agent_images` as differing when every
// shared role held an identical value.
//
// Unlike taskSet/scoring/tier, these maps are OBSERVED rather than derived from
// the task set, so they cannot be made whole-set at build time (§12.13's fix
// does not reach them). Coverage may differ; VALUES may not.

func armWithRoles(models, images map[string]string) ArmFields {
	return ArmFields{
		HarnessVersion: HarnessVersion, Name: "arm", BinarySHA256: "b", ConfigSHA256: "c",
		ContextPolicy: "p", TaskSetSHA256: "t", ScoringPolicySHA256: "s",
		TierPolicySHA256: "ti", Probes: []string{"schema-following"},
		Models: models, AgentImages: images,
	}
}

func TestCheckMergeable_DifferingRoleCoverageIsAllowed(t *testing.T) {
	const m, img = "Qwen/Qwen3.8-27B-FP8", "sha256:b06ab73a"
	a := armWithRoles(
		map[string]string{"lead": m, "coder": m},
		map[string]string{"lead": img, "coder": img})
	b := armWithRoles(
		map[string]string{"lead": m, "coder": m, "tester": m, "analyst": m},
		map[string]string{"lead": img, "coder": img, "tester": img, "analyst": img})

	if err := CheckMergeable(a, b); err != nil {
		t.Fatalf("batches differing only in role COVERAGE must merge: %v", err)
	}
}

// The safety property: a shared role whose MODEL differs is a different
// experiment and must still be refused. Losing this would let a mid-run model
// swap merge silently, which is what the comparability key exists to prevent.
func TestCheckMergeable_SharedRoleDisagreementIsRefused(t *testing.T) {
	const img = "sha256:b06ab73a"
	a := armWithRoles(
		map[string]string{"lead": "Qwen/Qwen3.8-27B-FP8"},
		map[string]string{"lead": img})
	b := armWithRoles(
		map[string]string{"lead": "zai.glm-5", "tester": "zai.glm-5"},
		map[string]string{"lead": img, "tester": img})

	if err := CheckMergeable(a, b); err == nil {
		t.Fatal("a shared role running a DIFFERENT model must refuse to merge")
	}
}

func TestCheckMergeable_SharedRoleImageDisagreementIsRefused(t *testing.T) {
	const m = "Qwen/Qwen3.8-27B-FP8"
	a := armWithRoles(map[string]string{"lead": m}, map[string]string{"lead": "sha256:aaa"})
	b := armWithRoles(map[string]string{"lead": m}, map[string]string{"lead": "sha256:bbb"})
	if err := CheckMergeable(a, b); err == nil {
		t.Fatal("a shared role on a DIFFERENT agent image must refuse to merge")
	}
}

// A non-role axis differing must still refuse — coverage tolerance must not
// leak into the axes that define the experiment.
func TestCheckMergeable_NonRoleAxisStillRefuses(t *testing.T) {
	const m, img = "Qwen/Qwen3.8-27B-FP8", "sha256:b06ab73a"
	a := armWithRoles(map[string]string{"lead": m}, map[string]string{"lead": img})
	b := armWithRoles(map[string]string{"lead": m}, map[string]string{"lead": img})
	b.ContextPolicy = "suppression=all;advert=off"
	if err := CheckMergeable(a, b); err == nil {
		t.Fatal("a differing context policy must refuse to merge")
	}
}

// The merged arm must carry the UNION, or the rolled-up figure would describe
// fewer roles than actually ran.
func TestUnionObservedRoles(t *testing.T) {
	const m, img = "Qwen/Qwen3.8-27B-FP8", "sha256:b06ab73a"
	a := armWithRoles(map[string]string{"lead": m}, map[string]string{"lead": img})
	b := armWithRoles(
		map[string]string{"lead": m, "tester": m},
		map[string]string{"lead": img, "tester": img})
	got := unionObservedRoles(a, b)
	if len(got.Models) != 2 || got.Models["tester"] != m {
		t.Errorf("models not unioned: %v", got.Models)
	}
	if len(got.AgentImages) != 2 || got.AgentImages["tester"] != img {
		t.Errorf("agent images not unioned: %v", got.AgentImages)
	}
}
