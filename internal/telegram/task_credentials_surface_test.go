package telegram

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

type fakeTaskCredRepo struct{ creds []*persistence.TaskCredential }

func (f *fakeTaskCredRepo) Upsert(context.Context, *persistence.TaskCredential) error { return nil }
func (f *fakeTaskCredRepo) ListByTaskLatestExecution(_ context.Context, taskID string) ([]*persistence.TaskCredential, error) {
	if taskID == "t1" {
		return f.creds, nil
	}
	return nil, nil
}

func TestRenderCredentialsSection(t *testing.T) {
	bot, _ := newBotWithRecorder(t, BotConfig{Token: "tok", AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}}})
	bot.taskCredentialRepo = &fakeTaskCredRepo{creds: []*persistence.TaskCredential{
		{TaskID: "t1", Label: "viewing password", Value: "hunter2-xY9pQ", ArtifactURL: "https://v/p/1"},
	}}

	// No repo / unknown task → empty.
	if s, allow := bot.renderCredentialsSection(context.Background(), &persistence.Task{ID: "other"}); s != "" || allow != nil {
		t.Fatalf("unknown task: got (%q,%v), want empty", s, allow)
	}

	s, allow := bot.renderCredentialsSection(context.Background(), &persistence.Task{ID: "t1"})
	if !strings.Contains(s, "viewing password: `hunter2-xY9pQ`") {
		t.Errorf("section missing backtick-wrapped credential: %q", s)
	}
	if !strings.Contains(s, "https://v/p/1") {
		t.Errorf("section missing artifact url: %q", s)
	}
	if len(allow) != 1 || string(allow[0]) != "hunter2-xY9pQ" {
		t.Errorf("allowlist = %v, want [hunter2-xY9pQ]", allow)
	}
}

// The credential must survive the outbound redactor (it's allowlisted) and
// render as tap-to-copy <code>, while an UN-allowlisted strong secret in the
// same message is still redacted.
func TestSendMessageGetIDAllow_CredentialSurvivesButStrongSecretRedacted(t *testing.T) {
	bot, rec := newBotWithRecorder(t, BotConfig{Token: "tok", AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}}})
	det, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatalf("detector: %v", err)
	}
	bot.SetSecretsDetector(det)

	text := "Password: `hunter2-xY9pQ`\nToken: sk-proj1234567890abcdefghijklmnopqrstuv"
	allow := [][]byte{[]byte("hunter2-xY9pQ")}
	if _, err := bot.sendMessageGetIDAllow(context.Background(), 42, text, allow); err != nil {
		t.Fatalf("send: %v", err)
	}
	snap := rec.snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 sent message, got %d", len(snap))
	}
	got := snap[0].Text
	if !strings.Contains(got, "<code>hunter2-xY9pQ</code>") {
		t.Errorf("credential should survive + render copyable, got: %q", got)
	}
	if strings.Contains(got, "sk-proj1234567890") {
		t.Errorf("un-allowlisted strong secret must be redacted, got: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:") {
		t.Errorf("expected a redaction marker for the strong secret, got: %q", got)
	}
}
