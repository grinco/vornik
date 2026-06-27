package chat

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestReadLimited covers the tiny helper that bounds a single Read.
func TestReadLimited(t *testing.T) {
	// Short buffer.
	r := strings.NewReader("hello world")
	got, err := readLimited(r, 5)
	if err != nil && err != io.EOF {
		t.Fatalf("readLimited: %v", err)
	}
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("got %q", got)
	}

	// EOF returned.
	r2 := strings.NewReader("hi")
	got, _ = readLimited(r2, 10)
	if string(got) != "hi" {
		t.Errorf("got %q", got)
	}
}
