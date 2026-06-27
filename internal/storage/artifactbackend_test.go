package storage

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/artifacts/s3"
	"vornik.io/vornik/internal/config"
)

func TestOpenArtifactBackend_DefaultFilesystem(t *testing.T) {
	t.Parallel()
	cfg := config.StorageConfig{ArtifactsPath: t.TempDir()}
	b, err := OpenArtifactBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenArtifactBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	if _, ok := b.(*artifacts.LocalBackend); !ok {
		t.Fatalf("expected *artifacts.LocalBackend, got %T", b)
	}
}

func TestOpenArtifactBackend_FilesystemExplicit(t *testing.T) {
	t.Parallel()
	cfg := config.StorageConfig{Backend: "filesystem", ArtifactsPath: t.TempDir()}
	b, err := OpenArtifactBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenArtifactBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	if _, ok := b.(*artifacts.LocalBackend); !ok {
		t.Fatalf("expected *artifacts.LocalBackend, got %T", b)
	}
}

func TestOpenArtifactBackend_LocalAlias(t *testing.T) {
	t.Parallel()
	cfg := config.StorageConfig{Backend: "local", ArtifactsPath: t.TempDir()}
	b, err := OpenArtifactBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenArtifactBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	if _, ok := b.(*artifacts.LocalBackend); !ok {
		t.Fatalf("expected *artifacts.LocalBackend, got %T", b)
	}
}

func TestOpenArtifactBackend_S3_BuildsBackend(t *testing.T) {
	t.Parallel()
	// Pass explicit credentials so the SDK's default chain doesn't
	// try to discover them via IMDS during the test.
	cfg := config.StorageConfig{
		Backend: "s3",
		S3: config.S3StorageConfig{
			Region:          "us-east-1",
			Bucket:          "vornik-art",
			AccessKeyID:     "AKIATEST",
			SecretAccessKey: "secrettest",
		},
	}
	b, err := OpenArtifactBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenArtifactBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	if _, ok := b.(*s3.Backend); !ok {
		t.Fatalf("expected *s3.Backend, got %T", b)
	}
}

func TestOpenArtifactBackend_S3_MissingBucket(t *testing.T) {
	t.Parallel()
	cfg := config.StorageConfig{Backend: "s3", S3: config.S3StorageConfig{Region: "r"}}
	_, err := OpenArtifactBackend(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Fatalf("expected bucket error, got %v", err)
	}
}

func TestOpenArtifactBackend_Unsupported(t *testing.T) {
	t.Parallel()
	cfg := config.StorageConfig{Backend: "gcs"}
	_, err := OpenArtifactBackend(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported-backend error, got %v", err)
	}
}
