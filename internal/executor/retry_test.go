package executor

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestIsShapeFailure_ClassifiesCorrectly — the matcher decides whether
// a step error should trigger a corrective re-run. False positives
// retry timeouts (wasted spend); false negatives leave shape errors
// uncovered (the bug we're fixing). Lock the boundary in tests.
func TestIsShapeFailure_ClassifiesCorrectly(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// Positive cases — these mirror real error messages emitted
		// from container.go (schema validation) and plan_step.go
		// (lead plan parse).
		{"missing required keys", errors.New(`schema violation: role "writer" result.json is missing required keys: [writing]`), true},
		{"plan parse failure", errors.New(`could not parse plan from lead output: invalid character`), true},
		{"result.json unmarshal", errors.New(`failed to unmarshal result.json: unexpected EOF`), true},
		// Plausibility violations are now in scope for retry — they
		// mean "JSON shape is fine but the values contradict each
		// other" and a re-prompt with the violation text fixes most.
		{"plausibility violation", errors.New(`plausibility violation: role "reviewer" failed 1 rule(s): approved_needs_feedback: feedback empty when approved=false`), true},
		// Negative cases — these reflect environmental/content
		// problems where re-prompting won't help.
		{"context timeout", errors.New(`context deadline exceeded`), false},
		{"container exited 1", errors.New(`container exited with code 1`), false},
		{"agent verification", errors.New(`agent claimed 1 file(s) but verification failed: "x.md" file does not exist`), false},
		{"network", errors.New(`gateway error 502: bad gateway`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isShapeFailure(tc.err)
			if got != tc.want {
				t.Fatalf("got %v, want %v for %q", got, tc.want, errString(tc.err))
			}
		})
	}
}

// TestClassifyShapeFailure_PicksRightHint — the retry layer uses the
// failure kind to choose which corrective hint to attach. JSON
// failures get a "respond only with JSON" message; plausibility
// failures need a different framing because the JSON was already
// fine. Mis-classifying causes the wrong nudge and frequently a
// regression on the second attempt — lock the mapping in tests.
func TestClassifyShapeFailure_PicksRightHint(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want shapeFailureKind
	}{
		{"nil", nil, shapeFailureNone},
		{"schema", errors.New(`schema violation: role "x" result.json is missing required keys: [y]`), shapeFailureJSON},
		{"plan parse", errors.New(`could not parse plan from lead output: invalid`), shapeFailureJSON},
		{"plausibility", errors.New(`plausibility violation: role "x" failed 1 rule(s): r: detail`), shapeFailurePlausibility},
		{"timeout", errors.New(`context deadline exceeded`), shapeFailureNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyShapeFailure(tc.err)
			if got != tc.want {
				t.Fatalf("classifyShapeFailure(%q) = %d, want %d", errString(tc.err), got, tc.want)
			}
		})
	}
}

// TestIsModelShapedFailure_FallbackTriggers — the fallback layer
// only fires when a different model could plausibly help. Lock the
// boundary so a future error-message change doesn't silently retry
// (or silently miss) a class.
func TestIsModelShapedFailure_FallbackTriggers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"schema", errors.New(`schema violation: role "x" missing keys`), true},
		{"plausibility", errors.New(`plausibility violation: feedback empty`), true},
		{"iteration limit", errors.New(`agent reported FAILED status: Tool iteration limit (50) reached`), true},
		{"provider error", errors.New(`PROVIDER_ERROR: upstream gave up after 3 attempts`), true},
		// Outside the model-shaped class — the agent's claim was a lie,
		// no different model produces a different filesystem.
		{"agent verification", errors.New(`agent claimed 1 file(s) but verification failed: x.md does not exist`), false},
		{"timeout", errors.New(`context deadline exceeded`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isModelShapedFailure(tc.err)
			if got != tc.want {
				t.Fatalf("got %v, want %v for %q", got, tc.want, errString(tc.err))
			}
		})
	}
}

// TestPlausibilityRetryHint_DistinctFromShape — the two hints must
// not be interchangeable: shapeRetryHint tells the model to fix its
// JSON, plausibilityRetryHint tells the model the JSON was fine and
// to fix the values. A future copy-paste regression that aliases
// them would re-introduce the "regression on retry" failure mode
// the split hints exist to prevent.
func TestPlausibilityRetryHint_DistinctFromShape(t *testing.T) {
	if shapeRetryHint == plausibilityRetryHint {
		t.Fatal("shapeRetryHint and plausibilityRetryHint must differ — see retry.go doc")
	}
	if !strings.Contains(plausibilityRetryHint, "plausibility") {
		t.Fatal("plausibilityRetryHint should mention plausibility so the operator can grep failure logs")
	}
	if !strings.Contains(plausibilityRetryHint, "Same JSON structure") {
		t.Fatal("plausibilityRetryHint should reassure the model its JSON shape was correct")
	}
}

// TestTruncateForPrompt_BoundsBudget — corrective prompts get appended
// to the next request body; an unbounded error message could push the
// retry over context limits. The truncator must clip and signal
// truncation so the model knows the message was cut.
func TestTruncateForPrompt_BoundsBudget(t *testing.T) {
	short := "boom"
	if got := truncateForPrompt(short, 100); got != "boom" {
		t.Fatalf("short string mangled: %q", got)
	}

	long := strings.Repeat("x", 1000)
	got := truncateForPrompt(long, 200)
	if len(got) <= 200 {
		t.Fatalf("expected something longer than 200 (with ellipsis suffix), got %d", len(got))
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("missing truncation marker: %q", got[len(got)-30:])
	}
	// Body before the suffix should be exactly the limit.
	prefix := strings.TrimSuffix(got, "…(truncated)")
	if len(prefix) != 200 {
		t.Fatalf("prefix len %d, want 200", len(prefix))
	}
}

// TestShapeRetryHint_FormattedWithError — the corrective prompt must
// inline the failure reason so the model knows what to fix. The
// instruction set ("no prose, no fences, no preamble") matches the
// failure modes seen in the field.
func TestShapeRetryHint_FormattedWithError(t *testing.T) {
	hint := strings.ReplaceAll(shapeRetryHint, "%s", "missing key: writing")
	for _, want := range []string{
		"failed schema validation",
		"missing key: writing",
		"No prose",
		"No markdown code fences",
		"required key",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q: %s", want, hint)
		}
	}
}

// TestExtractPriorMessage_HappyPath — when the prior result.json carries
// a substantive `message` field, extractPriorMessage returns it (post-
// strip) so the corrective hint can re-anchor the next attempt on the
// model's own prior reasoning. Captures the failure mode where a
// risk-officer wrote 2KB of approval prose, failed shape validation,
// and on retry collapsed to empty arrays — the anchor stops that.
func TestExtractPriorMessage_HappyPath(t *testing.T) {
	prior := `Now I review the proposals. Three approved: AAPL qty=3 stop $254.63, MSFT qty=2 stop $365.86, NVO qty=2 stop $80.50. All within risk caps; no correlation conflicts.`
	resultJSON := []byte(`{"status":"COMPLETED","message":` + jsonString(prior) + `}`)
	got := extractPriorMessage(resultJSON)
	if !strings.Contains(got, "AAPL qty=3") {
		t.Fatalf("prior message lost: %q", got)
	}
}

// TestExtractPriorMessage_StripsThinking — reasoning blocks must not
// leak into the corrective hint. They're already noise in the
// downstream-step handover; double-feeding them on retry just dilutes
// the substantive content the model should re-format.
func TestExtractPriorMessage_StripsThinking(t *testing.T) {
	prior := `<think>I should approve all three proposals after a careful audit of caps and correlation.</think>{"approved":[{"symbol":"AAPL","qty":3,"stop_loss_price":254.63}],"rejected":[],"has_approvals":true,"has_rejections":false}`
	resultJSON := []byte(`{"status":"COMPLETED","message":` + jsonString(prior) + `}`)
	got := extractPriorMessage(resultJSON)
	if strings.Contains(got, "<think>") {
		t.Fatalf("thinking block leaked into anchor: %q", got)
	}
	if !strings.Contains(got, "AAPL") {
		t.Fatalf("substantive content stripped: %q", got)
	}
}

// TestExtractPriorMessage_SkipsShortMessages — error-string-shaped
// messages ("FAILED", "LLM call failed: ...") are too short to
// re-anchor on, and feeding them back makes the corrective hint
// confusing ("re-format this error message"). The 50-char threshold
// excludes those without dropping genuine reasoning.
func TestExtractPriorMessage_SkipsShortMessages(t *testing.T) {
	for _, msg := range []string{
		"",
		"FAILED",
		"LLM call failed: timeout",
		"   short  ",
	} {
		resultJSON := []byte(`{"message":` + jsonString(msg) + `}`)
		got := extractPriorMessage(resultJSON)
		if got != "" {
			t.Errorf("expected empty anchor for short message %q, got %q", msg, got)
		}
	}
}

// TestExtractPriorMessage_TruncatesLong — the anchor is bounded so the
// corrective hint can't push the next request over the gateway's size
// envelope. Truncation must include a signal so the model knows the
// message was clipped (otherwise it might invent content past the cut).
func TestExtractPriorMessage_TruncatesLong(t *testing.T) {
	long := strings.Repeat("x", priorAttemptMaxChars*2)
	resultJSON := []byte(`{"message":` + jsonString(long) + `}`)
	got := extractPriorMessage(resultJSON)
	if len(got) <= priorAttemptMaxChars {
		t.Fatalf("expected truncated length > cap (with marker), got %d", len(got))
	}
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Fatalf("truncation marker missing: %q", got[len(got)-30:])
	}
}

// TestExtractPriorMessage_MalformedJSON — bad JSON should produce no
// anchor (rather than crashing), so the corrective hint falls back to
// the schema-only nudge. The result.json could legitimately be
// malformed when the agent crashed mid-write.
func TestExtractPriorMessage_MalformedJSON(t *testing.T) {
	for _, bad := range [][]byte{
		nil,
		[]byte(""),
		[]byte("not-json"),
		[]byte(`{"unterminated": "string`),
	} {
		got := extractPriorMessage(bad)
		if got != "" {
			t.Errorf("expected empty anchor for malformed JSON %q, got %q", bad, got)
		}
	}
}

// jsonString quotes s as a JSON string literal so it can be embedded
// in a hand-built result.json fixture without dragging in encoding/json
// at the call site.
func jsonString(s string) string {
	out := `"`
	for _, r := range s {
		switch r {
		case '"':
			out += `\"`
		case '\\':
			out += `\\`
		case '\n':
			out += `\n`
		case '\r':
			out += `\r`
		case '\t':
			out += `\t`
		default:
			out += string(r)
		}
	}
	return out + `"`
}

func errString(e error) string {
	if e == nil {
		return "<nil>"
	}
	return e.Error()
}

// TestIsInfraFailure_ClassifiesCorrectly — the matcher decides
// whether a step error should trigger the same-inputs retry
// (no prompt change). False positives waste retry budget on
// permanent failures; false negatives leave transient
// infrastructure errors uncovered (the bug we're fixing).
func TestIsInfraFailure_ClassifiesCorrectly(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// curl exit codes — agent's chat-proxy call lost its connection.
		{"curl 6 dns", errors.New(`agent reported FAILED status: LLM call failed: curl failed (exit 6): curl: (6) Could not resolve host`), true},
		{"curl 7 connection refused", errors.New(`agent reported FAILED: curl: (7) Failed to connect to host.containers.internal port 8080`), true},
		{"curl 28 timeout", errors.New(`curl: (28) Operation timed out`), true},
		{"curl 52 empty reply", errors.New(`curl: (52) Empty reply from server`), true},
		// Gateway transients.
		{"gateway 502", errors.New(`gateway error 502: Bad Gateway`), true},
		{"provider error", errors.New(`PROVIDER_ERROR: bedrock unavailable`), true},
		// Native go HTTP-style messages.
		{"connection refused", errors.New(`dial tcp 127.0.0.1:8080: connection refused`), true},
		{"i/o timeout", errors.New(`Get "http://upstream": net/http: request canceled (Client.Timeout exceeded while awaiting headers): i/o timeout`), true},
		// Negative cases — these are NOT transient and shouldn't be retried with the same inputs.
		{"schema violation", errors.New(`schema violation: role "writer" result.json is missing required keys: [writing]`), false},
		{"agent claimed lie", errors.New(`agent claimed 1 file(s) but verification failed: file does not exist`), false},
		{"context canceled", errors.New(`context canceled`), false},
		{"random content failure", errors.New(`agent reported FAILED status: I cannot complete this task`), false},
		// retryableError stays task-level — must NOT be caught here.
		{"retryable container-start", retryableError{err: errors.New(`failed to start container: image pull error`)}, false},
		{"retryable warm pool", retryableError{err: errors.New(`warm pool unavailable: full`)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isInfraFailure(tc.err)
			if got != tc.want {
				t.Fatalf("got %v, want %v for %q", got, tc.want, errString(tc.err))
			}
		})
	}
}

// TestInfraRetry_BudgetCoversProxyWarmup — pins the total sleep
// budget so a future "tighten this" doesn't reintroduce the 2026-05-14
// regression where the daemon-restart proxy warmup outran the retry
// budget. The chat proxy + IBGateway + scraper stack can take ~30s
// to be fully ready post-restart on a loaded host; the budget must
// comfortably exceed that.
func TestInfraRetry_BudgetCoversProxyWarmup(t *testing.T) {
	const minWarmupBudget = 30 * time.Second

	// Replay the sleep math the loop would emit and sum.
	total := time.Duration(0)
	cur := infraRetryBaseDelay
	for i := 1; i < infraRetryMaxAttempts; i++ { // attempt 1 has no sleep
		total += cur
		next := cur * 2
		if next > infraRetryMaxDelay {
			next = infraRetryMaxDelay
		}
		cur = next
	}
	if total < minWarmupBudget {
		t.Fatalf("infra-retry sleep budget %v < required warmup window %v "+
			"(infraRetryMaxAttempts=%d, base=%v, cap=%v) — "+
			"this regresses the 2026-05-14 chat-proxy warmup race",
			total, minWarmupBudget, infraRetryMaxAttempts,
			infraRetryBaseDelay, infraRetryMaxDelay)
	}
}

// TestInfraRetry_BackoffSchedule — the backoff sequence must
// double until it hits the cap, then stay there. Verifies the
// sleep math without spawning real containers (which would need
// the full Executor + mocks).
func TestInfraRetry_BackoffSchedule(t *testing.T) {
	// Compute the same way the loop does and assert the bounds.
	delays := []time.Duration{infraRetryBaseDelay}
	for len(delays) < infraRetryMaxAttempts {
		next := delays[len(delays)-1] * 2
		if next > infraRetryMaxDelay {
			next = infraRetryMaxDelay
		}
		delays = append(delays, next)
	}
	// First delay = base.
	if delays[0] != infraRetryBaseDelay {
		t.Errorf("first delay = %v, want %v", delays[0], infraRetryBaseDelay)
	}
	// Doubles until cap.
	for i := 1; i < len(delays); i++ {
		want := delays[i-1] * 2
		if want > infraRetryMaxDelay {
			want = infraRetryMaxDelay
		}
		if delays[i] != want {
			t.Errorf("delay[%d] = %v, want %v (sequence: %v)", i, delays[i], want, delays)
		}
	}
	// Last delay never exceeds the cap.
	if delays[len(delays)-1] > infraRetryMaxDelay {
		t.Errorf("last delay %v exceeds cap %v", delays[len(delays)-1], infraRetryMaxDelay)
	}
}
