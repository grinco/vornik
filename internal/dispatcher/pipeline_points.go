package dispatcher

import (
	"context"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/pipeline"
)

// The chat agent's three pipeline points (2026-09-04-pipeline-points-design.md
// §3.1–§3.3), and today's gates registered as their first participants. The
// participants are the blocks that used to sit inline in Process and
// ProcessStreaming — moved verbatim, twice removed to once — and the order in
// which they run is now a declaration TestAgentPoints_ParticipantsArePinned
// holds, not a side effect of where a block sat in the loop.
//
// Every participant checks its own dependency (a nil judge, guard or
// detector is "not wired", not "not registered"), so the participant list is
// the same on every deployment and the tests can pin it.

// ToolCallInput is what the pre-tool and post-tool points see for one tool call.
type ToolCallInput struct {
	Tool, Arguments string
	ActiveProject   string
	ChatID          int64
}

// ToolOutcome is the post-tool point's output. Result is the tool's answer as
// executed — its Content is PRE-guard and is what the audit records; Content
// is what the model sees — post-guard; Warning is non-nil when the guard found
// something, and the loop appends it to the turn's warnings.
type ToolOutcome struct {
	Result  ToolResult
	Content string
	Warning *GuardWarning
}

// ContinuationInput is what the continuation point sees for a final reply.
type ContinuationInput struct {
	Text          string
	Messages      []chat.Message
	ActiveProject string
	ProjectIDs    []string
	ChatID        int64
	// RetryUsed is the loop's once-only state: the participant reads it, the
	// loop owns it.
	RetryUsed bool
	// Streaming selects the log line's wording; the two paths logged
	// differently before the conversion and still do.
	Streaming bool
	// RecordSignals is the turn audit's hook, supplied by the loop so the
	// participant stays free of per-turn state.
	RecordSignals func([]hallucination.Signal)
}

type agentPoints struct {
	preTool      *pipeline.Decide[ToolCallInput]
	postTool     *pipeline.Around[ToolCallInput, ToolOutcome]
	continuation *pipeline.Decide[ContinuationInput]
}

// zerologPipelineLogger adapts the agent's logger to pipeline.Logger.
type zerologPipelineLogger struct{ l zerolog.Logger }

func (z zerologPipelineLogger) Warn(msg string, args ...any) {
	ev := z.l.Warn()
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok {
			ev = ev.Interface(k, args[i+1])
		}
	}
	ev.Msg(msg)
}

// points builds the agent's chains on first use. Lazy rather than in NewAgent
// so an Agent assembled as a bare literal in a test still has its points.
func (a *Agent) points() *agentPoints {
	a.pointsOnce.Do(func() { a.pts = newAgentPoints(a) })
	return a.pts
}

func newAgentPoints(a *Agent) *agentPoints {
	log := zerologPipelineLogger{a.logger}
	p := &agentPoints{
		preTool:      pipeline.NewDecide[ToolCallInput](pipeline.DispatcherPreTool, log),
		postTool:     pipeline.NewAround[ToolCallInput, ToolOutcome](pipeline.DispatcherPostTool, log),
		continuation: pipeline.NewDecide[ContinuationInput](pipeline.DispatcherContinuation, log),
	}
	p.preTool.Register("intent_judge", a.intentJudgeParticipant)
	p.postTool.Register("output_guard", a.outputGuardParticipant)
	p.continuation.Register("hallucination_retry", a.hallucinationRetryParticipant)
	return p
}

// intentJudgeParticipant fires the heuristic verdict and (when the refiner is
// wired and risk meets the floor) the async LLM tier. The verdict is
// telemetry: it never refuses. Gating execution behind the recommendation is
// a one-line change here, not a new seam — which is why the point exists.
func (a *Agent) intentJudgeParticipant(ctx context.Context, in ToolCallInput) pipeline.Verdict {
	if a.intentJudge == nil {
		return pipeline.Verdict{}
	}
	var chatIDPtr *int64
	if in.ChatID != 0 {
		id := in.ChatID
		chatIDPtr = &id
	}
	v := a.intentJudge.evaluate(ctx, in.ActiveProject, nil, nil, chatIDPtr,
		in.Tool, in.Arguments, nil, a.logger)
	a.logger.Info().
		Str("tool", in.Tool).
		Str("risk", string(v.Risk)).
		Str("rec", string(v.Recommendation)).
		Float64("confidence", v.Confidence).
		Msg("intent judge: heuristic verdict")
	return pipeline.Verdict{}
}

// outputGuardParticipant runs the tool, then rewrites what the model will see
// through the guard with the result's provenance. HIGH findings are redacted
// in place (when configured); every severity rides back as a warning. The
// audit keeps the pre-guard bytes in Result.
func (a *Agent) outputGuardParticipant(ctx context.Context, in ToolCallInput, next pipeline.Next[ToolCallInput, ToolOutcome]) (ToolOutcome, error) {
	out, err := next(ctx, in)
	if err != nil || a.outputGuard == nil {
		return out, err
	}
	content, w := a.outputGuard.applyOutputGuard(in.Tool, out.Result.Content, out.Result.Provenance, a.metrics)
	out.Content = content
	if w.MaxSeverity != "" {
		a.logger.Warn().
			Str("tool", w.Tool).
			Str("severity", string(w.MaxSeverity)).
			Strs("kinds", w.Kinds).
			Bool("redacted", w.Redacted).
			Msg("output guard: findings on tool result")
		out.Warning = &w
	}
	return out, nil
}

// hallucinationRetryParticipant scans the final reply. Blocking signals with
// no retry used yet → Retry with the synthetic user prompt; with the retry
// already burned → Banner. It never refuses.
func (a *Agent) hallucinationRetryParticipant(ctx context.Context, in ContinuationInput) pipeline.Verdict {
	if a.hallucinationDetector == nil {
		return pipeline.Verdict{}
	}
	gc := a.buildChatGroundingContext(ctx, in.Messages, in.ProjectIDs, in.ActiveProject)
	signals := a.hallucinationDetector.Scan(in.Text, gc)
	a.hallucinationMetrics.ObserveSignals(in.ActiveProject, signals)
	if in.RecordSignals != nil {
		in.RecordSignals(signals)
	}
	blocking := retainBlockingSignals(signals)
	if len(blocking) == 0 {
		return pipeline.Verdict{}
	}
	msg := "dispatcher: hallucination detected in reply"
	if in.Streaming {
		msg = "dispatcher: hallucination detected in streaming reply"
	}
	a.logger.Warn().
		Int64("chat_id", in.ChatID).
		Int("signals", len(signals)).
		Int("blocking", len(blocking)).
		Bool("retry_used", in.RetryUsed).
		Msg(msg)
	if !in.RetryUsed {
		return pipeline.Verdict{Retry: formatHallucinationRetryPrompt(blocking)}
	}
	return pipeline.Verdict{Banner: formatUserWarningBanner(blocking)}
}
