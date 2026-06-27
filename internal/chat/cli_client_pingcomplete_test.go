// Coverage for the CLIClient + CodexCLIClient delegating wrappers:
// Ping (--version subprocess), and Complete / CompleteWithTools /
// CompleteWithToolsStream which all funnel through invoke. These
// were 0%-covered because they each just call a private helper —
// landing the call paths needs an actual subprocess invocation.
//
// Strategy: use /bin/true / /bin/false to substitute for the real
// claude / codex binaries. /bin/true exits 0 → Ping success path,
// /bin/false exits 1 → Ping failure path. For Complete the parsed
// stream is garbage so the error branch fires; we assert the
// recordMetrics-on-error code path runs rather than the model's
// output.

package chat

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"
)

func skipIfNoUnixBinaries(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires /bin/true and /bin/false")
	}
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skipf("/bin/true not available: %v", err)
	}
	if _, err := os.Stat("/bin/false"); err != nil {
		t.Skipf("/bin/false not available: %v", err)
	}
}

func TestCLIClient_Ping_Success(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCLIClient("model-x", WithCLIBinary("/bin/true"))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping(/bin/true): got %v, want nil", err)
	}
}

func TestCLIClient_Ping_NonZeroExit(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCLIClient("model-x", WithCLIBinary("/bin/false"))
	err := c.Ping(context.Background())
	if err == nil {
		t.Error("Ping(/bin/false): got nil, want error")
	}
}

func TestCLIClient_Ping_BinaryMissing(t *testing.T) {
	c := NewCLIClient("model-x", WithCLIBinary("/this/does/not/exist"))
	err := c.Ping(context.Background())
	if err == nil {
		t.Error("Ping(missing-binary): got nil, want error")
	}
}

// Complete drives the full invoke pipeline. With /bin/true as the
// binary the subprocess produces no stream-json output, so the parse
// step errors out — we assert that the error path returns a non-nil
// error and that the invocation didn't panic.
func TestCLIClient_Complete_ErrorOnEmptyOutput(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCLIClient("model-x",
		WithCLIBinary("/bin/true"),
		WithCLITimeout(5*time.Second))
	_, err := c.Complete(context.Background(), []Message{
		{Role: "user", Content: "hello"},
	})
	if err == nil {
		t.Error("Complete with /bin/true must error (no stream-json output)")
	}
}

func TestCLIClient_CompleteWithTools_ErrorOnEmptyOutput(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCLIClient("model-x", WithCLIBinary("/bin/true"))
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "ping"}}}
	_, err := c.CompleteWithTools(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, tools)
	if err == nil {
		t.Error("CompleteWithTools with /bin/true must error")
	}
}

func TestCLIClient_CompleteWithToolsStream_ErrorOnEmptyOutput(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCLIClient("model-x", WithCLIBinary("/bin/true"))
	_, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Error("CompleteWithToolsStream with /bin/true must error")
	}
}

func TestCLIClient_Complete_ErrorOnNonZeroExit(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCLIClient("model-x", WithCLIBinary("/bin/false"))
	_, err := c.Complete(context.Background(), []Message{
		{Role: "user", Content: "x"},
	})
	if err == nil {
		t.Error("Complete with /bin/false must error")
	}
}

func TestCLIClient_Complete_EmptyMessagesRejected(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCLIClient("model-x", WithCLIBinary("/bin/true"))
	_, err := c.Complete(context.Background(), nil)
	if err == nil {
		t.Error("Complete with no messages must error")
	}
}

// --- CodexCLIClient delegators ------------------------------------------

func TestCodexCLI_Complete_ErrorOnEmptyOutput(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCodexCLIClient("gpt-5.4-mini",
		WithCodexBinary("/bin/true"),
		WithCodexTimeout(5*time.Second))
	_, err := c.Complete(context.Background(), []Message{
		{Role: "user", Content: "hi"},
	})
	if err == nil {
		t.Error("Codex Complete with /bin/true must error")
	}
}

func TestCodexCLI_CompleteWithTools_ErrorOnEmptyOutput(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCodexCLIClient("gpt-5.4-mini", WithCodexBinary("/bin/true"))
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "ping"}}}
	_, err := c.CompleteWithTools(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, tools)
	if err == nil {
		t.Error("Codex CompleteWithTools with /bin/true must error")
	}
}

func TestCodexCLI_CompleteWithToolsStream_ErrorOnEmptyOutput(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCodexCLIClient("gpt-5.4-mini", WithCodexBinary("/bin/true"))
	_, err := c.CompleteWithToolsStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Error("Codex CompleteWithToolsStream with /bin/true must error")
	}
}

func TestCodexCLI_Complete_ErrorOnNonZeroExit(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCodexCLIClient("gpt-5.4-mini", WithCodexBinary("/bin/false"))
	_, err := c.Complete(context.Background(), []Message{
		{Role: "user", Content: "x"},
	})
	if err == nil {
		t.Error("Codex Complete with /bin/false must error")
	}
}

func TestCodexCLI_Complete_EmptyMessagesRejected(t *testing.T) {
	skipIfNoUnixBinaries(t)
	c := NewCodexCLIClient("gpt-5.4-mini", WithCodexBinary("/bin/true"))
	_, err := c.Complete(context.Background(), nil)
	if err == nil {
		t.Error("Codex Complete with no messages must error")
	}
}
