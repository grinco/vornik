package featuredoctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestComposerFeature_RegisteredInRegistry(t *testing.T) {
	found := false
	for _, f := range Registry() {
		if f.ID == "composer" {
			found = true
			if f.Edition != "enterprise" {
				t.Errorf("expected Edition enterprise, got %q", f.Edition)
			}
			if f.DocRef == "" {
				t.Error("expected a non-empty DocRef")
			}
			if len(f.Gates) != 1 || f.Gates[0].Key != "composer.enabled" {
				t.Errorf("expected a single composer.enabled gate, got %+v", f.Gates)
			}
		}
	}
	if !found {
		t.Fatal("composer feature not registered")
	}
}

func writeArchetype(t *testing.T, dir, id string, valid bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "role-library"), 0o755); err != nil {
		t.Fatal(err)
	}
	modelTier := "standard"
	if !valid {
		// An invalid modelTier is a SeverityError finding attributed
		// to this archetype (rolelibrary.CheckLibrary), without
		// breaking the YAML parse itself.
		modelTier = "nonsense"
	}
	body := "---\narchetypeId: " + id + "\ndisplayName: \"" + id + "\"\ntools: [\"file_read\"]\nrequiredOutputKeys: [\"summary\"]\nruntime: { cpu: \"1\", memory: \"1Gi\", maxTokens: 2048 }\nmodelTier: " + modelTier + "\n---\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(dir, "role-library", id+".md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRoleLibraryPrereq(t *testing.T) {
	t.Run("no configDir", func(t *testing.T) {
		res := checkRoleLibraryPrereq(Deps{})
		if res.OK {
			t.Error("expected not-ok with no RoleLibraryDir wired")
		}
	})

	t.Run("empty library", func(t *testing.T) {
		dir := t.TempDir()
		res := checkRoleLibraryPrereq(Deps{RoleLibraryDir: dir})
		if res.OK {
			t.Error("expected not-ok with zero archetypes")
		}
	})

	t.Run("one valid archetype", func(t *testing.T) {
		dir := t.TempDir()
		writeArchetype(t, dir, "researcher", true)
		res := checkRoleLibraryPrereq(Deps{RoleLibraryDir: dir})
		if !res.OK {
			t.Errorf("expected ok with one valid archetype, got %+v", res)
		}
	})

	t.Run("all archetypes broken", func(t *testing.T) {
		dir := t.TempDir()
		// modelTier invalid -> a SeverityError finding for every archetype.
		writeArchetype(t, dir, "broken", false)
		res := checkRoleLibraryPrereq(Deps{RoleLibraryDir: dir})
		if res.OK {
			t.Errorf("expected not-ok when every archetype has errors, got %+v", res)
		}
	})

	t.Run("mixed: one clean survives a broken sibling", func(t *testing.T) {
		dir := t.TempDir()
		writeArchetype(t, dir, "researcher", true)
		writeArchetype(t, dir, "broken", false)
		res := checkRoleLibraryPrereq(Deps{RoleLibraryDir: dir})
		if !res.OK {
			t.Errorf("expected ok — one archetype is clean, got %+v", res)
		}
	})
}

func TestChatProviderConfigured(t *testing.T) {
	cases := []struct {
		name string
		vals map[string]any
		want bool
	}{
		{"router family", map[string]any{"chat.provider": "router"}, true},
		{"claude-cli family", map[string]any{"chat.provider": "claude-cli"}, true},
		{"http with endpoint+model", map[string]any{"chat.provider": "http", "chat.endpoint": "http://x", "chat.model": "m"}, true},
		{"http missing model", map[string]any{"chat.provider": "http", "chat.endpoint": "http://x"}, false},
		{"empty provider missing endpoint", map[string]any{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Deps{Config: stubConfig{vals: c.vals}}
			if got := chatProviderConfigured(d); got != c.want {
				t.Errorf("chatProviderConfigured(%v) = %v, want %v", c.vals, got, c.want)
			}
		})
	}
	if chatProviderConfigured(Deps{}) {
		t.Error("nil Config must report not configured")
	}
}

func TestComposerFeature_Prereqs_Names(t *testing.T) {
	f := composerFeature()
	names := map[string]bool{}
	for _, p := range f.Prereqs {
		names[p.Name] = true
	}
	for _, want := range []string{"chat provider configured", "role library has at least one valid archetype", "wizard v2 present"} {
		if !names[want] {
			t.Errorf("missing prereq %q", want)
		}
	}
}

func TestComposerFeature_WizardV2Prereq_AlwaysOK(t *testing.T) {
	f := composerFeature()
	for _, p := range f.Prereqs {
		if p.Name == "wizard v2 present" {
			res := p.Check(context.Background(), Deps{})
			if !res.OK {
				t.Errorf("expected wizard v2 prereq to always be OK, got %+v", res)
			}
			return
		}
	}
	t.Fatal("prereq not found")
}
