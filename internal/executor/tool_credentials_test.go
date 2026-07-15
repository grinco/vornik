package executor

import (
	"context"
	"regexp"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// fakeCredRepo records Upserts for assertions.
type fakeCredRepo struct{ creds []*persistence.TaskCredential }

func (f *fakeCredRepo) Upsert(_ context.Context, c *persistence.TaskCredential) error {
	f.creds = append(f.creds, c)
	return nil
}
func (f *fakeCredRepo) ListByTaskLatestExecution(context.Context, string) ([]*persistence.TaskCredential, error) {
	return f.creds, nil
}

func newCredExecutor(t *testing.T, repo persistence.TaskCredentialRepository) *Executor {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	return &Executor{
		logger:             zerolog.Nop(),
		secretsDetector:    det,
		taskCredentialRepo: repo,
		toolCredentialMappings: []ToolCredentialMapping{{
			Tool:            "mcp__pagedrop__pagedrop_publish",
			CredentialField: "password",
			ArtifactField:   "url",
			Label:           "viewing password",
		}},
	}
}

func TestCaptureToolCredential(t *testing.T) {
	ctx := context.Background()
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}
	pagedrop := "mcp__pagedrop__pagedrop_publish"

	t.Run("top-level object captured with value + url", func(t *testing.T) {
		repo := &fakeCredRepo{}
		e := newCredExecutor(t, repo)
		e.captureToolCredential(ctx, task, exec, pagedrop, `{"password":"hunter2-xY9pQ","url":"https://v/p/1"}`)
		require.Len(t, repo.creds, 1)
		require.Equal(t, "hunter2-xY9pQ", repo.creds[0].Value)
		require.Equal(t, "https://v/p/1", repo.creds[0].ArtifactURL)
		require.Equal(t, "viewing password", repo.creds[0].Label)
		require.Equal(t, "e1", repo.creds[0].ExecutionID)
	})

	t.Run("content envelope unwrapped and captured", func(t *testing.T) {
		repo := &fakeCredRepo{}
		e := newCredExecutor(t, repo)
		out := `{"content":[{"type":"text","text":"{\"password\":\"env-pw-77\",\"url\":\"https://v/p/2\"}"}]}`
		e.captureToolCredential(ctx, task, exec, pagedrop, out)
		require.Len(t, repo.creds, 1)
		require.Equal(t, "env-pw-77", repo.creds[0].Value)
		require.Equal(t, "https://v/p/2", repo.creds[0].ArtifactURL)
	})

	t.Run("strong-pattern value is refused (denylist)", func(t *testing.T) {
		repo := &fakeCredRepo{}
		e := newCredExecutor(t, repo)
		// An OpenAI-key-shaped value must never be captured even from a
		// trusted tool + correct mapping.
		e.captureToolCredential(ctx, task, exec, pagedrop, `{"password":"sk-proj1234567890abcdefghijklmnopqrstuv","url":"https://v/p/3"}`)
		require.Empty(t, repo.creds, "strong-pattern value must be refused")
	})

	t.Run("non-matching tool ignored", func(t *testing.T) {
		repo := &fakeCredRepo{}
		e := newCredExecutor(t, repo)
		e.captureToolCredential(ctx, task, exec, "mcp__other__thing", `{"password":"x9y8z7q6"}`)
		require.Empty(t, repo.creds)
	})

	t.Run("adjacent prefix tool ignored", func(t *testing.T) {
		repo := &fakeCredRepo{}
		e := newCredExecutor(t, repo)
		e.captureToolCredential(ctx, task, exec, "mcp__pagedrop__pagedrop_publisher_evil", `{"password":"x9y8z7q6"}`)
		require.Empty(t, repo.creds, "tool credential trust must be exact or underscore-delimited")
	})

	t.Run("missing credential field: no capture", func(t *testing.T) {
		repo := &fakeCredRepo{}
		e := newCredExecutor(t, repo)
		e.captureToolCredential(ctx, task, exec, pagedrop, `{"url":"https://v/p/4"}`)
		require.Empty(t, repo.creds)
	})

	t.Run("malformed content-envelope JSON: no capture", func(t *testing.T) {
		repo := &fakeCredRepo{}
		e := newCredExecutor(t, repo)
		e.captureToolCredential(ctx, task, exec, pagedrop, `{"content":[{"type":"text","text":"not json"}]}`)
		require.Empty(t, repo.creds)
	})

	t.Run("no mapping / nil repo: no-op", func(_ *testing.T) {
		e := &Executor{logger: zerolog.Nop()}
		e.captureToolCredential(ctx, task, exec, pagedrop, `{"password":"abc12345"}`) // must not panic
	})
}

// TestCaptureToolCredential_TextPattern covers the PageDrop-style case: the
// tool returns human-readable prose, and a regexp extracts the credential + URL.
func TestCaptureToolCredential_TextPattern(t *testing.T) {
	ctx := context.Background()
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}
	pagedrop := "mcp__pagedrop__pagedrop_publish_doc"

	repo := &fakeCredRepo{}
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	e := &Executor{
		logger:             zerolog.Nop(),
		secretsDetector:    det,
		taskCredentialRepo: repo,
		toolCredentialMappings: []ToolCredentialMapping{{
			Tool:   "mcp__pagedrop__pagedrop_publish",
			CredRE: regexp.MustCompile(`Password:\s*(\S+)`),
			ArtRE:  regexp.MustCompile(`View:\s*(\S+)`),
			Label:  "viewing password",
		}},
	}

	// The exact real shape observed from pagedrop_publish output.
	out := `Published page: "EuroBrussels Job Scan".
View: https://v.example.cc/p/eurobrussels-54ee31
Shared with anyone in your organization who has the link.
Password: wool4keg.stir.lend — share this separately from the link (shown only once).`
	e.captureToolCredential(ctx, task, exec, pagedrop, out)

	require.Len(t, repo.creds, 1)
	require.Equal(t, "wool4keg.stir.lend", repo.creds[0].Value)
	require.Equal(t, "https://v.example.cc/p/eurobrussels-54ee31", repo.creds[0].ArtifactURL)
	require.Equal(t, "viewing password", repo.creds[0].Label)
}

func TestLookupDottedPath(t *testing.T) {
	obj := map[string]any{
		"data": map[string]any{"credentials": map[string]any{"password": "deep-pw"}},
		"n":    float64(42),
		"flag": true,
	}
	if v, ok := lookupDottedPath(obj, "data.credentials.password"); !ok || v != "deep-pw" {
		t.Errorf("nested = (%q,%v), want deep-pw", v, ok)
	}
	if v, ok := lookupDottedPath(obj, "n"); !ok || v != "42" {
		t.Errorf("number = (%q,%v), want 42", v, ok)
	}
	if _, ok := lookupDottedPath(obj, "data.missing"); ok {
		t.Error("missing key should return false")
	}
	if _, ok := lookupDottedPath(obj, "data.credentials.password.extra"); ok {
		t.Error("descending past a scalar should return false")
	}
}
