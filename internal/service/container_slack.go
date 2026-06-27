package service

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/slack"
	"vornik.io/vornik/internal/voice"
)

// buildSlackChannels scans the project registry for every project
// carrying a fully configured `slack` block and constructs one
// *slack.Channel per project. Returns aligned slices: channels[i]
// corresponds to projects[i]. Empty inputs (or no project with a
// slack block) return (nil, nil, nil) — the service container then
// leaves the inbound webhook handler unmounted entirely.
//
// Per-project routing: every enabled project gets its own channel
// instance pinned to its workspace (team_id). Each channel carries
// its own signing secret, bot token, and allowlists. The dispatcher
// receiver wired in container.go pins each channel to its project's
// ID so an inbound from project A can't accidentally invoke create_task
// against project B.
//
// Returned errors are operator-facing — empty signing-secret env var,
// duplicate team_id across projects, surface as daemon-boot failures
// so the misconfig is loud at startup rather than at the first
// delivery. A failure on one project's channel aborts the entire
// build so the operator sees the misconfig immediately; mirrors
// buildEmailChannels' posture.
//
// Multi-workspace deployments wire one project per workspace and one
// channel per project. The slack.Channel implementation itself
// supports multi-installation routing on a single channel (for an
// operator with a shared signing secret across workspaces), but the
// per-project YAML pattern keeps the boundary clean: project ⇔
// workspace ⇔ channel.
func buildSlackChannels(projects []*registry.Project, stt voice.STTProvider, tts voice.TTSProvider) ([]*slack.Channel, []*registry.Project, error) {
	var (
		channels []*slack.Channel
		picked   []*registry.Project
	)
	seenTeams := make(map[string]string)
	for _, p := range projects {
		if !p.Slack.Enabled() {
			continue
		}
		if existing, ok := seenTeams[strings.TrimSpace(p.Slack.TeamID)]; ok {
			return nil, nil, fmt.Errorf("project %q slack: duplicate team_id %q (already configured by project %q)",
				p.ID, p.Slack.TeamID, existing)
		}
		ch, err := buildSlackChannelForProject(p, stt, tts)
		if err != nil {
			return nil, nil, err
		}
		seenTeams[strings.TrimSpace(p.Slack.TeamID)] = p.ID
		channels = append(channels, ch)
		picked = append(picked, p)
	}
	if len(channels) == 0 {
		return nil, nil, nil
	}
	return channels, picked, nil
}

// buildSlackChannelForProject is the per-project constructor
// buildSlackChannels delegates to. Kept separate for testability so
// callers (tests, future per-project hot-reload paths) can build a
// single channel without iterating a registry.
func buildSlackChannelForProject(p *registry.Project, stt voice.STTProvider, tts voice.TTSProvider) (*slack.Channel, error) {
	cfg, err := resolveSlackConfig(p.Slack, p.ID)
	if err != nil {
		return nil, fmt.Errorf("project %q slack: %w", p.ID, err)
	}
	// Production HTTPClient comes from Go's stdlib default; tests
	// inject via the channel's Config seam directly (buildSlackChannelForProject
	// is bypassed by tests that need a stub Slack API server).
	cfg.HTTPClient = http.DefaultClient
	// Voice round-trip: the channel adapter is nil-safe per
	// direction, so a nil STT/TTS just keeps the pre-voice text-only
	// path active for that direction.
	if stt != nil || tts != nil {
		cfg.Voice = slack.VoiceProviders{STT: stt, TTS: tts}
	}
	ch, err := slack.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("project %q slack: %w", p.ID, err)
	}
	return ch, nil
}

// resolveSlackConfig translates a registry.ProjectSlack YAML block
// into the slack.Config the channel constructor consumes. Env-var
// indirected secrets are resolved here; string allowlists pass
// through verbatim. The translation is one-project-one-installation
// — the resolved Config uses the Installations field (singular
// entry) rather than the top-level "single-installation back-compat"
// fields, so the channel's multi-installation routing primitive
// runs the same code path in single- and multi-project deployments.
func resolveSlackConfig(p registry.ProjectSlack, projectID string) (slack.Config, error) {
	signingSecret := os.Getenv(p.SigningSecretEnv)
	if strings.TrimSpace(signingSecret) == "" {
		return slack.Config{}, fmt.Errorf("signing_secret_env %q is unset or empty", p.SigningSecretEnv)
	}
	cfg := slack.Config{
		SigningSecret:    signingSecret,
		TeamID:           strings.TrimSpace(p.TeamID),
		PostMessageRPS:   p.PostMessageRPS,
		PostMessageBurst: p.PostMessageBurst,
		Installations: []slack.InstallationConfig{
			{
				ProjectID:        projectID,
				TeamID:           strings.TrimSpace(p.TeamID),
				ChannelAllowlist: p.ChannelAllowlist,
				SenderAllowlist:  p.SenderAllowlist,
			},
		},
	}
	if envName := strings.TrimSpace(p.BotTokenEnv); envName != "" {
		botToken := os.Getenv(envName)
		if strings.TrimSpace(botToken) == "" {
			// An operator who set BotTokenEnv expected outbound; an
			// empty resolved value is almost certainly a missing-env
			// misconfig. Fail loudly at boot — matches buildEmailChannel's
			// SMTP-password check.
			return slack.Config{}, fmt.Errorf("bot_token_env %q is unset or empty", envName)
		}
		cfg.Installations[0].BotToken = botToken
	}
	return cfg, nil
}
