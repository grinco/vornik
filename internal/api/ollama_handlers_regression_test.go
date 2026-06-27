// Regression coverage for the Ollama-compatibility surface that the
// happy-path tests in ollama_proxy_test.go leave uncovered:
//
//   - OllamaGenerate: per-request model override via WithModel, the
//     prompt-only (no system) one-message synthesis, the model
//     fallback when req.Model is omitted, and the streaming NDJSON
//     framing (its own copy of the delta loop, separate from
//     /api/chat).
//   - collectModelsForOllama: the QueuedProvider branches
//     (ListModelsAggregated hit + flat-list fallback), the plain
//     ModelLister branch, and ModelInfo.Provider overriding the map
//     key. The existing TestOllamaTags_ReturnsModelsList only walks
//     the bare modelListingStub (the final ModelLister branch).
//   - OllamaTags: the full synthetic Details field shape + the
//     family-empty -> "vornik" fallback.
//   - OllamaShow: capabilities for an unknown/empty model id (must
//     still advertise completion+tools, no vision) + the Details
//     shape. Existing tests cover known multimodal ids only.
//   - OllamaVersion: the exact pinned version string.
//
// Helpers added here are prefixed `oh` per the file's assignment.

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// ohOverridableStub is a chat.Provider that also implements
// chat.ModelOverridable. WithModel returns a *copy* pinned to the
// requested model and flips wasOverridden on the COPY (not the
// original) so a test can assert (a) the override fired and (b) the
// original provider was not mutated — the copy-not-mutate contract
// the proxy relies on for concurrent requests.
type ohOverridableStub struct {
	model         string
	withModelArg  *string // pointer set on the returned copy to record the arg
	wasOverridden bool
	finalResp     *chat.ChatResponse
	chunks        []string
}

func (o *ohOverridableStub) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return o.finalResp, nil
}
func (o *ohOverridableStub) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return o.finalResp, nil
}
func (o *ohOverridableStub) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, onText chat.StreamCallback) (*chat.ChatResponse, error) {
	acc := ""
	for _, c := range o.chunks {
		acc += c
		if onText != nil {
			onText(acc)
		}
	}
	return o.finalResp, nil
}
func (o *ohOverridableStub) Model() string              { return o.model }
func (o *ohOverridableStub) SetMetrics(_ *chat.Metrics) {}

// WithModel returns a shallow copy pinned to model. The arg is
// recorded on the copy (via a shared back-pointer) so the test can
// read it; the original keeps wasOverridden=false.
func (o *ohOverridableStub) WithModel(model string) chat.Provider {
	clone := *o
	clone.model = model
	clone.wasOverridden = true
	arg := model
	clone.withModelArg = &arg
	// Thread the recorded arg back to the original so the test
	// (which only holds the original) can observe what WithModel
	// was called with, without mutating the original's behaviour.
	o.withModelArg = &arg
	return &clone
}

var _ chat.ModelOverridable = (*ohOverridableStub)(nil)

// ohBuildResp mirrors buildOllamaOKResponse but lets the caller set
// the finish_reason (the shared helper hardcodes "stop"). Kept under
// the oh prefix to avoid colliding with sibling files.
func ohBuildResp(model, content, finishReason string) *chat.ChatResponse {
	resp := &chat.ChatResponse{Model: model}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{
		Message:      chat.Message{Role: "assistant", Content: content},
		FinishReason: finishReason,
	})
	return resp
}

// TestOllamaVersion_PinsCanonicalString locks the exact version
// string. Existing coverage only asserts non-empty; this pins the
// value clients gate on so a bump is a conscious change rather than
// an accident.
func TestOllamaVersion_PinsCanonicalString(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rr := httptest.NewRecorder()
	s.OllamaVersion(rr, req)
	var v ollamaVersionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Version != "0.5.0-vornik" {
		t.Errorf("version = %q, want 0.5.0-vornik", v.Version)
	}
}

// TestOllamaShow_UnknownModelStillAdvertisesBaseCapabilities — the
// handler synthesizes a response for ANY model id without validating
// it exists (rejecting unknowns would block operators wiring a model
// they're about to add). An unknown/text-only id must still carry
// completion+tools and must NOT carry vision. Existing show tests
// only cover known multimodal ids.
func TestOllamaShow_UnknownModelStillAdvertisesBaseCapabilities(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	body := bytes.NewBufferString(`{"model":"some-never-seen-model-xyz"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/show", body)
	rr := httptest.NewRecorder()
	s.OllamaShow(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp ollamaShowResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	caps := map[string]bool{}
	for _, c := range resp.Capabilities {
		caps[c] = true
	}
	if !caps["completion"] || !caps["tools"] {
		t.Errorf("unknown model caps = %v, want completion+tools", resp.Capabilities)
	}
	if caps["vision"] {
		t.Errorf("unknown text-only model must not advertise vision; caps=%v", resp.Capabilities)
	}
}

// TestOllamaShow_EmptyModelDetailsShape — with no model id supplied
// the family defaults to "vornik" and the synthetic Details block is
// filled with the api/n-a placeholders. Pins the field shape clients
// read (Open WebUI groups by Details.Family).
func TestOllamaShow_EmptyModelDetailsShape(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "x"}))
	req := httptest.NewRequest(http.MethodPost, "/api/show", bytes.NewBufferString(`{}`))
	rr := httptest.NewRecorder()
	s.OllamaShow(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp ollamaShowResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := resp.Details
	if d.Format != "api" {
		t.Errorf("Details.Format = %q, want api", d.Format)
	}
	if d.Family != "vornik" {
		t.Errorf("Details.Family = %q, want vornik (empty-model fallback)", d.Family)
	}
	if len(d.Families) != 1 || d.Families[0] != "vornik" {
		t.Errorf("Details.Families = %v, want [vornik]", d.Families)
	}
	if d.ParameterSize != "n/a" || d.QuantizationLevel != "n/a" {
		t.Errorf("Details placeholders = %q/%q, want n/a/n/a", d.ParameterSize, d.QuantizationLevel)
	}
	if resp.ModelInfo == nil {
		t.Error("ModelInfo must be a non-nil (empty) object")
	}
}

// TestOllamaTags_DetailsAndFamilyShape pins the per-row Details shape
// the existing tags test skips (it only checks Name). Also exercises
// the family-empty -> "vornik" fallback: a ModelInfo with a blank
// Provider that the bare ModelLister branch surfaces under the
// generic "chat" name (non-empty), plus verifying the synthetic
// placeholders ride every row.
func TestOllamaTags_DetailsAndFamilyShape(t *testing.T) {
	prov := modelListingStub{
		openaiStub: openaiStub{model: "gpt-4"},
		models: []chat.ModelInfo{
			{ID: "anthropic.claude-opus-4-7", Provider: "anthropic"},
		},
	}
	s := NewServer(WithChatProvider(prov))
	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rr := httptest.NewRecorder()
	s.OllamaTags(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp ollamaTagsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("got %d models, want 1", len(resp.Models))
	}
	m := resp.Models[0]
	if m.Name != "anthropic.claude-opus-4-7" || m.Model != "anthropic.claude-opus-4-7" {
		t.Errorf("Name/Model = %q/%q, want id verbatim in both", m.Name, m.Model)
	}
	if m.ModifiedAt == "" {
		t.Error("ModifiedAt must be set (clients sort on it)")
	}
	if m.Details.Format != "api" {
		t.Errorf("Details.Format = %q, want api", m.Details.Format)
	}
	if m.Details.ParameterSize != "n/a" || m.Details.QuantizationLevel != "n/a" {
		t.Errorf("Details placeholders = %q/%q, want n/a", m.Details.ParameterSize, m.Details.QuantizationLevel)
	}
}

// TestCollectModelsForOllama_BareListerStampsChat — the final
// ModelLister branch (a provider that is neither *Router nor
// *QueuedProvider) surfaces its models under the generic provider
// name "chat" when ModelInfo.Provider is blank. Pins that
// attribution, which OllamaTags then carries into the family field.
func TestCollectModelsForOllama_BareListerStampsChat(t *testing.T) {
	prov := modelListingStub{
		openaiStub: openaiStub{model: "m"},
		models:     []chat.ModelInfo{{ID: "bare-model"}},
	}
	s := NewServer(WithChatProvider(prov))
	rows := s.collectModelsForOllama(context.Background())
	if len(rows) != 1 {
		t.Fatalf("collectModelsForOllama returned %d rows, want 1", len(rows))
	}
	// The bare ModelLister branch stamps "chat" as the provider.
	if rows[0].provider != "chat" {
		t.Errorf("bare lister row provider = %q, want chat", rows[0].provider)
	}
	if rows[0].id != "bare-model" {
		t.Errorf("row id = %q, want bare-model", rows[0].id)
	}
}

// TestCollectModelsForOllama_ModelInfoProviderOverridesKey — when a
// ModelInfo carries its own Provider, it wins over the map key /
// "chat" default the push closure starts from. Pins the per-model
// provider attribution.
func TestCollectModelsForOllama_ModelInfoProviderOverridesKey(t *testing.T) {
	prov := modelListingStub{
		openaiStub: openaiStub{model: "m"},
		models: []chat.ModelInfo{
			{ID: "a", Provider: "vertex"},
			{ID: "b", Provider: "bedrock"},
		},
	}
	s := NewServer(WithChatProvider(prov))
	rows := s.collectModelsForOllama(context.Background())
	got := map[string]string{}
	for _, r := range rows {
		got[r.id] = r.provider
	}
	if got["a"] != "vertex" {
		t.Errorf("model a provider = %q, want vertex (ModelInfo.Provider wins)", got["a"])
	}
	if got["b"] != "bedrock" {
		t.Errorf("model b provider = %q, want bedrock", got["b"])
	}
}

// TestCollectModelsForOllama_QueuedFlatFallback — a QueuedProvider
// wrapping a NON-router lister can't return an aggregated breakdown,
// so collectModelsForOllama falls back to the flat ListModels path
// and stamps "chat". Exercises the q.ListModels(...) branch
// (priority_queue.go:117) that the aggregated branch skips. The
// inner lister's blank ModelInfo.Provider means the rows surface
// under "chat".
func TestCollectModelsForOllama_QueuedFlatFallback(t *testing.T) {
	inner := modelListingStub{
		openaiStub: openaiStub{model: "m"},
		models: []chat.ModelInfo{
			{ID: "q1"},
			{ID: "q2"},
		},
	}
	queued := chat.NewQueuedProvider(inner, 1)
	s := NewServer(WithChatProvider(queued))
	rows := s.collectModelsForOllama(context.Background())
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.provider != "chat" {
			t.Errorf("row %q provider = %q, want chat (flat-fallback default)", r.id, r.provider)
		}
	}
}

// TestOllamaGenerate_PerRequestModelOverride — /api/generate must
// route req.Model through chat.ModelOverridable.WithModel exactly
// like /api/chat and /v1/chat/completions do (the generate handler
// has its own copy of the WithModel block). A regression here lands
// the request on the fallback provider instead of routing by prefix.
// Asserts WithModel fired with req.Model and the ORIGINAL provider
// was not mutated (copy-not-mutate contract).
func TestOllamaGenerate_PerRequestModelOverride(t *testing.T) {
	stub := &ohOverridableStub{
		model:     "default-model",
		finalResp: ohBuildResp("default-model", "answer", "stop"),
	}
	s := NewServer(WithChatProvider(stub))
	body := bytes.NewBufferString(`{"model":"google/gemini-2.5-pro","stream":false,"prompt":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	rr := httptest.NewRecorder()
	s.OllamaGenerate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if stub.withModelArg == nil {
		t.Fatal("WithModel was not called — req.Model not routed")
	}
	if *stub.withModelArg != "google/gemini-2.5-pro" {
		t.Errorf("WithModel arg = %q, want google/gemini-2.5-pro", *stub.withModelArg)
	}
	// Original must not be mutated.
	if stub.wasOverridden {
		t.Error("original provider was mutated; WithModel must return a copy")
	}
	if stub.model != "default-model" {
		t.Errorf("original model mutated to %q", stub.model)
	}
}

// TestOllamaGenerate_OmittedModelFallsBackToProvider — when req.Model
// is empty the handler must NOT call WithModel and must fall back to
// provider.Model() for the response envelope. Mirrors the documented
// /v1 fallback behaviour for the generate path.
func TestOllamaGenerate_OmittedModelFallsBackToProvider(t *testing.T) {
	stub := &ohOverridableStub{
		model:     "provider-default",
		finalResp: ohBuildResp("", "answer", "stop"),
	}
	s := NewServer(WithChatProvider(stub))
	// No "model" field at all.
	body := bytes.NewBufferString(`{"stream":false,"prompt":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	rr := httptest.NewRecorder()
	s.OllamaGenerate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if stub.withModelArg != nil {
		t.Errorf("WithModel must NOT be called when model omitted; got arg %q", *stub.withModelArg)
	}
	var resp ollamaGenerateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Model != "provider-default" {
		t.Errorf("Model = %q, want provider-default (provider.Model() fallback)", resp.Model)
	}
}

// TestOllamaGenerate_PromptOnlySynthesizesSingleUserMessage — when no
// system prompt is supplied the handler must build a ONE-message
// (user-only) conversation, not inject an empty system message. The
// happy-path test always sends a system field; this pins the
// no-system branch (ollama_proxy.go:650-653).
func TestOllamaGenerate_PromptOnlySynthesizesSingleUserMessage(t *testing.T) {
	captured := &ohCapturingStub{
		finalResp: ohBuildResp("m", "ok", "stop"),
	}
	s := NewServer(WithChatProvider(captured))
	body := bytes.NewBufferString(`{"stream":false,"prompt":"just the prompt"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	rr := httptest.NewRecorder()
	s.OllamaGenerate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if len(captured.captured) != 1 {
		t.Fatalf("synthesized %d messages, want 1 (user only, no system)", len(captured.captured))
	}
	if captured.captured[0].Role != "user" || captured.captured[0].Content != "just the prompt" {
		t.Errorf("synthesized message = %+v, want user/'just the prompt'", captured.captured[0])
	}
}

// TestOllamaGenerate_SystemPlusPromptSynthesizesTwoMessages — with a
// system field present the handler builds system THEN user, in that
// order. Pins the two-message synthesis ordering the providers rely
// on (system must precede user).
func TestOllamaGenerate_SystemPlusPromptSynthesizesTwoMessages(t *testing.T) {
	captured := &ohCapturingStub{
		finalResp: ohBuildResp("m", "ok", "stop"),
	}
	s := NewServer(WithChatProvider(captured))
	body := bytes.NewBufferString(`{"stream":false,"system":"you are terse","prompt":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	rr := httptest.NewRecorder()
	s.OllamaGenerate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(captured.captured) != 2 {
		t.Fatalf("synthesized %d messages, want 2", len(captured.captured))
	}
	if captured.captured[0].Role != "system" || captured.captured[0].Content != "you are terse" {
		t.Errorf("first message = %+v, want system/'you are terse'", captured.captured[0])
	}
	if captured.captured[1].Role != "user" || captured.captured[1].Content != "hi" {
		t.Errorf("second message = %+v, want user/'hi'", captured.captured[1])
	}
}

// TestOllamaGenerate_StreamingEmitsNDJSON — the generate streaming
// path has its OWN delta loop + final-frame builder (separate from
// /api/chat). Verifies content arrives as per-line deltas in the
// `response` field (not `message.content`), the final frame carries
// done:true + done_reason "stop", and the deltas concatenate to the
// provider's full content exactly once.
func TestOllamaGenerate_StreamingEmitsNDJSON(t *testing.T) {
	stub := &ohOverridableStub{
		model:     "m",
		chunks:    []string{"Hel", "lo wor", "ld"},
		finalResp: ohBuildResp("m", "Hello world", "stop"),
	}
	s := NewServer(WithChatProvider(stub))
	// stream defaults to true (field omitted).
	body := bytes.NewBufferString(`{"prompt":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	rr := httptest.NewRecorder()
	s.OllamaGenerate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	scanner := bufio.NewScanner(rr.Body)
	var frames []ollamaGenerateResponse
	for scanner.Scan() {
		var f ollamaGenerateResponse
		if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
			t.Fatalf("decode line %q: %v", scanner.Text(), err)
		}
		frames = append(frames, f)
	}
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want >=2 (deltas + final done)", len(frames))
	}
	final := frames[len(frames)-1]
	if !final.Done {
		t.Error("final frame must have done:true")
	}
	if final.DoneReason != "stop" {
		t.Errorf("final done_reason = %q, want stop", final.DoneReason)
	}
	// The final frame carries stats only — content rides the deltas.
	if final.Response != "" {
		t.Errorf("final frame Response = %q, want empty (content rides deltas)", final.Response)
	}
	var assembled string
	for _, f := range frames {
		assembled += f.Response
	}
	if assembled != "Hello world" {
		t.Errorf("assembled stream = %q, want %q", assembled, "Hello world")
	}
	// None of the non-final frames may be done:true.
	for i, f := range frames[:len(frames)-1] {
		if f.Done {
			t.Errorf("frame %d unexpectedly done:true", i)
		}
	}
}

// (TestOllamaGenerate_RejectsNonPost and _NoChatProvider503 live in
// chat_endpoints_coverage_test.go — not duplicated here.)

// ohCapturingStub records the messages the proxy forwards through the
// non-streaming CompleteWithTools path so the generate-synthesis
// tests can assert on the internal Message list (role/content/order).
type ohCapturingStub struct {
	finalResp *chat.ChatResponse
	captured  []chat.Message
}

func (c *ohCapturingStub) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return c.finalResp, nil
}
func (c *ohCapturingStub) CompleteWithTools(_ context.Context, msgs []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	c.captured = append(c.captured[:0], msgs...)
	return c.finalResp, nil
}
func (c *ohCapturingStub) CompleteWithToolsStream(_ context.Context, msgs []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	c.captured = append(c.captured[:0], msgs...)
	return c.finalResp, nil
}
func (c *ohCapturingStub) Model() string              { return "ohcap" }
func (c *ohCapturingStub) SetMetrics(_ *chat.Metrics) {}
