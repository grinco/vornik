package registry

import "testing"

// TestAllowsSpawnTemplate_ClosedByDefault asserts a project
// with no AllowSpawn block refuses every template. Secure
// default — spawn_project is opt-in.
func TestAllowsSpawnTemplate_ClosedByDefault(t *testing.T) {
	p := &Project{ID: "marketing"}
	for _, tmpl := range []string{"sales-campaign", "partner-onboarding", ""} {
		if p.AllowsSpawnTemplate(tmpl) {
			t.Errorf("AllowsSpawnTemplate(%q) on empty allowlist = true, want false", tmpl)
		}
	}
}

func TestAllowsSpawnTemplate_ExactAndGlob(t *testing.T) {
	p := &Project{ID: "marketing", AllowSpawn: ProjectAllowSpawn{
		Templates: []string{"sales-campaign", "partner-*"},
	}}
	for _, tmpl := range []string{"sales-campaign", "partner-onboarding", "partner-tier1"} {
		if !p.AllowsSpawnTemplate(tmpl) {
			t.Errorf("AllowsSpawnTemplate(%q) = false, want true", tmpl)
		}
	}
	for _, tmpl := range []string{"sales-other", "anything-else"} {
		if p.AllowsSpawnTemplate(tmpl) {
			t.Errorf("AllowsSpawnTemplate(%q) = true, want false", tmpl)
		}
	}
}

func TestAllowsSpawnTemplate_Wildcard(t *testing.T) {
	p := &Project{ID: "marketing", AllowSpawn: ProjectAllowSpawn{Templates: []string{"*"}}}
	for _, tmpl := range []string{"sales-campaign", "anything", "x"} {
		if !p.AllowsSpawnTemplate(tmpl) {
			t.Errorf("wildcard should allow %q", tmpl)
		}
	}
	if p.AllowsSpawnTemplate("") {
		t.Error("empty template name must be rejected even under wildcard")
	}
}

func TestAllowsSpawnTemplate_NilReceiver(t *testing.T) {
	var p *Project
	if p.AllowsSpawnTemplate("any") {
		t.Error("nil receiver should reject")
	}
}
