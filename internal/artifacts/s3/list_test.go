package s3

import (
	"context"
	"errors"
	"io"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"vornik.io/vornik/internal/artifacts"
)

func seedList(t *testing.T, prefix string, keys map[string]string) (*Backend, *stubClient) {
	t.Helper()
	stub := newStub()
	for k, v := range keys {
		stub.objects[k] = []byte(v)
	}
	b := newWithClient(Options{Region: "r", Bucket: "b", Prefix: prefix}, stub)
	return b, stub
}

func TestBackend_List_Empty(t *testing.T) {
	t.Parallel()
	b, _ := seedList(t, "", nil)
	var got []string
	err := b.List(context.Background(), "", func(o artifacts.ObjectInfo) error {
		got = append(got, o.Key)
		return nil
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestBackend_List_SinglePage(t *testing.T) {
	t.Parallel()
	b, _ := seedList(t, "", map[string]string{
		"p1/a": "a", "p1/b": "b", "p2/c": "c",
	})
	var got []string
	if err := b.List(context.Background(), "", func(o artifacts.ObjectInfo) error {
		got = append(got, o.Key)
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(got)
	want := []string{"p1/a", "p1/b", "p2/c"}
	if len(got) != 3 {
		t.Fatalf("List: got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBackend_List_PrefixFilter(t *testing.T) {
	t.Parallel()
	b, stub := seedList(t, "", map[string]string{
		"p1/a": "a", "p1/b": "b", "p2/c": "c",
	})
	var got []string
	if err := b.List(context.Background(), "p1", func(o artifacts.ObjectInfo) error {
		got = append(got, o.Key)
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v want 2 results", got)
	}
	// The Backend should have sent prefix=p1 to S3, not the empty
	// prefix.
	if aws.ToString(stub.lastList.Prefix) != "p1" {
		t.Fatalf("List prefix sent = %q, want p1", aws.ToString(stub.lastList.Prefix))
	}
}

func TestBackend_List_BucketPrefixComposed(t *testing.T) {
	t.Parallel()
	b, stub := seedList(t, "vornik/prod", map[string]string{
		"vornik/prod/p1/a": "a",
		"vornik/prod/p2/b": "b",
		"otherapp/x":       "x", // outside the bucket prefix — must not surface
	})
	var got []string
	if err := b.List(context.Background(), "p1", func(o artifacts.ObjectInfo) error {
		got = append(got, o.Key)
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if aws.ToString(stub.lastList.Prefix) != "vornik/prod/p1" {
		t.Fatalf("composed prefix = %q want vornik/prod/p1", aws.ToString(stub.lastList.Prefix))
	}
	// stripPrefix should remove the backend's prefix.
	if len(got) != 1 || got[0] != "p1/a" {
		t.Fatalf("got %v, want [p1/a]", got)
	}
}

func TestBackend_List_MultiPagePagination(t *testing.T) {
	t.Parallel()
	keys := map[string]string{}
	for i := 0; i < 7; i++ {
		keys[fmtSprintf("p/k%d", i)] = "x"
	}
	b, stub := seedList(t, "", keys)
	stub.pageSize = 3 // force 3 pages: 3,3,1

	var got []string
	if err := b.List(context.Background(), "", func(o artifacts.ObjectInfo) error {
		got = append(got, o.Key)
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("got %d items, want 7", len(got))
	}
	if stub.listCalls != 3 {
		t.Fatalf("expected 3 ListObjectsV2 calls, got %d", stub.listCalls)
	}
}

func TestBackend_List_WalkerErrorStops(t *testing.T) {
	t.Parallel()
	b, _ := seedList(t, "", map[string]string{"k1": "v", "k2": "v", "k3": "v"})
	boom := errors.New("boom")
	count := 0
	err := b.List(context.Background(), "", func(o artifacts.ObjectInfo) error {
		count++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected single walker call, got %d", count)
	}
}

func TestBackend_List_WalkerEOFStopsCleanly(t *testing.T) {
	t.Parallel()
	keys := map[string]string{}
	for i := 0; i < 5; i++ {
		keys[fmtSprintf("k%d", i)] = "v"
	}
	b, stub := seedList(t, "", keys)
	stub.pageSize = 2
	count := 0
	err := b.List(context.Background(), "", func(o artifacts.ObjectInfo) error {
		count++
		if count == 3 {
			return io.EOF
		}
		return nil
	})
	if err != nil {
		t.Fatalf("io.EOF should stop cleanly, got %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 walker calls before EOF, got %d", count)
	}
}

func TestBackend_List_PropagatesAPIError(t *testing.T) {
	t.Parallel()
	b, stub := seedList(t, "", nil)
	stub.listErr = errors.New("network")
	err := b.List(context.Background(), "", func(artifacts.ObjectInfo) error { return nil })
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBackend_List_NilWalker(t *testing.T) {
	t.Parallel()
	b, _ := seedList(t, "", nil)
	if err := b.List(context.Background(), "", nil); err == nil {
		t.Fatal("expected nil-walker error")
	}
}

func TestBackend_List_ContextCancelled(t *testing.T) {
	t.Parallel()
	b, _ := seedList(t, "", map[string]string{"k": "v"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := b.List(ctx, "", func(artifacts.ObjectInfo) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestBackend_List_SizeAndETagPropagated(t *testing.T) {
	t.Parallel()
	b, _ := seedList(t, "", map[string]string{"k": "hello"})
	var info artifacts.ObjectInfo
	if err := b.List(context.Background(), "", func(o artifacts.ObjectInfo) error {
		info = o
		return io.EOF
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if info.Size != 5 {
		t.Fatalf("size = %d", info.Size)
	}
	if info.ETag != "abc" {
		t.Fatalf("etag = %q", info.ETag)
	}
}
