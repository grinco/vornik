package service

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/slack"
)

func TestSlackChannelsSubsystem_StartUsesRebuiltHTTPChannels(t *testing.T) {
	var logs bytes.Buffer
	c := &Container{
		Logger:        zerolog.New(&logs),
		SlackChannels: []*slack.Channel{{}},
		SlackProjects: []*registry.Project{{ID: "first"}},
	}
	subsystem := NewSlackChannelsSubsystem()
	require.NoError(t, subsystem.Build(&BuildDeps{Container: c}))

	// initHTTPServer is called again after subsystem Build and replaces the
	// channels mounted on the live API server. Start must use that rebuilt
	// slice, not the first slice captured during Build.
	c.SlackChannels = []*slack.Channel{{}, {}}
	c.SlackProjects = []*registry.Project{{ID: "first"}, {ID: "second"}}
	require.NoError(t, subsystem.Start(withContainer(context.Background(), c)))

	assert.Contains(t, logs.String(), `"channels":2`)
}

func TestGitHubChannelSubsystem_StartBindsRebuiltHTTPChannel(t *testing.T) {
	orphaned := &github.Channel{}
	live := &github.Channel{}
	c := &Container{
		Logger:        zerolog.Nop(),
		Dispatcher:    &dispatcher.Agent{},
		GitHubChannel: orphaned,
	}
	subsystem := NewGitHubChannelSubsystem()
	require.NoError(t, subsystem.Build(&BuildDeps{Container: c}))

	// Model the second initHTTPServer replacing the first channel with the
	// distinct instance mounted on the live API server.
	c.GitHubChannel = live
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, subsystem.Start(withContainer(ctx, c)))

	require.Eventually(t, live.ReceiverBound, time.Second, time.Millisecond)
	assert.False(t, orphaned.ReceiverBound(), "discarded channel must remain unbound")
}

func TestSlackChannelsSubsystem_StartRejectsMismatchedProjectPairing(t *testing.T) {
	c := &Container{
		Logger:        zerolog.Nop(),
		Dispatcher:    &dispatcher.Agent{},
		SlackChannels: []*slack.Channel{{}, {}},
		SlackProjects: []*registry.Project{{ID: "only-project"}},
	}
	subsystem := NewSlackChannelsSubsystem()
	require.NoError(t, subsystem.Build(&BuildDeps{Container: c}))

	err := subsystem.Start(withContainer(context.Background(), c))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "channel/project")
}
