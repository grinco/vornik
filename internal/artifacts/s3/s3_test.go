package s3

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOptions_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{name: "ok", opts: Options{Bucket: "b", Region: "r"}, want: ""},
		{name: "missing bucket", opts: Options{Region: "r"}, want: "bucket is required"},
		{name: "missing region", opts: Options{Bucket: "b"}, want: "region is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v want substring %q", err, tc.want)
			}
		})
	}
}

func TestNew_ValidatesOptions(t *testing.T) {
	t.Parallel()
	_, err := New(context.Background(), Options{})
	if err == nil {
		t.Fatal("expected validation error from empty Options")
	}
}

// TestErrNotImplementedExported keeps the sentinel discoverable for
// callers that key off it; the slice-4 List implementation no longer
// returns it, but the symbol stays exported as a forward-compat
// signal for hypothetical future drivers in this package.
func TestErrNotImplementedExported(t *testing.T) {
	t.Parallel()
	if ErrNotImplemented == nil {
		t.Fatal("ErrNotImplemented should be a non-nil sentinel")
	}
}

// keep the unused-import guards quiet now that those imports aren't
// used here.
var _ = bytes.NewReader
var _ = errors.New
