package mcp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestToolResultTextConcatenatesTextItems(t *testing.T) {
	result := &ToolResult{Content: []ContentItem{
		{Type: "text", Text: "hello"},
		{Type: "image", Text: "ignored"},
		{Text: " world"},
	}}

	if got := result.Text(); got != "hello world" {
		t.Fatalf("Text() = %q, want %q", got, "hello world")
	}
}

func TestJSONRPCErrorAndLogWriter(t *testing.T) {
	rpcErr := &jsonRPCError{Code: -32601, Message: "method not found"}
	if got := rpcErr.Error(); got != "method not found" {
		t.Fatalf("Error() = %q", got)
	}

	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	writer := &logWriter{logger: logger, server: "broker"}

	n, err := writer.Write([]byte("  broker ready  \n"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len("  broker ready  \n") {
		t.Fatalf("Write() bytes = %d", n)
	}
	if !strings.Contains(buf.String(), "broker ready") || !strings.Contains(buf.String(), "broker") {
		t.Fatalf("log output missing message/server: %q", buf.String())
	}

	buf.Reset()
	n, err = writer.Write([]byte(" \n\t "))
	if err != nil || n != len(" \n\t ") {
		t.Fatalf("blank Write() = %d, %v", n, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("blank Write() should not log, got %q", buf.String())
	}
}
