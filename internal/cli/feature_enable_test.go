package cli

import (
	"strings"
	"testing"
)

func TestRenderEnablePlan_WithChanges(t *testing.T) {
	plan := enablePlanDTO{
		FeatureID: "instinct",
		Apply:     "reload-hot",
		Changes: []gateChangeDTO{
			{Key: "instinct.enabled", From: false, To: true},
			{Key: "instinct.consumers.application_feedback", From: false, To: true},
		},
	}
	out := renderEnablePlan(plan)

	if !strings.Contains(out, "instinct") {
		t.Error("plan render must contain feature id")
	}
	if !strings.Contains(out, "reload-hot") {
		t.Error("plan render must contain apply mechanism")
	}
	if !strings.Contains(out, "instinct.enabled") {
		t.Error("plan render must contain changed key")
	}
	if !strings.Contains(out, "--apply") {
		t.Error("plan render must hint at --apply flag")
	}
}

func TestRenderEnablePlan_NoChanges(t *testing.T) {
	plan := enablePlanDTO{
		FeatureID: "auth",
		Apply:     "reload-hot",
		Changes:   nil,
	}
	out := renderEnablePlan(plan)
	if !strings.Contains(out, "no gate changes") {
		t.Error("empty changes must say no gate changes needed")
	}
}

func TestRenderEnableResult_OK(t *testing.T) {
	result := enableResultDTO{
		FeatureID: "auth",
		OK:        true,
		Detail:    "admin key present, auth enabled",
	}
	out := renderEnableResult(result)
	if !strings.Contains(strings.ToUpper(out), "OK") {
		t.Error("OK result must say OK")
	}
	if !strings.Contains(out, "auth") {
		t.Error("result must contain feature id")
	}
	if !strings.Contains(out, "admin key present") {
		t.Error("result must contain detail")
	}
}

func TestRenderEnableResult_Fail(t *testing.T) {
	result := enableResultDTO{
		FeatureID: "auth",
		OK:        false,
		Detail:    "verify: key still missing",
	}
	out := renderEnableResult(result)
	if !strings.Contains(out, "verify failed") && !strings.Contains(strings.ToLower(out), "fail") {
		t.Error("failed result must indicate failure")
	}
}

func TestRenderEnablePlan_EndsWithNewline(t *testing.T) {
	out := renderEnablePlan(enablePlanDTO{FeatureID: "f", Apply: "reload-hot"})
	if !strings.HasSuffix(out, "\n") {
		t.Error("plan render must end with newline")
	}
}

func TestRenderEnableResult_EndsWithNewline(t *testing.T) {
	out := renderEnableResult(enableResultDTO{FeatureID: "f", OK: true})
	if !strings.HasSuffix(out, "\n") {
		t.Error("result render must end with newline")
	}
}
