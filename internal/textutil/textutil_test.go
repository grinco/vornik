package textutil

import "testing"

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 3, "hel"},
		{"hello", 10, "hello"},
		{"hello", 0, ""},
		{"héllo", 2, "hé"}, // 'é' is 2 bytes; must not split
		{"日本語テスト", 3, "日本語"},
		{"", 5, ""},
	}
	for _, c := range cases {
		if got := TruncateRunes(c.s, c.n); got != c.want {
			t.Errorf("TruncateRunes(%q,%d)=%q want %q", c.s, c.n, got, c.want)
		}
	}
}

func TestTruncateBytes(t *testing.T) {
	cases := []struct {
		s    string
		max  int
		want string
	}{
		{"hello", 3, "hel"},
		{"hello", 10, "hello"},
		{"hello", 0, ""},
		{"héllo", 2, "h"},  // cutting mid-'é' (bytes 1-2) backs off to "h"
		{"héllo", 3, "hé"}, // 'é' fully fits at byte 3
		{"日本語", 4, "日"},    // each kanji is 3 bytes; 4 backs off to one
	}
	for _, c := range cases {
		if got := TruncateBytes(c.s, c.max); got != c.want {
			t.Errorf("TruncateBytes(%q,%d)=%q want %q", c.s, c.max, got, c.want)
		}
		if len(TruncateBytes(c.s, c.max)) > c.max && c.max > 0 {
			t.Errorf("TruncateBytes(%q,%d) exceeded byte cap", c.s, c.max)
		}
	}
}

func TestFlatten(t *testing.T) {
	cases := []struct{ s, want string }{
		{"  a\t b\n c ", "a b c"},
		{"single", "single"},
		{"   ", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := Flatten(c.s); got != c.want {
			t.Errorf("Flatten(%q)=%q want %q", c.s, got, c.want)
		}
	}
}
