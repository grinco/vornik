package service

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestMirrorOneFile_RejectsAbsoluteRelPath(t *testing.T) {
	sourceRoot := t.TempDir()
	sourceConfigsDir := filepath.Join(sourceRoot, "configs")
	if target, staged, _, err := mirrorOneFile(sourceRoot, sourceConfigsDir, "/tmp/evil.yaml", []byte("x"), zerolog.Nop()); err == nil {
		t.Fatalf("absolute rel path must be refused, got target=%q staged=%v", target, staged)
	} else if !strings.Contains(err.Error(), "safe relative path") {
		t.Fatalf("absolute rel path error = %v, want safe-relative refusal", err)
	}
}
