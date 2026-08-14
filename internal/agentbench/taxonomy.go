package agentbench

import (
	"errors"
	"strings"

	"vornik.io/vornik/internal/chat"
)

// Failure taxonomy (§4).
//
// WHY A BLENDED SUCCESS RATE IS WORTHLESS. "95.9% success" absorbs a provider
// outage, a context-window overflow and an agent that genuinely could not do the
// work into one number that answers no question anyone has. Broken out it is
// both more honest and a stronger claim: "97.1% task success, 1.2% infra" reads
// better than the blend it came from.
//
// DEVIATION FROM §4 AS DRAFTED, deliberate. The design listed context-window
// overflow as a flavour of infra failure. It is not, and for THIS benchmark the
// distinction is the whole point: an overflow is neither the agent failing nor
// the provider failing — it is the context policy under test failing at its job.
// Bucketing it as infra would discard the single most interesting failure signal
// a context-management benchmark can produce. It gets its own class.
//
// This also encodes the 2026-07-12 incident, where a deterministic
// ValidationException ("maximum context length is 202752 tokens... requested
// 8192 output + 194561 input") was sanitised into a generic PROVIDER_ERROR 502
// and re-sent ~12x by the retry ladder. chat.IsUpstreamInfraError already checks
// overflow first and returns false for it; this classifier reuses that function
// rather than copying its markers, so the two can never disagree.

// FailureClass is why an execution did not succeed.
type FailureClass string

const (
	// FailureNone is a successful execution.
	FailureNone FailureClass = "none"
	// FailureTask is the agent failing the work: wrong answer, unmet criteria,
	// gave up. The only class that reflects on agent quality.
	FailureTask FailureClass = "task"
	// FailureContextOverflow is the prompt exceeding the model's window. A
	// policy result, not an outage — see the deviation note above.
	FailureContextOverflow FailureClass = "context_overflow"
	// FailureInfra is the environment failing: provider errors, DNS, TLS,
	// timeouts, lease loss.
	FailureInfra FailureClass = "infra"
	// FailureHarness is the benchmark itself failing — a missing gold entry, a
	// trace that could not be assembled. Never counted against the system under
	// test, because measuring our own bug as a Vornik regression is how a
	// benchmark starts lying.
	FailureHarness FailureClass = "harness"
)

// harnessMarkers identify a failure in the measurement rather than the measured.
var harnessMarkers = []string{
	"gold",
	"no recorded paths",
	"excluded from gold",
	"assemble trace",
}

// leaseMarkers identify lease loss, which is infra: the daemon lost the
// execution's lease, so the agent never got to fail on its own merits.
var leaseMarkers = []string{
	"lease not found",
	"lease expired",
}

// ClassifyFailure maps a persisted execution outcome to its class.
//
// Takes the recorded error TEXT rather than an error value, because it runs over
// the ledger after the fact and a persisted row has no type. The text is wrapped
// so the shared chat classifiers can be reused verbatim: their marker-based paths
// work on any error, and reusing them is what keeps this taxonomy and the
// executor's retry ladder from drifting apart about what "infra" means.
//
// ORDER MATTERS AND MIRRORS chat.IsUpstreamInfraError: overflow is decided
// before infra, because a wrapping layer can glue an infra-looking marker onto a
// deterministic overflow — which is exactly what produced the 2026-07-12 retry
// storm.
func ClassifyFailure(succeeded bool, errText string) FailureClass {
	if succeeded {
		return FailureNone
	}
	msg := strings.TrimSpace(errText)
	if msg == "" {
		// A failure with no recorded reason is not evidence the agent failed.
		// Attributing it to the agent would inflate exactly the number this
		// benchmark exists to report honestly.
		return FailureHarness
	}
	err := errors.New(msg)
	lower := strings.ToLower(msg)

	for _, m := range harnessMarkers {
		if strings.Contains(lower, m) {
			return FailureHarness
		}
	}
	if chat.IsContextOverflow(err) {
		return FailureContextOverflow
	}
	for _, m := range leaseMarkers {
		if strings.Contains(lower, m) {
			return FailureInfra
		}
	}
	if chat.IsUpstreamInfraError(err) {
		return FailureInfra
	}
	return FailureTask
}

// OverflowSource distinguishes an overflow VORNIK caught from one the provider
// rejected. Round-4 review found the narrow gap this closes: filing every
// overflow as a policy failure would misattribute a provider-side rejection of a
// request that was inside our own budget.
type OverflowSource string

const (
	// OverflowSourceUnknown means the text did not say which side caught it.
	OverflowSourceUnknown OverflowSource = ""
	// OverflowSourcePolicy is our own budget or proxy refusing before the call —
	// unambiguously the context policy under test failing.
	OverflowSourcePolicy OverflowSource = "policy"
	// OverflowSourceProvider is the provider rejecting a request that got past
	// our budget. Still a policy signal (our budget was wrong about the
	// window), but it is a DIFFERENT fix — a model_limits entry rather than a
	// compaction change — so it is not blended with the former.
	OverflowSourceProvider OverflowSource = "provider"
)

// policyOverflowMarkers are the strings only OUR side emits: the chat proxy's
// 400 and the executor wrapper's budget outcome.
var policyOverflowMarkers = []string{
	"context_overflow",
	"prompt exceeds the model's context window",
	"prompt_token_budget",
}

// ClassifyOverflowSource reports which side caught an overflow. Meaningful only
// for text that already classified as FailureContextOverflow.
func ClassifyOverflowSource(errText string) OverflowSource {
	msg := strings.ToLower(strings.TrimSpace(errText))
	if msg == "" {
		return OverflowSourceUnknown
	}
	for _, m := range policyOverflowMarkers {
		if strings.Contains(msg, m) {
			return OverflowSourcePolicy
		}
	}
	if chat.IsContextOverflow(errors.New(errText)) {
		return OverflowSourceProvider
	}
	return OverflowSourceUnknown
}

// SuccessBreakdown is a run's outcomes by class. Published as the breakdown, not
// as a single rate.
type SuccessBreakdown struct {
	Attempted int                  `json:"attempted"`
	Succeeded int                  `json:"succeeded"`
	ByClass   map[FailureClass]int `json:"byClass"`
}

// Add records one execution's outcome.
func (b *SuccessBreakdown) Add(class FailureClass) {
	if b.ByClass == nil {
		b.ByClass = map[FailureClass]int{}
	}
	b.Attempted++
	if class == FailureNone {
		b.Succeeded++
	}
	b.ByClass[class]++
}

// TaskSuccessRate is successes over attempts EXCLUDING harness failures.
//
// A benchmark bug is not the system under test failing, so counting it against
// the system would understate quality; counting it as a success would overstate
// it. It leaves the denominator, and the harness-failure count is published
// beside the rate so a run with many of them is visibly untrustworthy rather
// than quietly flattering.
func (b SuccessBreakdown) TaskSuccessRate() (rate float64, defined bool) {
	denom := b.Attempted - b.ByClass[FailureHarness]
	if denom <= 0 {
		return 0, false
	}
	return float64(b.Succeeded) / float64(denom), true
}
