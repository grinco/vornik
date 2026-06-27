// Coverage for the small bedrock helpers that don't need an AWS
// SDK mock — supportsTextChat (pure data) and WithBedrockLiveCatalog
// (option closure). The big network paths stay AWS-mocked elsewhere
// (out of scope for this sweep per the brief).

package chat

import (
	"context"
	"testing"
	"time"

	bedrockctltypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
)

func TestSupportsTextChat_TextInAndOut(t *testing.T) {
	s := bedrockctltypes.FoundationModelSummary{
		InputModalities:  []bedrockctltypes.ModelModality{bedrockctltypes.ModelModalityText},
		OutputModalities: []bedrockctltypes.ModelModality{bedrockctltypes.ModelModalityText},
	}
	if !supportsTextChat(s) {
		t.Error("text in + text out should report true")
	}
}

func TestSupportsTextChat_EmbeddingOnly(t *testing.T) {
	s := bedrockctltypes.FoundationModelSummary{
		InputModalities:  []bedrockctltypes.ModelModality{bedrockctltypes.ModelModalityText},
		OutputModalities: []bedrockctltypes.ModelModality{bedrockctltypes.ModelModalityEmbedding},
	}
	if supportsTextChat(s) {
		t.Error("embedding output should report false")
	}
}

func TestSupportsTextChat_ImageInTextOut(t *testing.T) {
	s := bedrockctltypes.FoundationModelSummary{
		InputModalities:  []bedrockctltypes.ModelModality{bedrockctltypes.ModelModalityImage},
		OutputModalities: []bedrockctltypes.ModelModality{bedrockctltypes.ModelModalityText},
	}
	if supportsTextChat(s) {
		t.Error("image-only input should report false")
	}
}

func TestSupportsTextChat_Empty(t *testing.T) {
	s := bedrockctltypes.FoundationModelSummary{}
	if supportsTextChat(s) {
		t.Error("empty modalities should report false")
	}
}

func TestSupportsTextChat_MixedModalities(t *testing.T) {
	// Both text and image in input → true (text is one of them).
	s := bedrockctltypes.FoundationModelSummary{
		InputModalities: []bedrockctltypes.ModelModality{
			bedrockctltypes.ModelModalityImage,
			bedrockctltypes.ModelModalityText,
		},
		OutputModalities: []bedrockctltypes.ModelModality{
			bedrockctltypes.ModelModalityText,
		},
	}
	if !supportsTextChat(s) {
		t.Error("text-among-other-modalities should report true")
	}
}

func TestWithBedrockLiveCatalog_AppliesAndCapsTTL(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1",
		"anthropic.claude-3-haiku-20240307-v1:0",
		WithBedrockLiveCatalog(2*time.Hour))
	if err != nil {
		t.Fatalf("NewBedrockProvider: %v", err)
	}
	if p.liveCatalog == nil {
		t.Fatal("WithBedrockLiveCatalog did not wire liveCatalog")
	}
	if p.liveCatalog.ttl != 2*time.Hour {
		t.Errorf("ttl: got %v, want 2h", p.liveCatalog.ttl)
	}
}

func TestWithBedrockLiveCatalog_NonPositiveDefaultsTo24h(t *testing.T) {
	p, _ := NewBedrockProvider(context.Background(), "us-east-1", "m",
		WithBedrockLiveCatalog(0))
	if p.liveCatalog == nil || p.liveCatalog.ttl != 24*time.Hour {
		t.Errorf("zero ttl should default to 24h; got %v", p.liveCatalog.ttl)
	}
}

func TestWithBedrockLiveCatalog_TooLargeClampedTo24h(t *testing.T) {
	p, _ := NewBedrockProvider(context.Background(), "us-east-1", "m",
		WithBedrockLiveCatalog(48*time.Hour))
	if p.liveCatalog == nil || p.liveCatalog.ttl != 24*time.Hour {
		t.Errorf("48h ttl should clamp to 24h; got %v", p.liveCatalog.ttl)
	}
}
