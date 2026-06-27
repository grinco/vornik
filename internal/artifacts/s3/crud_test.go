package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"vornik.io/vornik/internal/artifacts"
)

// stubClient is an in-memory s3API implementation for unit tests.
// Captures inputs from every method so assertions can verify the
// Backend translated the FileBackend call into the right SDK request.
type stubClient struct {
	objects map[string][]byte

	putErr    error
	getErr    error
	deleteErr error
	headErr   error
	listErr   error
	// pageSize caps each List page (0 = unlimited).
	pageSize  int
	listCalls int

	// Last call recorders.
	lastPut    *awss3.PutObjectInput
	lastGet    *awss3.GetObjectInput
	lastDelete *awss3.DeleteObjectInput
	lastHead   *awss3.HeadObjectInput
	lastList   *awss3.ListObjectsV2Input
}

// Helpers for the stub — kept here to avoid importing fmt/sort in
// every test file using the stub.
func sortStrings(s []string) { sort.Strings(s) }
func fmtSprintf(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}
func fmtSscanf(s string, format string, args ...interface{}) (int, error) {
	return fmt.Sscanf(s, format, args...)
}

func newStub() *stubClient { return &stubClient{objects: map[string][]byte{}} }

func (c *stubClient) PutObject(ctx context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	c.lastPut = in
	if c.putErr != nil {
		return nil, c.putErr
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	c.objects[aws.ToString(in.Key)] = body
	return &awss3.PutObjectOutput{}, nil
}

func (c *stubClient) GetObject(ctx context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	c.lastGet = in
	if c.getErr != nil {
		return nil, c.getErr
	}
	body, ok := c.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (c *stubClient) DeleteObject(ctx context.Context, in *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	c.lastDelete = in
	if c.deleteErr != nil {
		return nil, c.deleteErr
	}
	delete(c.objects, aws.ToString(in.Key))
	return &awss3.DeleteObjectOutput{}, nil
}

func (c *stubClient) HeadObject(ctx context.Context, in *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	c.lastHead = in
	if c.headErr != nil {
		return nil, c.headErr
	}
	body, ok := c.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NotFound{}
	}
	sz := int64(len(body))
	tag := `"abc"`
	return &awss3.HeadObjectOutput{ContentLength: &sz, ETag: &tag}, nil
}

// listErr lets tests force a List error on a configurable call index.
// pageSize lets tests force pagination (default = unlimited so the
// stub returns everything in one page).
func (c *stubClient) ListObjectsV2(ctx context.Context, in *awss3.ListObjectsV2Input, _ ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error) {
	c.lastList = in
	c.listCalls++
	if c.listErr != nil {
		return nil, c.listErr
	}
	// Gather matching keys + sort for deterministic pagination.
	prefix := aws.ToString(in.Prefix)
	matched := make([]string, 0, len(c.objects))
	for k := range c.objects {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			matched = append(matched, k)
		}
	}
	// Deterministic order.
	sortStrings(matched)

	// Continuation token = the next index to read, encoded as a
	// hex string so the test can assert it without typing magic
	// numbers.
	start := 0
	if in.ContinuationToken != nil {
		var n int
		_, _ = fmtSscanf(*in.ContinuationToken, "%d", &n)
		start = n
	}
	end := len(matched)
	if c.pageSize > 0 && start+c.pageSize < end {
		end = start + c.pageSize
	}

	contents := make([]s3types.Object, 0, end-start)
	for _, k := range matched[start:end] {
		body := c.objects[k]
		sz := int64(len(body))
		etag := `"abc"`
		key := k
		contents = append(contents, s3types.Object{Key: &key, Size: &sz, ETag: &etag})
	}
	truncated := end < len(matched)
	out := &awss3.ListObjectsV2Output{
		Contents:    contents,
		IsTruncated: &truncated,
	}
	if truncated {
		tok := fmtSprintf("%d", end)
		out.NextContinuationToken = &tok
	}
	return out, nil
}

func testBackend(t *testing.T, prefix string) (*Backend, *stubClient) {
	t.Helper()
	stub := newStub()
	b := newWithClient(Options{Region: "us-east-1", Bucket: "bucket", Prefix: prefix}, stub)
	return b, stub
}

func TestBackend_Put_RoundTrip(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "")
	n, err := b.Put(context.Background(), "p1/x.txt", bytes.NewReader([]byte("hello")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != 5 {
		t.Fatalf("n = %d, want 5", n)
	}
	if aws.ToString(stub.lastPut.Bucket) != "bucket" {
		t.Fatalf("bucket = %q", aws.ToString(stub.lastPut.Bucket))
	}
	if aws.ToString(stub.lastPut.Key) != "p1/x.txt" {
		t.Fatalf("key = %q", aws.ToString(stub.lastPut.Key))
	}
	if !bytes.Equal(stub.objects["p1/x.txt"], []byte("hello")) {
		t.Fatalf("stored = %q", stub.objects["p1/x.txt"])
	}
}

func TestBackend_Put_AppliesPrefix(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "vornik/prod")
	if _, err := b.Put(context.Background(), "p1/x.txt", bytes.NewReader([]byte("hi"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if aws.ToString(stub.lastPut.Key) != "vornik/prod/p1/x.txt" {
		t.Fatalf("key = %q", aws.ToString(stub.lastPut.Key))
	}
}

func TestBackend_Put_EmptyKey(t *testing.T) {
	t.Parallel()
	b, _ := testBackend(t, "")
	if _, err := b.Put(context.Background(), "", bytes.NewReader(nil)); err == nil {
		t.Fatal("expected empty-key error")
	}
}

func TestBackend_Put_PropagatesError(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "")
	stub.putErr = errors.New("boom")
	_, err := b.Put(context.Background(), "k", bytes.NewReader([]byte("v")))
	if err == nil || !strings.Contains(err.Error(), "put") {
		t.Fatalf("err = %v", err)
	}
}

func TestBackend_Get_RoundTrip(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "vornik")
	stub.objects["vornik/k"] = []byte("hello")
	rc, err := b.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestBackend_Get_NotFound(t *testing.T) {
	t.Parallel()
	b, _ := testBackend(t, "")
	_, err := b.Get(context.Background(), "missing")
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestBackend_Get_OtherError(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "")
	stub.getErr = errors.New("network bad")
	_, err := b.Get(context.Background(), "k")
	if errors.Is(err, artifacts.ErrNotFound) {
		t.Fatal("unexpected ErrNotFound for network error")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestBackend_Delete_Idempotent(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "")
	stub.objects["k"] = []byte("v")
	if err := b.Delete(context.Background(), "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Second Delete: stub returns no error (matches S3 semantics).
	if err := b.Delete(context.Background(), "k"); err != nil {
		t.Fatalf("Delete idempotent: %v", err)
	}
}

func TestBackend_Delete_PropagatesError(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "")
	stub.deleteErr = errors.New("permission denied")
	if err := b.Delete(context.Background(), "k"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBackend_Exists(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "p")
	stub.objects["p/k"] = []byte("x")
	ok, err := b.Exists(context.Background(), "k")
	if err != nil || !ok {
		t.Fatalf("Exists existing: ok=%v err=%v", ok, err)
	}
	ok, err = b.Exists(context.Background(), "missing")
	if err != nil || ok {
		t.Fatalf("Exists missing: ok=%v err=%v", ok, err)
	}
}

func TestBackend_Exists_OtherError(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "")
	stub.headErr = errors.New("network")
	_, err := b.Exists(context.Background(), "k")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBackend_Stat(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "")
	stub.objects["k"] = []byte("hello")
	info, err := b.Stat(context.Background(), "k")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 5 || info.Key != "k" || info.ETag != "abc" {
		t.Fatalf("Stat info = %+v", info)
	}
}

func TestBackend_Stat_NotFound(t *testing.T) {
	t.Parallel()
	b, _ := testBackend(t, "")
	_, err := b.Stat(context.Background(), "missing")
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestBackend_Stat_OtherError(t *testing.T) {
	t.Parallel()
	b, stub := testBackend(t, "")
	stub.headErr = errors.New("network")
	_, err := b.Stat(context.Background(), "k")
	if errors.Is(err, artifacts.ErrNotFound) {
		t.Fatal("got ErrNotFound for network err")
	}
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBackend_ContextCancelled(t *testing.T) {
	t.Parallel()
	b, _ := testBackend(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Put(ctx, "k", bytes.NewReader([]byte("x"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put: %v", err)
	}
	if _, err := b.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get: %v", err)
	}
	if err := b.Delete(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Exists(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Exists: %v", err)
	}
	if _, err := b.Stat(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stat: %v", err)
	}
}

func TestIsNotFound(t *testing.T) {
	t.Parallel()
	if !isNotFound(&s3types.NoSuchKey{}) {
		t.Fatal("NoSuchKey should be not-found")
	}
	if !isNotFound(&s3types.NotFound{}) {
		t.Fatal("NotFound should be not-found")
	}
	if isNotFound(errors.New("other")) {
		t.Fatal("plain error should not be not-found")
	}
	if isNotFound(nil) {
		t.Fatal("nil should not be not-found")
	}
}
