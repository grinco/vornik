package s3_test

import (
	"context"
	"os"
	"testing"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/artifacts/backendtest"
	"vornik.io/vornik/internal/artifacts/s3"
)

// TestS3Backend_Integration_MinIO runs the FileBackend contract suite
// against a real MinIO instance. CI skips it by default (CI doesn't
// have to spin up MinIO); operators verifying a new SDK version
// against a known-good server invoke it with:
//
//	VORNIK_S3_INTEGRATION=1 \
//	VORNIK_S3_ENDPOINT=http://localhost:9000 \
//	VORNIK_S3_REGION=us-east-1 \
//	VORNIK_S3_BUCKET=vornik-art-test \
//	VORNIK_S3_ACCESS_KEY_ID=minioadmin \
//	VORNIK_S3_SECRET_ACCESS_KEY=minioadmin \
//	go test ./internal/artifacts/s3/... -run Integration -v
//
// The bucket MUST exist before running the test — vornik does not
// auto-create buckets. The recommended MinIO recipe is in
// https://docs.vornik.io
//
// Note: this test is intentionally NOT a Go build-tag because we
// want the suite to compile + be discoverable on every developer
// machine; the env-var gate keeps it as a one-line opt-in.
func TestS3Backend_Integration_MinIO(t *testing.T) {
	if os.Getenv("VORNIK_S3_INTEGRATION") != "1" {
		t.Skip("VORNIK_S3_INTEGRATION=1 not set; skipping MinIO integration suite")
	}
	endpoint := os.Getenv("VORNIK_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("VORNIK_S3_ENDPOINT is required for integration suite")
	}
	bucket := os.Getenv("VORNIK_S3_BUCKET")
	if bucket == "" {
		t.Fatal("VORNIK_S3_BUCKET is required for integration suite")
	}
	region := os.Getenv("VORNIK_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}

	opts := s3.Options{
		Endpoint:        endpoint,
		Region:          region,
		Bucket:          bucket,
		Prefix:          "vornik-contract-test/", // namespaced so test cleanup is bounded
		AccessKeyID:     os.Getenv("VORNIK_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("VORNIK_S3_SECRET_ACCESS_KEY"),
		UsePathStyle:    true,  // MinIO requires path-style
		ForceSSL:        false, // localhost dev typically over HTTP
	}

	backendtest.Run(t, func(t *testing.T) (artifacts.FileBackend, func()) {
		b, err := s3.New(context.Background(), opts)
		if err != nil {
			t.Fatalf("s3.New: %v", err)
		}
		// Each subtest gets a unique scoped prefix to avoid races
		// across parallel runs.
		scopedOpts := opts
		scopedOpts.Prefix = opts.Prefix + t.Name()
		scopedB, err := s3.New(context.Background(), scopedOpts)
		if err != nil {
			t.Fatalf("s3.New scoped: %v", err)
		}
		_ = b // pre-flight check: top-level backend constructs OK
		return scopedB, func() {
			// Best-effort cleanup: list and delete everything under
			// the scoped prefix. Failures are logged but don't fail
			// the test — the next run will overwrite or expire.
			ctx := context.Background()
			_ = scopedB.List(ctx, "", func(o artifacts.ObjectInfo) error {
				_ = scopedB.Delete(ctx, o.Key)
				return nil
			})
			_ = scopedB.Close()
		}
	})
}
