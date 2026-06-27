package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrockctltypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/prometheus/client_golang/prometheus"
)

// TestBedrockProvider_RecordMetrics_NilSink — recordMetrics is a no-op
// when no Metrics is attached.
func TestBedrockProvider_RecordMetrics_NilSink(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "model")
	if err != nil {
		t.Fatalf("NewBedrockProvider: %v", err)
	}
	// No SetMetrics call → metrics nil → recordMetrics is a no-op.
	p.recordMetrics(100*time.Millisecond, nil, nil)
	p.recordMetrics(100*time.Millisecond, nil, errors.New("noop"))
}

// TestBedrockProvider_RecordMetrics_SuccessAndError drives both labels.
func TestBedrockProvider_RecordMetrics_SuccessAndError(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "anthropic.claude-3-5")
	if err != nil {
		t.Fatalf("NewBedrockProvider: %v", err)
	}
	p.SetMetrics(metrics)

	// Success with usage.
	usage := &bedrocktypes.TokenUsage{
		InputTokens:  aws.Int32(100),
		OutputTokens: aws.Int32(50),
	}
	p.recordMetrics(50*time.Millisecond, usage, nil)

	// Error.
	p.recordMetrics(20*time.Millisecond, nil, errors.New("api boom"))
}

// TestBedrockProvider_SupportsTextChat — the helper used to filter the
// live catalog to chat-capable models.
func TestBedrockProvider_SupportsTextChat(t *testing.T) {
	textIn := []bedrockctltypes.ModelModality{bedrockctltypes.ModelModalityText}
	imgIn := []bedrockctltypes.ModelModality{bedrockctltypes.ModelModalityImage}

	cases := []struct {
		name string
		in   []bedrockctltypes.ModelModality
		out  []bedrockctltypes.ModelModality
		want bool
	}{
		{"text-in/text-out", textIn, textIn, true},
		{"text-in/image-out", textIn, imgIn, false},
		{"image-in/text-out", imgIn, textIn, false},
		{"image-in/image-out", imgIn, imgIn, false},
		{"empty-in", nil, textIn, false},
		{"empty-out", textIn, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sum := bedrockctltypes.FoundationModelSummary{
				InputModalities:  tc.in,
				OutputModalities: tc.out,
			}
			if got := supportsTextChat(sum); got != tc.want {
				t.Errorf("supportsTextChat = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBedrockProvider_WithLiveCatalogOption ensures the option configures
// the catalog field with the requested TTL (capped at 24h).
func TestBedrockProvider_WithLiveCatalogOption(t *testing.T) {
	cases := []struct {
		name    string
		ttl     time.Duration
		wantTTL time.Duration
	}{
		{"explicit 1h", time.Hour, time.Hour},
		{"zero -> 24h cap", 0, 24 * time.Hour},
		{"negative -> 24h cap", -time.Second, 24 * time.Hour},
		{"oversize -> 24h cap", 48 * time.Hour, 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewBedrockProvider(context.Background(), "us-east-1", "m", WithBedrockLiveCatalog(tc.ttl))
			if err != nil {
				t.Fatalf("NewBedrockProvider: %v", err)
			}
			if p.liveCatalog == nil {
				t.Fatal("liveCatalog should be set")
			}
			if p.liveCatalog.ttl != tc.wantTTL {
				t.Errorf("ttl = %v, want %v", p.liveCatalog.ttl, tc.wantTTL)
			}
		})
	}
}

// TestBedrockProvider_RequestIDFromMetadata — empty path + nil-safe.
func TestBedrockProvider_RequestIDFromMetadata(t *testing.T) {
	if got := requestIDFromMetadata(nil); got != "" {
		t.Errorf("nil -> %q, want empty", got)
	}
}
