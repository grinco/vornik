// Package storage: artifactbackend.go owns the dispatch from
// config.StorageConfig.Backend to the concrete artifacts.FileBackend
// implementation. Keeping the picker here (rather than in
// internal/artifacts) keeps the artifacts package free of config-
// shape coupling — the artifacts.LocalBackend / s3.Backend factories
// take their own minimal Options structs, and this picker translates
// from YAML to those.
//
// Phase 4 of the storage abstraction plan ships filesystem (default)
// + s3. New backends register by adding a case to OpenArtifactBackend
// below.
package storage

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/artifacts/s3"
	"vornik.io/vornik/internal/config"
)

// OpenArtifactBackend constructs the FileBackend named by cfg.Backend.
// The returned backend MUST be Close()d by the caller when the daemon
// shuts down; the existing artifact Store wraps it and forwards Close
// to its FileBackend, so callers usually never see it directly.
//
// Default ("" / "local" / "filesystem") returns a LocalBackend rooted
// at cfg.ArtifactsPath. "s3" returns an s3.Backend configured from
// cfg.S3.
func OpenArtifactBackend(ctx context.Context, cfg config.StorageConfig) (artifacts.FileBackend, error) {
	switch cfg.NormalizedBackend() {
	case "filesystem":
		return artifacts.NewLocalBackend(cfg.ArtifactsPath)
	case "s3":
		opts := s3.Options{
			Endpoint:        cfg.S3.Endpoint,
			Region:          cfg.S3.Region,
			Bucket:          cfg.S3.Bucket,
			Prefix:          cfg.S3.Prefix,
			AccessKeyID:     cfg.S3.AccessKeyID,
			SecretAccessKey: cfg.S3.SecretAccessKey,
			UsePathStyle:    cfg.S3.UsePathStyle,
			ForceSSL:        cfg.S3.ResolveForceSSL(),
		}
		return s3.New(ctx, opts)
	default:
		return nil, fmt.Errorf("storage: unsupported artifact backend %q", cfg.Backend)
	}
}
