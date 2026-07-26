package service

import (
	"bytes"
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/slack"
)

func TestSlackChannelsSubsystem_StartUsesRebuiltHTTPChannels(t *testing.T) {
	var logs bytes.Buffer
	c := &Container{
		Logger:        zerolog.New(&logs),
		SlackChannels: []*slack.Channel{{}},
	}
	subsystem := NewSlackChannelsSubsystem()
	require.NoError(t, subsystem.Build(&BuildDeps{Container: c}))

	// initHTTPServer is called again after subsystem Build and replaces the
	// channels mounted on the live API server. Start must use that rebuilt
	// slice, not the first slice captured during Build.
	c.SlackChannels = []*slack.Channel{{}, {}}
	require.NoError(t, subsystem.Start(withContainer(context.Background(), c)))

	assert.Contains(t, logs.String(), `"channels":2`)
}

func TestGitHubChannelSubsystem_StartIgnoresDiscardedHTTPChannel(t *testing.T) {
	var logs bytes.Buffer
	c := &Container{
		Logger:        zerolog.New(&logs),
		GitHubChannel: &github.Channel{},
	}
	subsystem := NewGitHubChannelSubsystem()
	require.NoError(t, subsystem.Build(&BuildDeps{Container: c}))

	// Model the second initHTTPServer replacing/removing the handler after
	// Build. Start must not act on the orphaned channel from the first server.
	c.GitHubChannel = nil
	require.NoError(t, subsystem.Start(withContainer(context.Background(), c)))

	assert.Empty(t, logs.String())
}
