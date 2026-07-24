package config

import "testing"

func TestTaintLineageMode_Validation(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "advisory", false}, // empty ≡ advisory (observe-first default)
		{"advisory", "advisory", false},
		{"ADVISORY", "advisory", false}, // normalized
		{" off ", "off", false},         // trimmed
		{"enforce", "enforce", false},
		{"bogus", "", true}, // hard error, not silent advisory
		{"true", "", true},  // not a bool
		{"1", "", true},
	}
	for _, tc := range cases {
		got, err := TaintLineageConfig{EnforcementMode: tc.in}.TaintLineageMode()
		if tc.wantErr {
			if err == nil {
				t.Errorf("TaintLineageMode(%q): want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("TaintLineageMode(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("TaintLineageMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
