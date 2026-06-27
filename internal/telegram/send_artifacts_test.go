// Coverage for sendArtifactsToWatchers — the function pulls
// artifacts from the repo, filters to OUTPUT-class non-response.md
// entries, and posts each via SendDocument to each watcher.

package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func TestSendArtifactsToWatchers_NoArtifacts(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, nil
		},
	}
	b := newBareTestBot(t, BotConfig{Token: "t"})
	WithArtifactRepository(repo)(b)

	// Just call; nothing to send → no panic, no error.
	b.sendArtifactsToWatchers(context.Background(), "task-1", []int64{100, 200})
}

func TestSendArtifactsToWatchers_ListError(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, errors_New("db down")
		},
	}
	b := newBareTestBot(t, BotConfig{Token: "t"})
	WithArtifactRepository(repo)(b)
	b.sendArtifactsToWatchers(context.Background(), "task-1", []int64{100})
	// no assertion: function is fire-and-forget; the test verifies
	// that the error branch doesn't panic.
}

func TestSendArtifactsToWatchers_SendsToEachWatcher(t *testing.T) {
	dir := t.TempDir()
	out1Path := filepath.Join(dir, "deliverable.md")
	if err := os.WriteFile(out1Path, []byte("# Result"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out2Path := filepath.Join(dir, "summary.txt")
	if err := os.WriteFile(out2Path, []byte("done"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// response-suffixed artifact must be skipped.
	respPath := filepath.Join(dir, "step-response.md")
	_ = os.WriteFile(respPath, []byte("noise"), 0o644)

	repo := &mocks.MockArtifactRepository{
		ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "deliverable.md", StoragePath: out1Path, ArtifactClass: persistence.ArtifactClassOutput},
				{Name: "summary.txt", StoragePath: out2Path, ArtifactClass: persistence.ArtifactClassOutput},
				{Name: "step-response.md", StoragePath: respPath, ArtifactClass: persistence.ArtifactClassOutput},
				{Name: "junk.txt", StoragePath: respPath, ArtifactClass: persistence.ArtifactClassInput},
			}, nil
		},
	}

	var sends atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, err := NewBot(BotConfig{Token: "tok"}, chatClient,
		WithArtifactRepository(repo),
		WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	b.baseURL = srv.URL

	b.sendArtifactsToWatchers(context.Background(), "task-1", []int64{100, 200})

	// 2 OUTPUT non-response artifacts × 2 watchers = 4 sends.
	if got := sends.Load(); got != 4 {
		t.Errorf("send count: got %d, want 4", got)
	}
}

func TestSendArtifactsToWatchers_SendDocumentErrorContinues(t *testing.T) {
	// One artifact, two watchers. The first watcher's send fails;
	// the second still gets the file.
	dir := t.TempDir()
	out := filepath.Join(dir, "x.md")
	_ = os.WriteFile(out, []byte("x"), 0o644)
	repo := &mocks.MockArtifactRepository{
		ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "x.md", StoragePath: out, ArtifactClass: persistence.ArtifactClassOutput},
			}, nil
		},
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, _ := NewBot(BotConfig{Token: "tok"}, chatClient,
		WithArtifactRepository(repo),
		WithHTTPClient(srv.Client()),
	)
	b.baseURL = srv.URL

	b.sendArtifactsToWatchers(context.Background(), "task-1", []int64{100, 200})
	// Both watchers were attempted.
	if got := hits.Load(); got != 2 {
		t.Errorf("attempts: got %d, want 2 (loop must continue past first error)", got)
	}
}

// helper that wraps errors.New without importing it (avoids
// shadowing the local sanitizeTelegramError test rig).
func errors_New(msg string) error {
	return &simpleErr{msg: msg}
}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// Lightweight sanity guard so we don't shadow strings/etc.
var _ = strings.HasPrefix
