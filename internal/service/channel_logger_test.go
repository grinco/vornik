package service

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/registry"
)

// Regression (2026-07-26): none of the conversation-channel builders set
// Config.Logger, so every channel ran on a zero-value zerolog.Logger — which
// discards every write instead of failing. Inbound email had been receiving
// attachments, auto-extracting them and writing chunks into project memory
// for weeks without a single line in journald, which made "is email ingest
// working?" unanswerable from the daemon's own logs (and made allowlist
// drops, parse failures and IMAP errors invisible too). Each builder must
// hand its channel a logger that actually writes, tagged with the channel
// kind and the owning project.

// assertLogged drains buf and fails unless the probe line and every wanted
// field is present.
func assertLogged(t *testing.T, buf *bytes.Buffer, want ...string) {
	t.Helper()
	got := buf.String()
	if got == "" {
		t.Fatal("channel logger wrote nothing — Config.Logger is the discarding zero value")
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("log line %q missing %q", got, w)
		}
	}
}

func TestBuildEmailChannels_WiresAWritingLogger(t *testing.T) {
	t.Setenv("EMAIL_PASS_LOGGER", "shhh")
	var buf bytes.Buffer
	p := buildEmailProject("logproj", "EMAIL_PASS_LOGGER")

	channels, _, err := buildEmailChannels([]*registry.Project{p}, nil, "", nil, zerolog.New(&buf))
	if err != nil {
		t.Fatalf("buildEmailChannels: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("channels = %d, want 1", len(channels))
	}

	channels[0].Logger().Info().Msg("probe")
	assertLogged(t, &buf, "probe", "email-channel", "logproj")
}

func TestBuildSlackChannels_WiresAWritingLogger(t *testing.T) {
	t.Setenv("SLACK_SIGNING_LOGGER", "shhh")
	var buf bytes.Buffer
	p := projectWithSlack("slackproj", "T_LOG", "SLACK_SIGNING_LOGGER", "")

	channels, _, err := buildSlackChannels([]*registry.Project{p}, nil, nil, zerolog.New(&buf))
	if err != nil {
		t.Fatalf("buildSlackChannels: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("channels = %d, want 1", len(channels))
	}

	channels[0].Logger().Info().Msg("probe")
	assertLogged(t, &buf, "probe", "slack-channel", "slackproj")
}

func TestBuildGitHubChannel_WiresAWritingLogger(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	var buf bytes.Buffer
	p := inboundOnlyProject("ghproj")

	ch, _, err := buildGitHubChannel([]*registry.Project{p}, zerolog.New(&buf))
	if err != nil {
		t.Fatalf("buildGitHubChannel: %v", err)
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}

	ch.Logger().Info().Msg("probe")
	assertLogged(t, &buf, "probe", "github-app-channel", "ghproj")
}

// The multi-installation GitHub channel serves several projects through one
// webhook handler, so it carries no single project_id — but it still has to
// log.
func TestBuildGitHubChannel_MultiInstall_WiresAWritingLogger(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	var buf bytes.Buffer
	pA, pB, _ := multiInstallProjectPair(t)

	ch, _, err := buildGitHubChannel([]*registry.Project{pA, pB}, zerolog.New(&buf))
	if err != nil {
		t.Fatalf("buildGitHubChannel: %v", err)
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}

	ch.Logger().Info().Msg("probe")
	assertLogged(t, &buf, "probe", "github-app-channel")
}

// channelLogger is the one place the component/project tagging lives, so the
// next channel that gets wired inherits the convention instead of inventing
// its own (or forgetting the logger entirely).
func TestChannelLogger_TagsKindAndProject(t *testing.T) {
	var buf bytes.Buffer
	emailLog := channelLogger(zerolog.New(&buf), "email", "proj-1")
	emailLog.Info().Msg("probe")
	assertLogged(t, &buf, "probe", `"component":"email-channel"`, `"project_id":"proj-1"`)

	buf.Reset()
	ghLog := channelLogger(zerolog.New(&buf), "github-app", "")
	ghLog.Info().Msg("probe")
	got := buf.String()
	if !strings.Contains(got, `"component":"github-app-channel"`) {
		t.Errorf("log line %q missing component", got)
	}
	if strings.Contains(got, "project_id") {
		t.Errorf("empty project id must not emit a project_id field, got %q", got)
	}
}
