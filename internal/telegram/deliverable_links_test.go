package telegram

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestRenderDeliverableLinks_NoArtifactRepo — bot without an
// artifact repo emits no block. Defensive: the helper must never
// panic when the optional dependency is missing.
func TestRenderDeliverableLinks_NoArtifactRepo(t *testing.T) {
	bot := &Bot{config: BotConfig{WebUIBaseURL: "https://x"}}
	got := bot.renderDeliverableLinks(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p"})
	assert.Equal(t, "", got)
}

// TestRenderDeliverableLinks_NoArtifacts — empty result from the
// repo also produces no block. Operators don't want a "Produced
// files:" header when nothing was produced.
func TestRenderDeliverableLinks_NoArtifacts(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, nil
		},
	}
	bot := &Bot{artifactRepo: repo, config: BotConfig{WebUIBaseURL: "https://x"}}
	got := bot.renderDeliverableLinks(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p"})
	assert.Equal(t, "", got)
}

// TestRenderDeliverableLinks_FormatsLinksWithBaseURL — happy path:
// repo returns one OUTPUT artifact, the helper builds a project-
// scoped artifact-raw URL and appends the "Download:" line.
func TestRenderDeliverableLinks_FormatsLinksWithBaseURL(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "deliverable.md", ArtifactClass: persistence.ArtifactClassOutput},
			}, nil
		},
	}
	bot := &Bot{artifactRepo: repo, config: BotConfig{WebUIBaseURL: "https://vornik.example.com"}}
	got := bot.renderDeliverableLinks(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p"})
	assert.Contains(t, got, "Produced files:")
	assert.Contains(t, got, "Download: deliverable.md")
	assert.Contains(t, got, "https://vornik.example.com/ui/projects/p/artifacts/raw?path=deliverable.md")
	assert.NotContains(t, got, "shell access")
}

// TestRenderDeliverableLinks_NoBaseURLFallsBackToShellNotice —
// when WebUIBaseURL is unset, the operator sees the filename but
// the helper appends the "operator must have shell access" trailer
// so they don't think they're staring at a broken link.
func TestRenderDeliverableLinks_NoBaseURLFallsBackToShellNotice(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "deliverable.md", ArtifactClass: persistence.ArtifactClassOutput},
			}, nil
		},
	}
	bot := &Bot{artifactRepo: repo, config: BotConfig{WebUIBaseURL: ""}}
	got := bot.renderDeliverableLinks(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p"})
	assert.Contains(t, got, "Download: deliverable.md")
	assert.Contains(t, got, "shell access")
	assert.NotContains(t, got, "https://")
}

// TestRenderDeliverableLinks_SkipsNonOutputAndResponseDumps —
// only ArtifactClassOutput artifacts that aren't raw response.md
// dumps surface in the block; the operator-facing deliverable is
// the writer's product, not the debug transcript.
func TestRenderDeliverableLinks_SkipsNonOutputAndResponseDumps(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "internal.tmp", ArtifactClass: persistence.ArtifactClassIntermediate},
				{Name: "writer-response.md", ArtifactClass: persistence.ArtifactClassOutput},
				{Name: "deliverable.md", ArtifactClass: persistence.ArtifactClassOutput},
			}, nil
		},
	}
	bot := &Bot{artifactRepo: repo, config: BotConfig{WebUIBaseURL: "https://x"}}
	got := bot.renderDeliverableLinks(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p"})
	assert.Contains(t, got, "deliverable.md")
	assert.NotContains(t, got, "writer-response.md")
	assert.NotContains(t, got, "internal.tmp")
}

// TestRenderDeliverableLinks_NilBotOrTask — defensive: nil bot
// or nil task return empty string, no panic. The dispatcher
// wires the call from NotifyTaskCompleted, but tests construct
// bots in many shapes and the helper must never explode.
func TestRenderDeliverableLinks_NilBotOrTask(t *testing.T) {
	var bot *Bot
	assert.Equal(t, "", bot.renderDeliverableLinks(context.Background(),
		&persistence.Task{ID: "t1"}))

	bot = &Bot{}
	assert.Equal(t, "", bot.renderDeliverableLinks(context.Background(), nil))
}
