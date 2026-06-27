// Final round of one-shot coverage for the small leaf helpers in
// the chat package. Each lift is ~0.1pp on the package total but
// together get us to the 82% gate without touching the big network
// paths.

package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// --- contextOptions -----------------------------------------------------

func TestClient_ContextOptions_ZeroReturnsNil(t *testing.T) {
	c := NewClient("https://api.example.com", "k", "x")
	if got := c.contextOptions(); got != nil {
		t.Errorf("zero contextSize: got %v, want nil", got)
	}
}

func TestClient_ContextOptions_NonZeroReturnsMap(t *testing.T) {
	c := NewClient("https://api.example.com", "k", "x",
		WithContextSize(8192))
	got := c.contextOptions()
	if got == nil || got["num_ctx"] != 8192 {
		t.Errorf("contextOptions: got %v, want {num_ctx: 8192}", got)
	}
}

func TestClient_ContextOptions_NegativeTreatedAsZero(t *testing.T) {
	c := NewClient("https://api.example.com", "k", "x",
		WithContextSize(-1))
	if got := c.contextOptions(); got != nil {
		t.Errorf("negative contextSize: got %v, want nil", got)
	}
}

// --- claudeAccountInfoPath ----------------------------------------------

func TestClaudeAccountInfoPath_WithConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/swarm-claude")
	got := claudeAccountInfoPath()
	if got != "/tmp/swarm-claude/.claude.json" {
		t.Errorf("got %q, want /tmp/swarm-claude/.claude.json", got)
	}
}

func TestClaudeAccountInfoPath_WithoutConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	got := claudeAccountInfoPath()
	if got == "" {
		t.Error("got empty path")
	}
}

// --- defaultClaudeCredentialsPath ---------------------------------------

func TestDefaultClaudeCredentialsPath_WithConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/swarm-claude2")
	got := defaultClaudeCredentialsPath()
	if got != "/tmp/swarm-claude2/.credentials.json" {
		t.Errorf("got %q", got)
	}
}

func TestDefaultClaudeCredentialsPath_WithoutConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	got := defaultClaudeCredentialsPath()
	if got == "" {
		t.Error("got empty path")
	}
}

// --- truncateForLog -----------------------------------------------------

func TestTruncateForLog_LongString(t *testing.T) {
	s := strings.Repeat("a", 100)
	got := truncateForLog(s, 10)
	if len(got) <= 10 {
		t.Errorf("len: %d (should be >10 due to elipsis)", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected elipsis; got %q", got)
	}
}

func TestTruncateForLog_ShortString(t *testing.T) {
	if got := truncateForLog("hi", 10); got != "hi" {
		t.Errorf("got %q", got)
	}
}

func TestTruncateForLog_ZeroMaxReturnsInput(t *testing.T) {
	if got := truncateForLog("hi", 0); got != "hi" {
		t.Errorf("max=0: got %q", got)
	}
}

func TestTruncateForLog_NegativeMaxReturnsInput(t *testing.T) {
	if got := truncateForLog("hi", -1); got != "hi" {
		t.Errorf("max=-1: got %q", got)
	}
}

// --- mimeToImageFormat --------------------------------------------------

func TestMimeToImageFormat_AllSupported(t *testing.T) {
	cases := map[string]bedrocktypes.ImageFormat{
		"image/png":     bedrocktypes.ImageFormatPng,
		"image/jpeg":    bedrocktypes.ImageFormatJpeg,
		"image/jpg":     bedrocktypes.ImageFormatJpeg,
		"image/gif":     bedrocktypes.ImageFormatGif,
		"image/webp":    bedrocktypes.ImageFormatWebp,
		"  IMAGE/PNG  ": bedrocktypes.ImageFormatPng, // trims + lowercases
	}
	for in, want := range cases {
		got, ok := mimeToImageFormat(in)
		if !ok || got != want {
			t.Errorf("%q: got %v/%v, want %v/true", in, got, ok, want)
		}
	}
}

func TestMimeToImageFormat_Unsupported(t *testing.T) {
	if _, ok := mimeToImageFormat("image/bmp"); ok {
		t.Error("bmp should not be supported")
	}
	if _, ok := mimeToImageFormat("text/plain"); ok {
		t.Error("text/plain should not be supported")
	}
	if _, ok := mimeToImageFormat(""); ok {
		t.Error("empty mime should not be supported")
	}
}

// --- isAllToolResults ---------------------------------------------------

func TestIsAllToolResults_EmptyFalse(t *testing.T) {
	if got := isAllToolResults(nil); got {
		t.Error("nil blocks should report false")
	}
	if got := isAllToolResults([]bedrocktypes.ContentBlock{}); got {
		t.Error("empty blocks should report false")
	}
}

func TestIsAllToolResults_AllToolResults(t *testing.T) {
	blocks := []bedrocktypes.ContentBlock{
		&bedrocktypes.ContentBlockMemberToolResult{},
		&bedrocktypes.ContentBlockMemberToolResult{},
	}
	if !isAllToolResults(blocks) {
		t.Error("all-tool-result blocks should report true")
	}
}

func TestIsAllToolResults_MixedReturnsFalse(t *testing.T) {
	blocks := []bedrocktypes.ContentBlock{
		&bedrocktypes.ContentBlockMemberToolResult{},
		&bedrocktypes.ContentBlockMemberText{Value: "x"},
	}
	if isAllToolResults(blocks) {
		t.Error("mixed blocks should report false")
	}
}

// --- priority_queue.Less -----------------------------------------------

func TestCallHeap_LessByPriorityAndSeq(t *testing.T) {
	h := callHeap{
		{priority: 5, seq: 10},
		{priority: 1, seq: 20},
		{priority: 5, seq: 5},
	}
	// 1 priority beats anything bigger.
	if !h.Less(1, 0) {
		t.Error("priority 1 should rank before priority 5")
	}
	// Same priority, lower seq wins.
	if !h.Less(2, 0) {
		t.Error("same priority, lower seq should rank first")
	}
	if h.Less(0, 2) {
		t.Error("same priority, higher seq should NOT rank first")
	}
}

// --- models_list.Ping live path -----------------------------------------

func TestClient_Ping_LiveFetchOk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /v1/models lookup. The Ping uses ListModels which uses the
		// http client + URL set on Client.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m",
		WithHTTPClient(srv.Client()))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping live: got %v, want nil", err)
	}
}

func TestClient_Ping_LiveFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oops`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m",
		WithHTTPClient(srv.Client()))
	if err := c.Ping(context.Background()); err == nil {
		t.Error("Ping live 500: got nil, want error")
	}
}

// --- store.SaveNamedConversation error paths ----------------------------

func TestSaveNamedConversation_RejectsEmptyName(t *testing.T) {
	dir := t.TempDir()
	conv := NewConversation("c1", 32)
	conv.AddMessage(Message{Role: "user", Content: "hi"})
	// A name that sanitises to empty (only punctuation) is rejected.
	err := SaveNamedConversation(filepath.Join(dir, "s.json"), 100, "///", conv)
	if err == nil {
		t.Error("expected error for unsavable name, got nil")
	}
}

func TestSaveNamedConversation_HappyPath(t *testing.T) {
	dir := t.TempDir()
	conv := NewConversation("c1", 32)
	conv.AddMessage(Message{Role: "user", Content: "hi"})
	if err := SaveNamedConversation(filepath.Join(dir, "s.json"), 100, "good-name", conv); err != nil {
		t.Fatalf("SaveNamedConversation: %v", err)
	}
	// File was written.
	names, err := ListNamedSaves(filepath.Join(dir, "s.json"), 100)
	if err != nil {
		t.Fatalf("ListNamedSaves: %v", err)
	}
	if len(names) != 1 || names[0] != "good-name" {
		t.Errorf("names: got %v, want [good-name]", names)
	}
}

// Sanity helper to ensure the os import stays referenced.
func TestOSGetEnvHelper(t *testing.T) {
	_ = os.Getenv("CLAUDE_CONFIG_DIR")
}
