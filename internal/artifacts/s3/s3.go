// Package s3 implements the artifacts.FileBackend interface against
// any S3-compatible object store (AWS S3, MinIO, Ceph RGW, …) via
// github.com/aws/aws-sdk-go-v2. The package is built incrementally
// across phase-4 slices:
//
//   - Slice 1: Options + Backend skeleton.
//   - Slice 2: SDK client construction (config + credentials).
//   - Slice 3 (current): Put/Get/Delete/Exists/Stat over the SDK.
//   - Slice 4: List with continuation-token pagination.
//   - Slice 5: contract test parity + MinIO integration test.
//
// The package lives under internal/artifacts/ rather than inside
// internal/artifacts itself so the s3 SDK's import graph stays out
// of the LocalBackend test build — operators with a filesystem-only
// deployment don't pay for the SDK at all.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"vornik.io/vornik/internal/artifacts"
)

// Options carries every knob the S3 backend needs. The storage
// factory (internal/storage/artifactbackend.go) builds this from
// config.S3StorageConfig.
type Options struct {
	// Endpoint is the S3 endpoint URL. Empty uses the SDK default
	// (AWS regional endpoints).
	Endpoint string
	// Region is the AWS region. Required even for MinIO.
	Region string
	// Bucket is the S3 bucket holding all artifacts.
	Bucket string
	// Prefix optionally namespaces keys so one bucket can host
	// multiple vornik deployments.
	Prefix string
	// AccessKeyID / SecretAccessKey override the SDK's credential
	// chain. Leave empty to use IAM role, ~/.aws, AWS_ACCESS_KEY_ID.
	AccessKeyID     string
	SecretAccessKey string
	// UsePathStyle forces path-style addressing (required for MinIO).
	UsePathStyle bool
	// ForceSSL requires https. Defaults true at the config layer; the
	// caller must explicitly pass false for MinIO localhost dev.
	ForceSSL bool
}

// Validate checks the minimum fields for the S3 backend to function.
// Bucket and Region are non-negotiable; the credentials may be empty
// (the SDK falls back to its default chain).
func (o Options) Validate() error {
	if o.Bucket == "" {
		return errors.New("artifacts/s3: bucket is required")
	}
	if o.Region == "" {
		return errors.New("artifacts/s3: region is required")
	}
	return nil
}

// ErrNotImplemented is kept exported for callers that want to assert
// "the operator picked a backend whose method I just called isn't
// wired yet". After slice 4 lands no method returns it, but the
// symbol stays for forward compatibility with future backends added
// to the same package (e.g. an Azure Blob driver would follow the
// same incremental-slice pattern).
var ErrNotImplemented = errors.New("artifacts/s3: not implemented")

// s3API is the minimum interface from awss3.Client that Backend uses.
// Defining it explicitly lets unit tests stub the network entirely;
// the real client (*awss3.Client) implements this trivially because
// every method here matches its native signature.
type s3API interface {
	PutObject(ctx context.Context, in *awss3.PutObjectInput, opts ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *awss3.GetObjectInput, opts ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, in *awss3.DeleteObjectInput, opts ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error)
	HeadObject(ctx context.Context, in *awss3.HeadObjectInput, opts ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *awss3.ListObjectsV2Input, opts ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error)
}

// Backend is the S3 FileBackend. It holds a configured s3API and
// applies the configured prefix to every key it touches.
type Backend struct {
	opts   Options
	client s3API
	// prefix is the cleaned-up Options.Prefix with surrounding slashes
	// trimmed so resolveKey can join unambiguously.
	prefix string
}

// New constructs the S3 Backend. The SDK client is built up front so
// the daemon fails closed at boot if credentials are bad (rather than
// at first artifact write).
func New(ctx context.Context, opts Options) (*Backend, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	client, err := buildClient(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Backend{
		opts:   opts,
		client: client,
		prefix: cleanPrefix(opts.Prefix),
	}, nil
}

// newWithClient is the test-only constructor that bypasses
// buildClient. Production callers go through New.
func newWithClient(opts Options, client s3API) *Backend {
	return &Backend{
		opts:   opts,
		client: client,
		prefix: cleanPrefix(opts.Prefix),
	}
}

// cleanPrefix strips leading and trailing slashes so resolveKey can
// produce "prefix/key" or just "key" without doubled separators.
func cleanPrefix(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "/")
	return p
}

// resolveKey produces the absolute S3 object key for the given
// caller-side key. Empty key is an error (matches LocalBackend). The
// caller's key is normalised by stripping leading slashes so a path
// like "/p1/x" lands the same place as "p1/x".
func (b *Backend) resolveKey(key string) (string, error) {
	if key == "" {
		return "", errors.New("artifacts/s3: empty key")
	}
	clean := strings.TrimLeft(key, "/")
	if clean == "" {
		return "", errors.New("artifacts/s3: empty key")
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return "", fmt.Errorf("artifacts/s3: resolve key %q: path traversal is not allowed", key)
		}
	}
	clean = path.Clean(clean)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("artifacts/s3: resolve key %q: path traversal is not allowed", key)
	}
	if b.prefix == "" {
		return clean, nil
	}
	return b.prefix + "/" + clean, nil
}

// stripPrefix removes the bucket-level prefix from a returned object
// key, leaving the caller-visible key. Mirrors LocalBackend's
// filepath.Rel adjustment in List.
func (b *Backend) stripPrefix(key string) string {
	if b.prefix == "" {
		return key
	}
	pfx := b.prefix + "/"
	return strings.TrimPrefix(key, pfx)
}

// Put writes the bytes from r at the given key. The SDK requires a
// seekable reader for signed payloads; small artifacts buffer
// in-memory, larger ones still use the same path (artifacts are
// typically small text outputs — the multipart cutover lands when we
// see >5MB writes, which the current call sites don't generate).
func (b *Backend) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	resolvedKey, err := b.resolveKey(key)
	if err != nil {
		return 0, err
	}
	// Buffer the payload so the SDK can compute the content length
	// and sign the request. Artifact bodies are bounded by upstream
	// caps (verifier ≤1MiB; agent outputs typically <500KB) so this
	// is fine; if we ever need to stream multipart, that's a slice-7
	// follow-up.
	body, err := io.ReadAll(r)
	if err != nil {
		return 0, fmt.Errorf("artifacts/s3: read source: %w", err)
	}
	_, err = b.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(b.opts.Bucket),
		Key:    aws.String(resolvedKey),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		return 0, fmt.Errorf("artifacts/s3: put %q: %w", key, err)
	}
	return int64(len(body)), nil
}

// Get fetches the object at key. The returned ReadCloser must be
// closed by the caller; closing it releases the underlying HTTP
// connection back to the SDK's pool.
func (b *Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolvedKey, err := b.resolveKey(key)
	if err != nil {
		return nil, err
	}
	out, err := b.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(b.opts.Bucket),
		Key:    aws.String(resolvedKey),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, artifacts.ErrNotFound
		}
		return nil, fmt.Errorf("artifacts/s3: get %q: %w", key, err)
	}
	return out.Body, nil
}

// Delete removes the object at key. S3 DeleteObject is already
// idempotent (returns 204 for both existing + missing keys), so this
// matches the LocalBackend contract without extra branching.
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resolvedKey, err := b.resolveKey(key)
	if err != nil {
		return err
	}
	_, err = b.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(b.opts.Bucket),
		Key:    aws.String(resolvedKey),
	})
	if err != nil {
		return fmt.Errorf("artifacts/s3: delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether an object exists at key. Implemented via
// HeadObject — cheaper than GetObject because the body is not
// transferred.
func (b *Backend) Exists(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	resolvedKey, err := b.resolveKey(key)
	if err != nil {
		return false, err
	}
	_, err = b.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(b.opts.Bucket),
		Key:    aws.String(resolvedKey),
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("artifacts/s3: head %q: %w", key, err)
	}
	return true, nil
}

// Stat returns size + ETag for the object at key.
func (b *Backend) Stat(ctx context.Context, key string) (artifacts.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return artifacts.ObjectInfo{}, err
	}
	resolvedKey, err := b.resolveKey(key)
	if err != nil {
		return artifacts.ObjectInfo{}, err
	}
	out, err := b.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(b.opts.Bucket),
		Key:    aws.String(resolvedKey),
	})
	if err != nil {
		if isNotFound(err) {
			return artifacts.ObjectInfo{}, artifacts.ErrNotFound
		}
		return artifacts.ObjectInfo{}, fmt.Errorf("artifacts/s3: head %q: %w", key, err)
	}
	info := artifacts.ObjectInfo{Key: key}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.ETag != nil {
		info.ETag = strings.Trim(*out.ETag, `"`)
	}
	return info, nil
}

// List walks every object whose key starts with the configured
// prefix + the caller-supplied prefix. ListObjectsV2's continuation
// token is followed transparently so callers see one continuous
// stream; the per-page size defaults to whatever the SDK negotiates
// (1000 keys at AWS; MinIO honours the same limit).
//
// fn returning io.EOF stops iteration cleanly (matches LocalBackend);
// any other non-nil error from fn propagates back to the caller.
// Empty prefix lists everything under the backend's bucket+prefix
// scope.
func (b *Backend) List(ctx context.Context, prefix string, fn artifacts.WalkFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("artifacts/s3: nil walker")
	}
	// Build the effective S3-side prefix: bucket-level prefix + the
	// caller-supplied one. Empty caller prefix degrades to just the
	// backend's prefix (or no prefix at all for an unconfigured
	// deployment).
	s3Prefix := b.prefix
	if prefix != "" {
		callerClean := strings.TrimLeft(prefix, "/")
		if callerClean != "" {
			var err error
			callerClean, err = (&Backend{}).resolveKey(callerClean)
			if err != nil {
				return err
			}
		}
		if callerClean != "" {
			if s3Prefix == "" {
				s3Prefix = callerClean
			} else {
				s3Prefix = s3Prefix + "/" + callerClean
			}
		}
	}

	var continuationToken *string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		in := &awss3.ListObjectsV2Input{
			Bucket:            aws.String(b.opts.Bucket),
			ContinuationToken: continuationToken,
		}
		if s3Prefix != "" {
			in.Prefix = aws.String(s3Prefix)
		}
		out, err := b.client.ListObjectsV2(ctx, in)
		if err != nil {
			return fmt.Errorf("artifacts/s3: list %q: %w", prefix, err)
		}
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			info := artifacts.ObjectInfo{
				Key: b.stripPrefix(key),
			}
			if obj.Size != nil {
				info.Size = *obj.Size
			}
			if obj.ETag != nil {
				info.ETag = strings.Trim(*obj.ETag, `"`)
			}
			if err := fn(info); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			return nil
		}
		// Prefer NextContinuationToken; fall back to the page's last
		// key if some servers omit it.
		if out.NextContinuationToken != nil {
			continuationToken = out.NextContinuationToken
		} else {
			// No token + truncated is a server bug, but we stop
			// rather than loop forever.
			return nil
		}
	}
}

// Close releases SDK resources. The HTTP client uses keep-alive pools;
// the SDK's standard client has no explicit Close, so this is a
// no-op for now.
func (b *Backend) Close() error { return nil }

// isNotFound reports whether err is the SDK's typed not-found error
// for either GetObject (NoSuchKey) or HeadObject (NotFound — HTTP
// 404 with no body, hence a separate type).
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *s3types.NotFound
	return errors.As(err, &notFound)
}

// Compile-time interface check.
var _ artifacts.FileBackend = (*Backend)(nil)
