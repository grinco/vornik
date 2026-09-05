package supportbundle

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/version"
)

// stubRegistry is the deployed registry, narrowed to what the bundle reads.
type stubRegistry struct {
	workflows []*registry.Workflow
	swarms    []*registry.Swarm
	projects  []*registry.Project
}

func (s *stubRegistry) ListWorkflows() []*registry.Workflow { return s.workflows }
func (s *stubRegistry) ListSwarms() []*registry.Swarm       { return s.swarms }
func (s *stubRegistry) ListProjects() []*registry.Project   { return s.projects }

type stubWebhooks struct {
	events []*persistence.WebhookEvent
	err    error
}

func (s *stubWebhooks) List(context.Context, persistence.WebhookEventFilter) ([]*persistence.WebhookEvent, error) {
	return s.events, s.err
}

func supportTestDetector(t *testing.T) secrets.Detector {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:  secrets.EffectivePatterns(nil, nil),
		Allowlist: secrets.DefaultAllowlist(),
	})
	require.NoError(t, err)
	return det
}

// The prompt is the point. The bundle carried every execution row for a task
// and still could not explain the customer-reported forge-review failure,
// because the mechanism was a prompt telling the agent "the previous step
// provided the diff" while the executor sent nothing. Rows show what happened;
// the prompt shows what the agent was told to believe.
func TestBundle_CarriesTheDeployedRegistryIncludingPrompts(t *testing.T) {
	const prompt = "Review the change. The previous step provided the diff."
	b := &Builder{
		Detector: supportTestDetector(t),
		Registry: &stubRegistry{
			workflows: []*registry.Workflow{{
				ID:    "github-review",
				Steps: map[string]registry.WorkflowStep{"review": {Type: "agent", Role: "reviewer", Prompt: prompt}},
			}},
			swarms:   []*registry.Swarm{{ID: "dev-swarm", Roles: []registry.SwarmRole{{Name: "reviewer"}}}},
			projects: []*registry.Project{{ID: "headmatch", SwarmID: "dev-swarm"}},
		},
	}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.collectRegistry(Request{}, res)

	wf, ok := res.Files["registry/workflows.json"]
	require.True(t, ok, "the bundle must carry the deployed workflows")
	require.Contains(t, string(wf), prompt, "the step PROMPT must be in the bundle — it is the half of the diagnosis rows cannot show")
	require.Contains(t, string(res.Files["registry/swarms.json"]), "reviewer", "swarm roles must be carried")
	require.Contains(t, string(res.Files["registry/projects.json"]), "headmatch", "projects must be carried")
}

// A prompt is text like any other section: a credential pasted into one is
// redacted by the same detector, not exempted for being configuration.
func TestBundle_RedactsSecretsInPrompts(t *testing.T) {
	b := &Builder{
		Detector: supportTestDetector(t),
		Registry: &stubRegistry{workflows: []*registry.Workflow{{
			ID:    "wf",
			Steps: map[string]registry.WorkflowStep{"s": {Prompt: "deploy with AKIAIOSFODNN7EXAMPLE"}},
		}}},
	}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.collectRegistry(Request{}, res)

	got := string(res.Files["registry/workflows.json"])
	require.NotContains(t, got, "AKIAIOSFODNN7EXAMPLE", "a credential in a prompt must not leave the trust boundary")
	require.Contains(t, got, "[REDACTED:", "the redaction must be visible")
	require.Positive(t, res.Tally.Total, "the redaction must be counted, or REDACTION.txt understates what left")
}

// An absent registry omits the section rather than failing the bundle — the
// same degradation every repo gets.
func TestBundle_OmitsRegistryWhenAbsent(t *testing.T) {
	b := &Builder{Detector: supportTestDetector(t)}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.collectRegistry(Request{}, res)
	require.Empty(t, res.Files, "a nil registry must omit the section, not error")
}

// Webhook ingress: status and error code, never the payload or a message that
// can quote it.
func TestBundle_WebhookEventsCarryNoPayload(t *testing.T) {
	taskID := "task_1"
	b := &Builder{
		Detector: supportTestDetector(t),
		Webhooks: &stubWebhooks{events: []*persistence.WebhookEvent{{
			ID: "wh1", ProjectID: "headmatch", Source: "github", EventID: "e1",
			PayloadHash: "sha256:abc", Status: "rejected", TaskID: &taskID,
			ErrorCode:    "SIGNATURE_INVALID",
			ErrorMessage: "body was {\"secret\":\"hunter2\"}",
			CreatedAt:    time.Now().UTC(),
		}}},
	}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.collectWebhookEvents(context.Background(), Request{}, res)

	raw, ok := res.Files["webhook_events.json"]
	require.True(t, ok, "the ingress audit must be carried")
	got := string(raw)
	require.Contains(t, got, "SIGNATURE_INVALID", "the error CODE is the diagnostic value")
	require.Contains(t, got, "sha256:abc", "the payload hash correlates two reports of one event")
	require.NotContains(t, got, "hunter2",
		"error_message can quote the body, so it is dropped with the payload rather than scanned")

	var rows []map[string]any
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	for k := range rows[0] {
		require.False(t, strings.Contains(strings.ToLower(k), "payload") && k != "payload_hash",
			"no payload field may appear: %s", k)
	}
}

// A failing repository records the section error rather than failing the run.
func TestBundle_WebhookErrorIsRecordedNotFatal(t *testing.T) {
	b := &Builder{Detector: supportTestDetector(t), Webhooks: &stubWebhooks{err: context.DeadlineExceeded}}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.collectWebhookEvents(context.Background(), Request{}, res)
	require.Contains(t, res.SectionErrs, "webhook_events.json")
	require.NotContains(t, res.Files, "webhook_events.json")
}

// The registry is capped like every other list section, and the truncation is
// recorded — a support engineer reading a capped file must know it is capped
// (review 2026-09-04, finding f).
func TestBundle_RegistrySectionsAreBoundedAndSaySo(t *testing.T) {
	many := make([]*registry.Workflow, defaultRegistryCap+25)
	for i := range many {
		many[i] = &registry.Workflow{ID: "wf"}
	}
	b := &Builder{Detector: supportTestDetector(t), Registry: &stubRegistry{workflows: many}}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.collectRegistry(Request{}, res)

	var got []any
	require.NoError(t, json.Unmarshal(res.Files["registry/workflows.json"], &got))
	require.Len(t, got, defaultRegistryCap, "the section must be capped")
	require.Contains(t, res.Truncations, "registry/workflows.json",
		"a capped section must record that it was capped")
	require.Contains(t, res.Truncations["registry/workflows.json"], "525",
		"the note must say how many there were")
}

// REDACTION.txt publishes counts, and a count with no scope reads as a
// guarantee. The detector finds secrets, not business identifiers — and the
// registry section carries prompts verbatim apart from secrets, so the file
// that reports the counts must say what they do not cover (review 2026-09-04,
// finding d).
func TestBundle_RedactionSummaryStatesWhatItDoesNotCover(t *testing.T) {
	b := &Builder{Detector: supportTestDetector(t), Version: "test"}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.Finalize(Request{}, res)

	summary := string(res.Files["REDACTION.txt"])
	require.Contains(t, summary, "does NOT remove business identifiers")
	require.Contains(t, summary, "registry/",
		"the notice must name the section that carries prompts verbatim")
	require.Contains(t, summary, "before sending this archive",
		"it must tell the operator what to do about it")
}

// TestBundle_StatesVersionAndEditionExplicitly — operator requirement,
// 2026-09-04: the bundle must say which version AND which edition produced it.
//
// It is not decoration. Half this bundle's diagnostic surface exists in one
// edition and not the other — the admin endpoints, the blackbox trace, the EE
// providers — so an ABSENT section means "not built into this edition" on
// Community and "broken" on Enterprise. Those are opposite diagnoses from
// identical evidence, and a support engineer who has to infer the edition from
// what is missing is guessing at the thing that decides how to read the rest.
func TestBundle_StatesVersionAndEditionExplicitly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edition string
		want    string
	}{
		{"enterprise", version.EditionEnterprise, "enterprise"},
		{"community", version.EditionCommunity, "community"},
		// An unstamped or untrusted edition normalizes DOWN to community:
		// claiming the more-privileged edition on a build that may not be it
		// would misdirect the reader in the more dangerous direction.
		{"unstamped falls back to community", "", "community"},
		{"garbage falls back to community", "enterprise-ish", "community"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Builder{Detector: supportTestDetector(t), Version: "2026.9.1-71-gabc", Edition: tc.edition}
			res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
				Truncations: map[string]string{}, SectionErrs: map[string]string{}}
			b.collectVersion(Request{}, res)

			got := string(res.Files["version.txt"])
			require.Contains(t, got, "version: 2026.9.1-71-gabc")
			require.Contains(t, got, "edition: "+tc.want)
		})
	}
}

// The Manifest carries the edition too — it is what a tool reads, and a
// section missing from Files means opposite things in the two editions.
func TestBundle_ManifestCarriesTheEdition(t *testing.T) {
	b := &Builder{Detector: supportTestDetector(t), Version: "v1", Edition: version.EditionEnterprise}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.Finalize(Request{}, res)
	require.Equal(t, "enterprise", res.Manifest.VornikEdition)
	require.Equal(t, "v1", res.Manifest.VornikVersion)
}

// A bundle must be diffable: two collections from the SAME deployment differing
// only by map-iteration order cannot be compared, and the registry's List*
// methods walk a map. Caught 2026-09-04 by the structural-parity test, which
// found the two drivers agreeing on every byte of the registry except its
// order.
func TestBundle_RegistrySectionsAreOrderedByID(t *testing.T) {
	reg := &stubRegistry{workflows: []*registry.Workflow{
		{ID: "zulu"}, {ID: "alpha"}, {ID: "mike"},
	}}
	b := &Builder{Detector: supportTestDetector(t), Registry: reg}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.collectRegistry(Request{}, res)

	var got []struct {
		ID string
	}
	if err := json.Unmarshal(res.Files["registry/workflows.json"], &got); err != nil {
		t.Fatalf("workflows.json: %v", err)
	}
	want := []string{"alpha", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("got %d workflows, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("workflow %d = %q, want %q — the section must be ordered by ID", i, got[i].ID, w)
		}
	}
}

// A nil entry must not panic the collection: the bundle degrades, it never
// fails.
func TestBundle_RegistrySortSurvivesNilEntries(t *testing.T) {
	b := &Builder{Detector: supportTestDetector(t),
		Registry: &stubRegistry{workflows: []*registry.Workflow{nil, {ID: "a"}}}}
	res := &Result{Files: map[string][]byte{}, Tally: NewRedactionTally(),
		Truncations: map[string]string{}, SectionErrs: map[string]string{}}
	b.collectRegistry(Request{}, res)
	if _, ok := res.Files["registry/workflows.json"]; !ok {
		t.Error("a nil registry entry lost the section")
	}
}
