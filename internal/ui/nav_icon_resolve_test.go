package ui

import (
	"strings"
	"testing"
)

// TestNavIconResolve_CoversEveryNavModelIcon guards against the bare-icon
// regression: navIconResolve is a static name→template dispatcher (Go
// templates can't call a template by a runtime name), so every Icon a
// navModel area/dest references MUST have a case there or it renders blank
// and the nav "feels broken". This asserts each one resolves to an <svg>.
func TestNavIconResolve_CoversEveryNavModelIcon(t *testing.T) {
	s := NewServer()
	if s.templates == nil {
		t.Fatal("templates not parsed")
	}
	seen := map[string]bool{}
	for _, area := range navModel() {
		icons := []string{area.Icon}
		for _, d := range area.Dests {
			icons = append(icons, d.Icon)
		}
		for _, icon := range icons {
			if icon == "" || seen[icon] {
				continue
			}
			seen[icon] = true
			var b strings.Builder
			if err := s.templates.ExecuteTemplate(&b, "navIconResolve", icon); err != nil {
				t.Errorf("navIconResolve(%q): %v", icon, err)
				continue
			}
			if !strings.Contains(b.String(), "<svg") {
				t.Errorf("navIconResolve(%q) rendered no <svg> — missing a dispatcher case → bare nav icon", icon)
			}
		}
	}
}
