package agentbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two shipped workflows — companion-architectural-review and
// companion-doc-review — demand a STAGED INPUT ARTIFACT ("Review the STAGED
// INPUT, not memory"; the artifact lands under /app/input/uploads/). TaskSpec
// carried only {id, name, workflow, prompt, tier, scoring}, so the benchmark
// could submit a prompt and nothing else and those workflows were unreachable —
// recorded in the coverage design §5.1 as the cheapest coverage unlock, since
// the delegate tool already accepts inputArtifacts.
func TestDaemonRunner_AttachmentsAreStagedAsInputArtifacts(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "design.md")
	if err := os.WriteFile(fixture, []byte("# A design to review\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var gotArgs map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &envelope)
		if envelope.Params.Name == "delegate" {
			gotArgs = envelope.Params.Arguments
			_, _ = w.Write([]byte(`{"result":{"content":[{"type":"text","text":"{\"task_id\":\"t-1\",\"status\":\"QUEUED\"}"}]}}`))
			return
		}
		// status poll: report terminal immediately so Run returns.
		_, _ = w.Write([]byte(`{"result":{"content":[{"type":"text","text":"{\"task_id\":\"t-1\",\"status\":\"COMPLETED\"}"}]}}`))
	}))
	defer srv.Close()

	runner := NewDaemonTaskRunner(DaemonConfig{
		BaseURL: srv.URL, Token: "t", Project: "companionbench",
		Timeout: 5 * time.Second, PollInterval: time.Millisecond, HTTPClient: srv.Client(),
	})
	if _, err := runner.Run(context.Background(), TaskSpec{
		ID: "tw-review", Workflow: "companion-architectural-review",
		Prompt: "Review the attached design.", Tier: TaskTierTripwire,
		Attachments: []string{fixture},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	raw, ok := gotArgs["inputArtifacts"]
	if !ok {
		t.Fatal("delegate carried no inputArtifacts — a workflow demanding a staged " +
			"artifact cannot be benchmarked without them")
	}
	items, _ := raw.([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(items))
	}
	item, _ := items[0].(map[string]any)
	if item["name"] != "design.md" {
		t.Errorf("artifact name = %v, want design.md (the basename lands in the agent's uploads dir)", item["name"])
	}
	decoded, err := base64.StdEncoding.DecodeString(item["content"].(string))
	if err != nil {
		t.Fatalf("content must be base64: %v", err)
	}
	if string(decoded) != "# A design to review\n" {
		t.Errorf("content round-trip = %q", decoded)
	}
}

// A spec with no attachments must not send the field at all: delegate treats
// an empty inputArtifacts as a staging attempt, and every existing tripwire is
// prompt-only.
func TestDaemonRunner_NoAttachmentsSendsNoField(t *testing.T) {
	var gotArgs map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &envelope)
		if envelope.Params.Name == "delegate" {
			gotArgs = envelope.Params.Arguments
		}
		_, _ = w.Write([]byte(`{"result":{"content":[{"type":"text","text":"{\"task_id\":\"t-1\",\"status\":\"COMPLETED\"}"}]}}`))
	}))
	defer srv.Close()

	runner := NewDaemonTaskRunner(DaemonConfig{
		BaseURL: srv.URL, Token: "t", Project: "agentbench",
		Timeout: 5 * time.Second, PollInterval: time.Millisecond, HTTPClient: srv.Client(),
	})
	_, _ = runner.Run(context.Background(), TaskSpec{ID: "tw-plain", Workflow: "simple-workflow", Prompt: "go"})
	if _, present := gotArgs["inputArtifacts"]; present {
		t.Error("a prompt-only spec must not send inputArtifacts")
	}
}
