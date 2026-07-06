package projectdoctor

import "testing"

func TestWorstOf(t *testing.T) {
	cases := []struct {
		in   []Status
		want Status
	}{
		{[]Status{StatusGreen, StatusGreen}, StatusGreen},
		{[]Status{StatusGreen, StatusYellow}, StatusYellow},
		{[]Status{StatusYellow, StatusRed}, StatusRed},
		{[]Status{StatusUnknown, StatusYellow}, StatusUnknown},
		{[]Status{StatusRed, StatusUnknown}, StatusRed},
		{[]Status{StatusNeutral, StatusGreen}, StatusGreen},
		{[]Status{StatusNeutral}, StatusNeutral},
		{nil, StatusNeutral},
	}
	for _, c := range cases {
		if got := WorstOf(c.in...); got != c.want {
			t.Errorf("WorstOf(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestComputeComplete(t *testing.T) {
	// Required red => incomplete.
	if ComputeComplete([]CheckResult{{Key: "a", Status: StatusRed, Required: true}}) {
		t.Error("required red must be incomplete")
	}
	// Required unknown => incomplete.
	if ComputeComplete([]CheckResult{{Key: "a", Status: StatusUnknown, Required: true}}) {
		t.Error("required unknown must be incomplete")
	}
	// Required yellow => complete (soft failure is acceptable).
	if !ComputeComplete([]CheckResult{{Key: "a", Status: StatusYellow, Required: true}}) {
		t.Error("required yellow must be complete")
	}
	// Non-required red => complete (e.g. smoke never blocks).
	if !ComputeComplete([]CheckResult{{Key: "smoke", Status: StatusRed, Required: false}}) {
		t.Error("non-required red must not block completeness")
	}
	// All green => complete.
	if !ComputeComplete([]CheckResult{{Key: "a", Status: StatusGreen, Required: true}}) {
		t.Error("all green must be complete")
	}
}
