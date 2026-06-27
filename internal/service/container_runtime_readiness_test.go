package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
)

// TestProbeBinaryRow_ConfiguredAndPresent — when the operator pins
// a path AND it exists + is executable, the row reports OK.
func TestProbeBinaryRow_ConfiguredAndPresent(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "fake-whisper")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	row := probeBinaryRow("Whisper binary", bin, []string{"whisper-cpp"})
	if !row.Configured {
		t.Error("Configured should be true for an explicit path")
	}
	if !row.OK {
		t.Errorf("OK should be true; got error %q", row.Error)
	}
	if row.Path != bin {
		t.Errorf("Path = %q, want %q", row.Path, bin)
	}
}

// TestProbeBinaryRow_ConfiguredButNotExecutable — a file that
// exists but lacks +x renders red with an explanatory error.
// Mirrors the operator-facing diagnostic from probeBinary in
// container_voice.go.
func TestProbeBinaryRow_ConfiguredButNotExecutable(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "fake-piper-noperm")
	if err := os.WriteFile(bin, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	row := probeBinaryRow("Piper binary", bin, nil)
	if row.OK {
		t.Error("OK should be false for non-executable file")
	}
	if !strings.Contains(row.Error, "not executable") {
		t.Errorf("error should mention non-executable; got %q", row.Error)
	}
}

// TestProbeBinaryRow_ConfiguredButMissing — clear error when the
// pinned path doesn't exist. Operators see "first voice call will
// fail" in the daemon log; the row carries the same signal.
func TestProbeBinaryRow_ConfiguredButMissing(t *testing.T) {
	row := probeBinaryRow("Whisper binary", "/nonexistent/vornik-test-whisper", nil)
	if row.OK {
		t.Error("OK should be false")
	}
	if !strings.Contains(row.Error, "not found") {
		t.Errorf("error should mention not found; got %q", row.Error)
	}
}

// TestProbeBinaryRow_FallsBackToPath — when configured is empty,
// the helper walks $PATH against the candidate list. Verified by
// dropping a fake on a controlled $PATH.
func TestProbeBinaryRow_FallsBackToPath(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "whisper-cpp")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp)
	row := probeBinaryRow("Whisper binary", "", []string{"whisper-cpp", "whisper-cli"})
	if row.Configured {
		t.Error("Configured should be false when no explicit path")
	}
	if !row.OK {
		t.Errorf("expected OK from PATH lookup; error=%q", row.Error)
	}
	if filepath.Base(row.Path) != "whisper-cpp" {
		t.Errorf("Path = %q, want a whisper-cpp resolution", row.Path)
	}
}

// TestProbeBinaryRow_NoPathHitNorConfigured — no path, no $PATH
// match → renders red with an actionable error.
func TestProbeBinaryRow_NoPathHitNorConfigured(t *testing.T) {
	t.Setenv("PATH", "/tmp/empty-vornik-test")
	row := probeBinaryRow("Whisper binary", "", []string{"nonexistent-bin-xyz"})
	if row.OK {
		t.Error("OK should be false")
	}
	if !strings.Contains(row.Error, "not found on $PATH") {
		t.Errorf("error should mention $PATH; got %q", row.Error)
	}
}

// TestProbeModelRow_HappyPath stats a real file and reports OK.
func TestProbeModelRow_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	model := filepath.Join(tmp, "ggml-small.bin")
	if err := os.WriteFile(model, []byte("fake gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	row := probeModelRow("Whisper model", model)
	if !row.OK {
		t.Errorf("expected OK; error=%q", row.Error)
	}
	if row.Path != model {
		t.Errorf("Path = %q, want %q", row.Path, model)
	}
}

// TestProbeModelRow_Empty — empty config renders a soft error so
// the row still surfaces in the table.
func TestProbeModelRow_Empty(t *testing.T) {
	row := probeModelRow("Whisper model", "")
	if row.OK {
		t.Error("OK should be false on empty path")
	}
	if !strings.Contains(row.Error, "empty in config") {
		t.Errorf("error should mention empty config; got %q", row.Error)
	}
}

// TestProbeModelRow_Directory — pointing the config at a directory
// (rare but happens) yields a clear error rather than a misleading
// OK from os.Stat alone.
func TestProbeModelRow_Directory(t *testing.T) {
	tmp := t.TempDir()
	row := probeModelRow("Whisper model", tmp)
	if row.OK {
		t.Error("OK should be false for directory")
	}
	if !strings.Contains(row.Error, "directory, not a file") {
		t.Errorf("error should mention directory; got %q", row.Error)
	}
}

// TestProbeFilesystemWritable_HappyPath touches and removes a probe
// file in a freshly-created tmpdir.
func TestProbeFilesystemWritable_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	writable, err := probeFilesystemWritable(tmp)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !writable {
		t.Error("expected writable")
	}
}

// TestProbeFilesystemWritable_EmptyPath — empty config is a clear
// error rather than a silent "false" with no explanation.
func TestProbeFilesystemWritable_EmptyPath(t *testing.T) {
	writable, err := probeFilesystemWritable("")
	if err == nil {
		t.Error("expected error for empty path")
	}
	if writable {
		t.Error("expected not writable")
	}
}

// TestProbeFilesystemWritable_MissingDir — operator points at a
// path that doesn't exist; the helper surfaces the os.Stat error
// verbatim.
func TestProbeFilesystemWritable_MissingDir(t *testing.T) {
	writable, err := probeFilesystemWritable("/nonexistent/vornik-test-artifacts")
	if err == nil {
		t.Error("expected stat error")
	}
	if writable {
		t.Error("expected not writable")
	}
}

// TestProbeFilesystemWritable_PathIsAFile — operator pointed at a
// file rather than a directory; the helper reports the mismatch.
func TestProbeFilesystemWritable_PathIsAFile(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "a-file")
	if err := os.WriteFile(bin, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writable, err := probeFilesystemWritable(bin)
	if err == nil {
		t.Error("expected error on file-not-directory")
	}
	if writable {
		t.Error("expected not writable")
	}
}

// TestRuntimeReadinessProbe_VoiceStatus_DisabledProvider — when
// neither STT nor TTS is configured, VoiceStatus returns empty
// Probes so the template renders the "voice disabled" placeholder.
func TestRuntimeReadinessProbe_VoiceStatus_DisabledProvider(t *testing.T) {
	cfg := &config.Config{}
	p := newRuntimeReadinessProbe(cfg)
	got := p.VoiceStatus(context.Background())
	if got.STTProvider != "" || got.TTSProvider != "" {
		t.Errorf("expected disabled providers; got %+v", got)
	}
	if len(got.Probes) != 0 {
		t.Errorf("expected zero probes; got %d", len(got.Probes))
	}
}

// TestRuntimeReadinessProbe_VoiceStatus_PopulatesProbes — configured
// providers populate 6 probe rows (3 STT + 3 TTS).
func TestRuntimeReadinessProbe_VoiceStatus_PopulatesProbes(t *testing.T) {
	cfg := &config.Config{}
	cfg.Voice.STT.Provider = "whisper-local"
	cfg.Voice.TTS.Provider = "piper"
	p := newRuntimeReadinessProbe(cfg)
	got := p.VoiceStatus(context.Background())
	if got.STTProvider != "whisper-local" || got.TTSProvider != "piper" {
		t.Errorf("provider names lost: %+v", got)
	}
	if len(got.Probes) != 6 {
		t.Errorf("expected 6 probe rows (3 STT + 3 TTS), got %d", len(got.Probes))
	}
}

// TestRuntimeReadinessProbe_StorageStatus_Filesystem stats a real
// tempdir as the artifacts path; expects Writable=true.
func TestRuntimeReadinessProbe_StorageStatus_Filesystem(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{}
	cfg.Storage.Backend = "filesystem"
	cfg.Storage.ArtifactsPath = tmp
	p := newRuntimeReadinessProbe(cfg)
	got := p.StorageStatus(context.Background())
	if got.Backend != "filesystem" {
		t.Errorf("Backend = %q, want filesystem", got.Backend)
	}
	if !got.FilesystemWritable {
		t.Errorf("expected writable; error=%q", got.FilesystemError)
	}
	if got.FilesystemPath != tmp {
		t.Errorf("FilesystemPath = %q, want %q", got.FilesystemPath, tmp)
	}
}

// TestRuntimeReadinessProbe_StorageStatus_S3_ReportsConfig — when
// Storage.Backend is s3 the probe surfaces the config values + the
// deferred-reachability-probe note. Pins that the page renders
// useful info even without a live S3 client.
func TestRuntimeReadinessProbe_StorageStatus_S3_ReportsConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.Backend = "s3"
	cfg.Storage.S3.Region = "us-east-1"
	cfg.Storage.S3.Bucket = "vornik-prod-artifacts"
	cfg.Storage.S3.Prefix = "prod"
	cfg.Storage.S3.UsePathStyle = true
	p := newRuntimeReadinessProbe(cfg)
	got := p.StorageStatus(context.Background())
	if got.Backend != "s3" {
		t.Errorf("Backend = %q, want s3", got.Backend)
	}
	if got.S3Region != "us-east-1" || got.S3Bucket != "vornik-prod-artifacts" || got.S3Prefix != "prod" {
		t.Errorf("S3 config not surfaced: %+v", got)
	}
	if !got.S3UsePathStyle {
		t.Error("S3UsePathStyle should be true")
	}
	if got.S3Reachable {
		t.Error("S3Reachable should be false until live probe lands")
	}
	if !strings.Contains(got.S3Error, "deferred") {
		t.Errorf("S3Error should explain the deferred probe; got %q", got.S3Error)
	}
}
