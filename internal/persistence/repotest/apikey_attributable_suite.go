package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunAPIKeyAttributableSuite pins ListAttributable on BOTH backends.
//
// The filter is the whole point: per-task keys are 574 of 585 rows on the
// reference host, so a surface offering them is unusable — which is how the
// filter was found, by an operator who could not use the picker it fed.
//
// The exclusion is a prefix COMPARISON, not LIKE, because
// TaskKeyNamePrefix ends in "_" and LIKE reads that as "any single
// character" — a LIKE pattern would also swallow a human key named
// "agent:taskforce". This suite pins that distinction on both drivers,
// because it is exactly the kind of thing one driver gets right by accident.
func RunAPIKeyAttributableSuite(t *testing.T, repo persistence.APIKeyRepository) {
	ctx := context.Background()
	project := uniqueID("proj")

	mk := func(name string, revoked bool) string {
		id := uniqueID("akey")
		k := &persistence.APIKey{
			ID: id, ProjectID: project, Name: name,
			KeyHash: uniqueID("hash"), KeyPrefix: "sk-vornik-xx",
			CreatedAt: time.Now().UTC(),
		}
		wantOK(t, "Create("+name+")", repo.Create(ctx, k))
		if revoked {
			wantOK(t, "Revoke("+name+")", repo.Revoke(ctx, id))
		}
		return id
	}

	human := mk(uniqueID("vadim/laptop"), false)
	revokedHuman := mk(uniqueID("slava/codex"), true)
	taskKey := mk(persistence.TaskKeyNamePrefix+uniqueID("t"), false)
	// The name LIKE would swallow: same prefix but for the "_" being a real
	// character rather than a wildcard.
	lookalike := mk("agent:taskforce-"+uniqueID("x"), false)

	got, err := repo.ListAttributable(ctx)
	wantOK(t, "ListAttributable", err)

	seen := map[string]bool{}
	for _, k := range got {
		seen[k.ID] = true
	}
	if !seen[human] {
		t.Error("a human key is missing; the attribution picker would not offer it")
	}
	if !seen[revokedHuman] {
		t.Error("a REVOKED human key is missing — §5.4 requires a revoked key to stay " +
			"claimable, so it must stay visible to attribute")
	}
	if seen[taskKey] {
		t.Error("a per-task key is offered for attribution; these are machine credentials " +
			"bound to one task and they dominate the table")
	}
	if !seen[lookalike] {
		t.Error("a key named 'agent:taskforce-…' was excluded: the filter is matching " +
			"TaskKeyNamePrefix as a LIKE pattern, where the trailing '_' is a wildcard")
	}
}
