package chat

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// TestApplyReasoningEffort_ReachesTheRequest pins the plumbing between
// WithReasoningEffort and the wire.
//
// Converse has no first-class reasoning field, so the hint rides
// AdditionalModelRequestFields. That is easy to get subtly wrong — a wrong key
// name is accepted by the SDK and silently ignored by the model, which would
// look exactly like the effort setting having no effect and send the next
// person back to raising the token ceiling.
func TestApplyReasoningEffort_ReachesTheRequest(t *testing.T) {
	input := &bedrockruntime.ConverseInput{}
	applyReasoningEffort(WithReasoningEffort(context.Background(), ReasoningEffortLow), input)

	if input.AdditionalModelRequestFields == nil {
		t.Fatal("reasoning effort did not reach AdditionalModelRequestFields")
	}
	// MarshalSmithyDocument is what the SDK actually puts on the wire —
	// encoding/json on the lazy document yields "{}" and would pass while
	// sending nothing.
	marshaler, ok := input.AdditionalModelRequestFields.(interface{ MarshalSmithyDocument() ([]byte, error) })
	if !ok {
		t.Fatal("additional fields do not expose MarshalSmithyDocument")
	}
	raw, err := marshaler.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("marshal additional fields: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal additional fields: %v", err)
	}
	if got["reasoning_effort"] != ReasoningEffortLow {
		t.Errorf("additional fields = %s, want reasoning_effort=%q", raw, ReasoningEffortLow)
	}
}

// TestApplyReasoningEffort_AbsentWhenUnset — every existing Bedrock call must
// keep its exact current shape. A model that does not understand
// reasoning_effort must never be sent the key.
func TestApplyReasoningEffort_AbsentWhenUnset(t *testing.T) {
	input := &bedrockruntime.ConverseInput{}
	applyReasoningEffort(context.Background(), input)
	if input.AdditionalModelRequestFields != nil {
		t.Error("no effort requested, so no model-specific field may be sent")
	}
}
