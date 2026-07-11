package projectdoctor

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

type fakeSecrets map[string]bool

func (f fakeSecrets) Has(name string) bool { return f[name] }

func TestCheckSecrets(t *testing.T) {
	// No declared secrets => neutral, not required.
	d := New(Deps{Secrets: fakeSecrets{}})
	none := &registry.Project{}
	if got := d.checkSecrets(none); got.Status != StatusNeutral || got.Required {
		t.Fatalf("no secrets: got %+v", got)
	}
	// All present => green with per-secret items.
	d = New(Deps{Secrets: fakeSecrets{"GITHUB_TOKEN": true, "SLACK_TOKEN": true}})
	proj := &registry.Project{ID: "proj-1", Permissions: registry.ProjectPermissions{
		Secrets: []string{"GITHUB_TOKEN", "SLACK_TOKEN"},
	}}
	got := d.checkSecrets(proj)
	if got.Status != StatusGreen || !got.Required || len(got.Items) != 2 {
		t.Fatalf("all present: got %+v", got)
	}
	// FixHref deep-links into the Guided Integrations Hub, pre-scoped to
	// this project (task 5.4, design §5.7) — regardless of outcome, since
	// most declared secrets are channel credentials the hub now writes.
	if got.FixHref != "/ui/integrations?project=proj-1" {
		t.Fatalf("FixHref = %q, want /ui/integrations?project=proj-1", got.FixHref)
	}
	// One missing => red, that item red.
	d = New(Deps{Secrets: fakeSecrets{"GITHUB_TOKEN": true}})
	got = d.checkSecrets(proj)
	if got.Status != StatusRed {
		t.Fatalf("one missing: got %+v", got)
	}
	var slack CheckItem
	for _, it := range got.Items {
		if it.Name == "SLACK_TOKEN" {
			slack = it
		}
	}
	if slack.Status != StatusRed {
		t.Fatalf("missing secret item must be red: %+v", slack)
	}
	// Nil reader => unknown.
	d = New(Deps{})
	if got := d.checkSecrets(proj); got.Status != StatusUnknown {
		t.Fatalf("nil reader: got %+v", got)
	}
}
