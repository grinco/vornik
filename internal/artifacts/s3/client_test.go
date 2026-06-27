package s3

import (
	"context"
	"errors"
	"testing"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestCleanPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"/", ""},
		{"//", ""},
		{" ", ""},
		{"foo", "foo"},
		{"/foo", "foo"},
		{"foo/", "foo"},
		{"/foo/", "foo"},
		{"foo/bar", "foo/bar"},
		{"/foo/bar/", "foo/bar"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := cleanPrefix(tc.in)
			if got != tc.want {
				t.Fatalf("cleanPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBackend_ResolveKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prefix  string
		key     string
		want    string
		wantErr bool
	}{
		{name: "empty key", key: "", wantErr: true},
		{name: "no prefix simple", key: "k", want: "k"},
		{name: "no prefix nested", key: "a/b/c", want: "a/b/c"},
		{name: "no prefix leading slash", key: "/a/b", want: "a/b"},
		{name: "slash only key", key: "/", wantErr: true},
		{name: "parent traversal", key: "../secret", wantErr: true},
		{name: "nested parent traversal", key: "a/../secret", wantErr: true},
		{name: "prefix simple", prefix: "vornik", key: "k", want: "vornik/k"},
		{name: "prefix nested", prefix: "vornik/prod", key: "p/e/f", want: "vornik/prod/p/e/f"},
		{name: "prefix with surrounding slashes", prefix: "/vornik/", key: "k", want: "vornik/k"},
		{name: "prefix + leading slash key", prefix: "vornik", key: "/k", want: "vornik/k"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &Backend{prefix: cleanPrefix(tc.prefix)}
			got, err := b.resolveKey(tc.key)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveKey(%q) prefix=%q = %q want %q", tc.key, tc.prefix, got, tc.want)
			}
		})
	}
}

func TestBackend_StripPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prefix string
		in     string
		want   string
	}{
		{"", "k", "k"},
		{"vornik", "vornik/k", "k"},
		{"vornik", "k", "k"}, // no prefix to strip — returned as-is
		{"vornik/prod", "vornik/prod/p/e/f", "p/e/f"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			b := &Backend{prefix: cleanPrefix(tc.prefix)}
			got := b.stripPrefix(tc.in)
			if got != tc.want {
				t.Fatalf("stripPrefix(%q) prefix=%q = %q want %q", tc.in, tc.prefix, got, tc.want)
			}
		})
	}
}

func TestNew_BuildsClient(t *testing.T) {
	t.Parallel()
	// Swap the SDK builder for a stub so tests don't hit STS / IMDS.
	original := buildClient
	defer func() { buildClient = original }()
	var capturedOpts Options
	buildClient = func(ctx context.Context, opts Options) (*awss3.Client, error) {
		capturedOpts = opts
		return &awss3.Client{}, nil
	}

	b, err := New(context.Background(), Options{
		Region:          "us-east-1",
		Bucket:          "vornik-art",
		Prefix:          "/prod/",
		Endpoint:        "http://localhost:9000",
		AccessKeyID:     "AKIA",
		SecretAccessKey: "secret",
		UsePathStyle:    true,
		ForceSSL:        false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.client == nil {
		t.Fatal("client not wired")
	}
	if capturedOpts.Region != "us-east-1" || capturedOpts.Bucket != "vornik-art" {
		t.Fatalf("captured opts not propagated: %+v", capturedOpts)
	}
	if b.prefix != "prod" {
		t.Fatalf("prefix = %q want prod", b.prefix)
	}
}

func TestNew_PropagatesBuilderError(t *testing.T) {
	t.Parallel()
	original := buildClient
	defer func() { buildClient = original }()
	wantErr := errors.New("boom")
	buildClient = func(ctx context.Context, opts Options) (*awss3.Client, error) {
		return nil, wantErr
	}
	_, err := New(context.Background(), Options{Region: "r", Bucket: "b"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("got %v want %v", err, wantErr)
	}
}

func TestDefaultBuildClient_Smoke(t *testing.T) {
	t.Parallel()
	// We don't have AWS creds + can't hit the network, but the SDK's
	// LoadDefaultConfig doesn't make network calls when an explicit
	// region + static creds are provided. Verifies the wiring compiles
	// and returns a real client.
	client, err := defaultBuildClient(context.Background(), Options{
		Region:          "us-east-1",
		Bucket:          "x",
		AccessKeyID:     "AKIA",
		SecretAccessKey: "shhh",
		Endpoint:        "http://localhost:9000",
		UsePathStyle:    true,
		ForceSSL:        false,
	})
	if err != nil {
		t.Fatalf("defaultBuildClient: %v", err)
	}
	if client == nil {
		t.Fatal("client nil")
	}
}
