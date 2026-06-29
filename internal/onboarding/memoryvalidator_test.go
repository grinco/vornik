package onboarding

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func fakeProbe(vec []float32, err error) embedProbe {
	return func(context.Context, string, string, string) ([]float32, error) {
		return vec, err
	}
}

func TestMemoryValidate_DisabledSkips(t *testing.T) {
	v := NewMemoryValidatorWithProbe(fakeProbe(nil, fmt.Errorf("must not be called")), time.Second)
	res := v.Validate(context.Background(), MemoryConfigProposal{Enabled: false})
	if !res.Skipped {
		t.Fatalf("expected Skipped for disabled memory, got %#v", res)
	}
	if res.EmbeddingOK {
		t.Error("disabled memory must not report EmbeddingOK")
	}
}

func TestMemoryValidate_IncompleteCreds(t *testing.T) {
	v := NewMemoryValidatorWithProbe(fakeProbe([]float32{0.1}, nil), time.Second)
	res := v.Validate(context.Background(), MemoryConfigProposal{Enabled: true, EmbeddingModel: "m"})
	if res.EmbeddingOK || !hasMemFailure(res.Failures, "embedding_incomplete") {
		t.Fatalf("expected embedding_incomplete failure, got %#v", res)
	}
}

func TestMemoryValidate_OKWithDimensionMatch(t *testing.T) {
	v := NewMemoryValidatorWithProbe(fakeProbe(make([]float32, 1536), nil), time.Second)
	res := v.Validate(context.Background(), MemoryConfigProposal{
		Enabled: true, EmbeddingEndpoint: "https://e/v1", EmbeddingModel: "m", EmbeddingDimension: 1536,
	})
	if !res.EmbeddingOK || res.ReturnedDimension != 1536 || !res.DimensionMatches {
		t.Fatalf("unexpected result: %#v", res)
	}
	if len(res.Failures) != 0 {
		t.Errorf("expected no failures on a clean match, got %#v", res.Failures)
	}
}

func TestMemoryValidate_DimensionMismatchAdvisory(t *testing.T) {
	v := NewMemoryValidatorWithProbe(fakeProbe(make([]float32, 768), nil), time.Second)
	res := v.Validate(context.Background(), MemoryConfigProposal{
		Enabled: true, EmbeddingEndpoint: "https://e/v1", EmbeddingModel: "m", EmbeddingDimension: 1536,
	})
	if !res.EmbeddingOK {
		t.Fatal("embedding probe succeeded; EmbeddingOK should be true")
	}
	if res.DimensionMatches || !hasMemFailure(res.Failures, "dimension_mismatch") {
		t.Fatalf("expected dimension_mismatch advisory, got %#v", res)
	}
}

func TestMemoryValidate_AuthErrorMapped(t *testing.T) {
	v := NewMemoryValidatorWithProbe(fakeProbe(nil, fmt.Errorf("HTTP 401 unauthorized")), time.Second)
	res := v.Validate(context.Background(), MemoryConfigProposal{
		Enabled: true, EmbeddingEndpoint: "https://e/v1", EmbeddingModel: "m",
	})
	if res.EmbeddingOK || !hasMemFailure(res.Failures, "embedding_key_rejected") {
		t.Fatalf("expected embedding_key_rejected, got %#v", res)
	}
}

// hasMemFailure is the []CheckFailure variant (chatvalidator_test.go's
// hasFailure takes a ChatValidationResult, so this avoids the collision).
func hasMemFailure(fs []CheckFailure, name string) bool {
	for _, f := range fs {
		if f.Name == name {
			return true
		}
	}
	return false
}
