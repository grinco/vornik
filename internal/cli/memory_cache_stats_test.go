package cli

import "testing"

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KB"},
		{2 * 1024 * 1024, "2.0 MB"},
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
	}
	for _, c := range cases {
		got := humanBytes(c.in)
		if got != c.want {
			t.Errorf("humanBytes(%d): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCacheStatusLabel(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		rows       int64
		wantStatus string
		wantNote   string
	}{
		{"disabled", false, 0, "disabled", ""},
		{"enabled empty", true, 0, "enabled", "no rows yet"},
		{"enabled populated", true, 42, "enabled", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, note := cacheStatusLabel(c.enabled, c.rows)
			if status != c.wantStatus || note != c.wantNote {
				t.Errorf("cacheStatusLabel(%v, %d): got (%q, %q), want (%q, %q)",
					c.enabled, c.rows, status, note, c.wantStatus, c.wantNote)
			}
		})
	}
}

func TestDistinctModelsNote(t *testing.T) {
	if got := distinctModelsNote("", 1); got != "" {
		t.Errorf("single model should produce empty note, got %q", got)
	}
	if got := distinctModelsNote("", 3); got == "" {
		t.Error("multiple models should produce a warning note")
	}
	if got := distinctModelsNote("seed", 2); got == "" || got == "seed" {
		t.Errorf("expected prefix+suffix, got %q", got)
	}
}

func TestDistinctPurposesNote(t *testing.T) {
	if got := distinctPurposesNote("", 0); got != "" {
		t.Errorf("zero purposes → empty, got %q", got)
	}
	if got := distinctPurposesNote("", 2); got == "" {
		t.Error("non-zero should produce note")
	}
}
