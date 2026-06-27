package idfmt

import "testing"

func TestShort(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"task ID", "task_20260518075825_d8f66e9c88d3fee9", "T-fee9"},
		{"exec ID", "exec_20260518080037_be319afc75233107", "X-3107"},
		{"execution alias", "execution_20260518_abcd1234", "X-1234"},
		{"tmsg ID", "tmsg_20260518_aabbccdd", "M-ccdd"},
		{"msg alias", "msg_20260518_aabbccdd", "M-ccdd"},
		{"epoch ID", "cep_20260518_aabbccdd", "E-ccdd"},
		{"artifact ID", "art_20260518_aabbccdd", "A-ccdd"},
		{"unknown prefix", "vornik_20260518_aabbccdd", "vornik_20260518_aabbccdd"},
		{"no underscore", "deadbeef", "deadbeef"},
		{"too short", "abc", "abc"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Short(tc.in)
			if got != tc.want {
				t.Errorf("Short(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
