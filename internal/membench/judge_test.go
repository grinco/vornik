package membench

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Answer generator and judge (design §5.6, §5.9).
//
// Both sides get an IDENTICAL answer prompt and an identical judge prompt, with
// per-category judge variants because a single grading rule is wrong for these
// categories — most sharply for temporal reasoning (off-by-one on day counts is
// correct) and abstention (refusing to answer is correct).

// stubLLM returns canned completions and records what it was asked.
type stubLLM struct {
	replies []string
	calls   []string
	err     error
}

func (s *stubLLM) Complete(_ context.Context, prompt string) (string, error) {
	s.calls = append(s.calls, prompt)
	if s.err != nil {
		return "", s.err
	}
	if len(s.replies) == 0 {
		return "", errors.New("stub exhausted")
	}
	out := s.replies[0]
	s.replies = s.replies[1:]
	return out, nil
}

// ---------------------------------------------------------------------------
// answer generator
// ---------------------------------------------------------------------------

// TestAnswerGenerator_PassesRecalledContextAndQuestion — the generator answers
// from retrieved context only. If it could answer without it, the benchmark would
// be measuring the model's world knowledge rather than the memory system.
func TestAnswerGenerator_PassesRecalledContextAndQuestion(t *testing.T) {
	llm := &stubLLM{replies: []string{"Alice works at Google."}}
	g := NewAnswerGenerator(llm)

	got, err := g.Answer(context.Background(), "Where does Alice work?", []Hit{
		{SourceID: "s1", Text: "I just started at Google."},
		{SourceID: "s3", Text: "My commute is 40 minutes."},
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got != "Alice works at Google." {
		t.Errorf("Answer = %q, want the model's completion verbatim", got)
	}

	prompt := llm.calls[0]
	for _, want := range []string{"Where does Alice work?", "I just started at Google.", "My commute is 40 minutes."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt omits %q", want)
		}
	}
}

// TestAnswerGenerator_EmptyContextStillAnswers — a recall that found nothing must
// still produce an attempt, so the judge scores it as incorrect rather than the
// runner recording an error. An empty recall IS a retrieval failure and belongs in
// the accuracy denominator.
func TestAnswerGenerator_EmptyContextStillAnswers(t *testing.T) {
	llm := &stubLLM{replies: []string{"I don't know."}}
	g := NewAnswerGenerator(llm)

	got, err := g.Answer(context.Background(), "Where does Alice work?", nil)
	if err != nil {
		t.Fatalf("Answer with empty context: %v", err)
	}
	if got == "" {
		t.Error("empty answer; an empty recall must still yield a gradeable attempt")
	}
}

// TestAnswerGenerator_PromptHashStable — the hash goes in the comparability key,
// so it must be stable across runs and change when the template changes.
func TestAnswerGenerator_PromptHashStable(t *testing.T) {
	a := AnswerPromptSHA256()
	if a == "" {
		t.Fatal("empty prompt hash")
	}
	if a != AnswerPromptSHA256() {
		t.Error("prompt hash is not stable across calls")
	}
}

// TestAnswerGenerator_LLMErrorPropagates — an LLM fault is an error outcome, not
// a wrong answer, so it must reach the runner rather than being swallowed into an
// empty string.
func TestAnswerGenerator_LLMErrorPropagates(t *testing.T) {
	g := NewAnswerGenerator(&stubLLM{err: errors.New("upstream 503")})
	if _, err := g.Answer(context.Background(), "q", nil); err == nil {
		t.Error("an LLM failure was swallowed; it would be scored as a wrong answer")
	}
}

// ---------------------------------------------------------------------------
// judge
// ---------------------------------------------------------------------------

// TestJudge_ParsesVerdict — the happy path, both ways.
func TestJudge_ParsesVerdict(t *testing.T) {
	t.Run("correct", func(t *testing.T) {
		j := NewJudge(&stubLLM{replies: []string{`{"reasoning":"matches","correct":true}`}})
		got, err := j.Judge(context.Background(), JudgeRequest{
			Category: "multi-session", Question: "q", GoldAnswer: "Google", Answer: "Google",
		})
		if err != nil {
			t.Fatalf("Judge: %v", err)
		}
		if got != OutcomeCorrect {
			t.Errorf("outcome = %s, want correct", got)
		}
	})
	t.Run("incorrect", func(t *testing.T) {
		j := NewJudge(&stubLLM{replies: []string{`{"reasoning":"wrong company","correct":false}`}})
		got, err := j.Judge(context.Background(), JudgeRequest{
			Category: "multi-session", Question: "q", GoldAnswer: "Google", Answer: "Meta",
		})
		if err != nil {
			t.Fatalf("Judge: %v", err)
		}
		if got != OutcomeIncorrect {
			t.Errorf("outcome = %s, want incorrect", got)
		}
	})
}

// TestJudge_ToleratesFencedJSON — models wrap JSON in code fences constantly.
// Failing on that would inflate the invalid rate for a formatting habit rather
// than a grading problem.
func TestJudge_ToleratesFencedJSON(t *testing.T) {
	j := NewJudge(&stubLLM{replies: []string{
		"Here is my verdict:\n```json\n{\"reasoning\":\"ok\",\"correct\":true}\n```\n",
	}})
	got, err := j.Judge(context.Background(), JudgeRequest{Category: "multi-session"})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got != OutcomeCorrect {
		t.Errorf("outcome = %s, want correct from fenced JSON", got)
	}
}

// TestJudge_RetriesOnceThenInvalid — unparseable output gets exactly one retry,
// then scores invalid. Never incorrect: we do not know what the verdict was, and
// guessing would blame retrieval for the judge's formatting.
func TestJudge_RetriesOnceThenInvalid(t *testing.T) {
	llm := &stubLLM{replies: []string{"complete gibberish", "still gibberish"}}
	j := NewJudge(llm)

	got, err := j.Judge(context.Background(), JudgeRequest{Category: "multi-session"})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got != OutcomeInvalid {
		t.Errorf("outcome = %s, want invalid — an unparseable verdict must never "+
			"be scored as incorrect", got)
	}
	if len(llm.calls) != 2 {
		t.Errorf("made %d calls, want exactly 2 (one retry)", len(llm.calls))
	}
}

// TestJudge_RetrySucceeds — a transient formatting slip should not cost the item.
func TestJudge_RetrySucceeds(t *testing.T) {
	llm := &stubLLM{replies: []string{"gibberish", `{"correct":true}`}}
	j := NewJudge(llm)

	got, err := j.Judge(context.Background(), JudgeRequest{Category: "multi-session"})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got != OutcomeCorrect {
		t.Errorf("outcome = %s, want correct on the retry", got)
	}
}

// TestJudge_LLMErrorIsErrorNotInvalid — an HTTP fault is infrastructure, and must
// be distinguishable from a judge that answered unintelligibly.
func TestJudge_LLMErrorIsErrorNotInvalid(t *testing.T) {
	j := NewJudge(&stubLLM{err: errors.New("upstream 503")})
	if _, err := j.Judge(context.Background(), JudgeRequest{Category: "multi-session"}); err == nil {
		t.Error("an LLM failure was reported as a verdict rather than an error")
	}
}

// TestJudge_PerCategoryPromptsDiffer is the fairness requirement from §5.6: a
// single grading rule is wrong for these categories, so each must get its own
// instruction.
func TestJudge_PerCategoryPromptsDiffer(t *testing.T) {
	seen := map[string]string{}
	for _, cat := range []string{
		"multi-session", "temporal-reasoning", "knowledge-update",
		"single-session-preference", "unanswerable",
	} {
		p := judgePromptFor(cat)
		if p == "" {
			t.Errorf("category %q has no judge prompt", cat)
			continue
		}
		for other, prev := range seen {
			if prev == p {
				t.Errorf("categories %q and %q share a judge prompt; their grading "+
					"rules genuinely differ", cat, other)
			}
		}
		seen[cat] = p
	}
}

// TestJudge_TemporalPromptForgivesOffByOne — the specific rule that makes the
// temporal category gradeable at all. Without it, a correct reasoning chain that
// counts 19 days instead of 18 scores as a retrieval failure.
func TestJudge_TemporalPromptForgivesOffByOne(t *testing.T) {
	p := strings.ToLower(judgePromptFor("temporal-reasoning"))
	if !strings.Contains(p, "off-by-one") {
		t.Error("the temporal-reasoning judge prompt does not forgive off-by-one " +
			"day counts, so arithmetic slips would be scored as retrieval failures")
	}
}

// TestJudge_UnanswerablePromptRewardsAbstention — for these items the CORRECT
// behaviour is declining to answer. A default prompt would score every honest
// refusal as wrong and reward confident fabrication.
func TestJudge_UnanswerablePromptRewardsAbstention(t *testing.T) {
	p := strings.ToLower(judgePromptFor("unanswerable"))
	if !strings.Contains(p, "abstain") && !strings.Contains(p, "not know") &&
		!strings.Contains(p, "no information") {
		t.Errorf("the unanswerable judge prompt does not treat abstention as "+
			"correct; it would reward fabrication. Prompt: %q", p)
	}
}

// TestJudge_KnowledgeUpdatePromptAcceptsSupersededAlongside — the update category
// tolerates the old value being mentioned as long as the new one is present.
func TestJudge_KnowledgeUpdatePromptAcceptsSupersededAlongside(t *testing.T) {
	p := strings.ToLower(judgePromptFor("knowledge-update"))
	if !strings.Contains(p, "updated") {
		t.Error("the knowledge-update judge prompt does not distinguish the updated " +
			"answer from superseded information")
	}
}

// TestJudge_PreferencePromptUsesRubric — preference questions are graded against
// a rubric, not string equality.
func TestJudge_PreferencePromptUsesRubric(t *testing.T) {
	p := strings.ToLower(judgePromptFor("single-session-preference"))
	if !strings.Contains(p, "rubric") && !strings.Contains(p, "desired") {
		t.Error("the preference judge prompt does not grade against a rubric")
	}
}

// TestJudge_UnknownCategoryFallsBackNotEmpty — an unrecognised category must get
// the default rule rather than an empty prompt, which would make every item in it
// invalid.
func TestJudge_UnknownCategoryFallsBackNotEmpty(t *testing.T) {
	if p := judgePromptFor("some-new-category"); p == "" {
		t.Error("an unknown category yielded an empty prompt; every item in it " +
			"would score invalid")
	}
}

// TestJudge_PromptCarriesTheRequest — a prompt missing the answer or gold answer
// would have the judge grading in the dark.
func TestJudge_PromptCarriesTheRequest(t *testing.T) {
	llm := &stubLLM{replies: []string{`{"correct":true}`}}
	j := NewJudge(llm)
	_, err := j.Judge(context.Background(), JudgeRequest{
		Category:   "multi-session",
		Question:   "Where does Alice work?",
		GoldAnswer: "Google",
		Answer:     "She works at Google.",
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	for _, want := range []string{"Where does Alice work?", "Google", "She works at Google."} {
		if !strings.Contains(llm.calls[0], want) {
			t.Errorf("judge prompt omits %q", want)
		}
	}
}

// TestJudgePromptSHA256_CoversEveryVariant — the comparability key must change
// when ANY category's grading rule changes, not just the default one.
func TestJudgePromptSHA256_CoversEveryVariant(t *testing.T) {
	h := JudgePromptSHA256()
	if h == "" {
		t.Fatal("empty judge prompt hash")
	}
	if h != JudgePromptSHA256() {
		t.Error("judge prompt hash is not stable")
	}
	// The digest must incorporate every variant; a hash over only the default
	// would let a temporal-rule edit pass as comparable.
	for _, cat := range []string{"temporal-reasoning", "unanswerable"} {
		if !strings.Contains(judgePromptCorpus(), judgePromptFor(cat)) {
			t.Errorf("the hashed corpus omits the %q variant", cat)
		}
	}
}

// TestJudge_MalformedJSONObjectIsInvalid — a reply that LOOKS like JSON but does
// not parse must score invalid, not incorrect. The distinction is the whole point
// of the taxonomy: we never learned what the judge thought.
func TestJudge_MalformedJSONObjectIsInvalid(t *testing.T) {
	llm := &stubLLM{replies: []string{`{correct: yes, reasoning: unquoted}`, `{still: bad}`}}
	j := NewJudge(llm)

	got, err := j.Judge(context.Background(), JudgeRequest{Category: "multi-session"})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got != OutcomeInvalid {
		t.Errorf("outcome = %s, want invalid for unparseable JSON", got)
	}
}

// TestJudge_VerdictWithoutCorrectFieldDefaultsIncorrect — valid JSON that omits
// the field parses, and Go's zero value makes it false. That is the safe default:
// a judge that did not say "correct" has not affirmed the answer.
func TestJudge_VerdictWithoutCorrectFieldDefaultsIncorrect(t *testing.T) {
	j := NewJudge(&stubLLM{replies: []string{`{"reasoning":"forgot the verdict"}`}})
	got, err := j.Judge(context.Background(), JudgeRequest{Category: "multi-session"})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if got != OutcomeIncorrect {
		t.Errorf("outcome = %s, want incorrect — an absent verdict is not an "+
			"affirmation", got)
	}
}
