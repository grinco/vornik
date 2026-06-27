package featuredoctor

import "testing"

func ok() PrereqResult                { return PrereqResult{OK: true} }
func unmet(fixable bool) PrereqResult { return PrereqResult{OK: false, Fixable: fixable} }

func TestComputeStatus(t *testing.T) {
	verifyOK := PrereqResult{OK: true}
	verifyFail := PrereqResult{OK: false}
	cases := []struct {
		name    string
		gatesOn bool
		prereqs []PrereqResult
		verify  *PrereqResult
		want    Status
	}{
		{"off, all prereqs met", false, []PrereqResult{ok()}, nil, StatusReady},
		{"off, only fixable unmet", false, []PrereqResult{unmet(true)}, nil, StatusReady},
		{"off, unfixable unmet", false, []PrereqResult{unmet(false)}, nil, StatusBlocked},
		{"on, prereqs+verify ok", true, []PrereqResult{ok()}, &verifyOK, StatusOK},
		{"on, prereq unmet", true, []PrereqResult{unmet(true)}, &verifyOK, StatusDegraded},
		{"on, verify fails", true, []PrereqResult{ok()}, &verifyFail, StatusDegraded},
		{"on, verify nil (errored)", true, []PrereqResult{ok()}, nil, StatusDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeStatus(tc.gatesOn, tc.prereqs, tc.verify); got != tc.want {
				t.Fatalf("ComputeStatus(%v) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
