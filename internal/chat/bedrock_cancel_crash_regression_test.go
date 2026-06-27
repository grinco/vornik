package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// panickingBedrockClient reproduces the aws-sdk-go-v2 bedrockruntime
// deserialize-middleware nil-deref that crashed the daemon on 2026-06-19:
// a task cancel aborted an in-flight Converse while the daemon was
// handling SIGTERM for a redeploy, and the panic — raised synchronously
// inside the SDK call, on a plain background goroutine with no net/http
// per-request recover — took the whole process down (crash dump through
// ResponseErrorWrapper.HandleDeserialize). The fake panics at the same
// point the real SDK does: inside the Converse / ConverseStream call.
type panickingBedrockClient struct{}

func (panickingBedrockClient) Converse(context.Context, *bedrockruntime.ConverseInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	panic("runtime error: invalid memory address or nil pointer dereference")
}

func (panickingBedrockClient) ConverseStream(context.Context, *bedrockruntime.ConverseStreamInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	panic("runtime error: invalid memory address or nil pointer dereference")
}

// TestBedrockConverseRecoversSDKPanic is the regression for the
// 2026-06-19 cancel-during-shutdown daemon crash. A panic from inside the
// AWS SDK Converse call must be converted into an ordinary error so the
// daemon survives; pre-fix the panic propagated and crashed the process.
func TestBedrockConverseRecoversSDKPanic(t *testing.T) {
	p := &BedrockProvider{model: "zai.glm-5", region: "us-east-1", client: panickingBedrockClient{}}

	resp, err := p.complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatalf("expected an error from the recovered SDK panic, got nil (resp=%v)", resp)
	}
	if resp != nil {
		t.Fatalf("expected nil response on recovered panic, got %v", resp)
	}
	if !strings.Contains(err.Error(), "recovered panic") {
		t.Fatalf("error should name the recovered panic, got %q", err.Error())
	}
}

// TestBedrockConverseStreamRecoversSDKPanic covers the streaming entry
// point: a panic on the initial ConverseStream call must not escape. The
// streaming path falls back to non-streaming Converse on a ConverseStream
// error, which also panics on this fake and is likewise recovered, so the
// caller gets a clean error instead of a crash.
func TestBedrockConverseStreamRecoversSDKPanic(t *testing.T) {
	p := &BedrockProvider{model: "zai.glm-5", region: "us-east-1", client: panickingBedrockClient{}}

	resp, err := p.CompleteWithToolsStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Fatalf("expected an error from the recovered SDK panic, got nil (resp=%v)", resp)
	}
	if resp != nil {
		t.Fatalf("expected nil response on recovered panic, got %v", resp)
	}
}
