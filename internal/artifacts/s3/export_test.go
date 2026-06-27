package s3

// export_test.go exposes a couple of internal hooks for tests living
// in the external s3_test package (e.g. contract_test.go,
// integration_test.go). Production callers never see these — Go's
// _test.go convention keeps them out of the regular build.

// NewWithClient is the test-only constructor that bypasses
// buildClient so external tests can drop in an in-memory s3API stub
// (or a real MinIO-backed *awss3.Client). Mirrors the package-internal
// newWithClient.
func NewWithClient(opts Options, client S3API) *Backend {
	return newWithClient(opts, client)
}

// S3API is the exported alias of the package-internal s3API interface
// — needed because external tests can't reference unexported types.
type S3API = s3API
