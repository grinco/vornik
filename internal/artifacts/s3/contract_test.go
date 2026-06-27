package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/artifacts/backendtest"
	"vornik.io/vornik/internal/artifacts/s3"
)

// memClient is an in-memory s3API used by the contract suite. It is
// intentionally minimal but page-aware so the same suite exercises
// LocalBackend and the S3 backend against equivalent behavior.
//
// Lives in the s3_test (external test) package so production code
// doesn't accidentally take a dep on it; the s3 package exposes
// NewWithClient for this fixture (see export_test.go).
type memClient struct {
	mu       sync.Mutex
	objects  map[string][]byte
	pageSize int
}

func newMemClient() *memClient {
	return &memClient{objects: map[string][]byte{}, pageSize: 0}
}

func (c *memClient) PutObject(_ context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.objects[aws.ToString(in.Key)] = body
	c.mu.Unlock()
	return &awss3.PutObjectOutput{}, nil
}

func (c *memClient) GetObject(_ context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	c.mu.Lock()
	body, ok := c.objects[aws.ToString(in.Key)]
	c.mu.Unlock()
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (c *memClient) DeleteObject(_ context.Context, in *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	c.mu.Lock()
	delete(c.objects, aws.ToString(in.Key))
	c.mu.Unlock()
	return &awss3.DeleteObjectOutput{}, nil
}

func (c *memClient) HeadObject(_ context.Context, in *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	c.mu.Lock()
	body, ok := c.objects[aws.ToString(in.Key)]
	c.mu.Unlock()
	if !ok {
		return nil, &s3types.NotFound{}
	}
	sz := int64(len(body))
	tag := `"abc"`
	return &awss3.HeadObjectOutput{ContentLength: &sz, ETag: &tag}, nil
}

func (c *memClient) ListObjectsV2(_ context.Context, in *awss3.ListObjectsV2Input, _ ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error) {
	c.mu.Lock()
	keys := make([]string, 0, len(c.objects))
	prefix := aws.ToString(in.Prefix)
	for k := range c.objects {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	c.mu.Unlock()
	sort.Strings(keys)

	start := 0
	if in.ContinuationToken != nil {
		_, _ = fmt.Sscanf(*in.ContinuationToken, "%d", &start)
	}
	end := len(keys)
	if c.pageSize > 0 && start+c.pageSize < end {
		end = start + c.pageSize
	}
	contents := make([]s3types.Object, 0, end-start)
	for _, k := range keys[start:end] {
		c.mu.Lock()
		size := int64(len(c.objects[k]))
		c.mu.Unlock()
		etag := `"abc"`
		key := k
		contents = append(contents, s3types.Object{Key: &key, Size: &size, ETag: &etag})
	}
	truncated := end < len(keys)
	out := &awss3.ListObjectsV2Output{Contents: contents, IsTruncated: &truncated}
	if truncated {
		tok := fmt.Sprintf("%d", end)
		out.NextContinuationToken = &tok
	}
	return out, nil
}

// TestS3Backend_Contract proves the in-memory S3 backend satisfies
// the FileBackend contract on identical scenarios to LocalBackend.
// The MinIO integration test in integration_test.go drives the same
// suite against a real S3 implementation when VORNIK_S3_INTEGRATION
// is set.
func TestS3Backend_Contract(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) (artifacts.FileBackend, func()) {
		mc := newMemClient()
		b := s3.NewWithClient(s3.Options{Region: "r", Bucket: "b", Prefix: "vornik/test"}, mc)
		return b, func() { _ = b.Close() }
	})
}
