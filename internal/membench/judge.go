package membench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Answer generation and judging (design §5.6, §5.9).
//
// Both systems get an IDENTICAL answer prompt and an identical judge prompt. The
// judge has per-category variants because one grading rule is genuinely wrong for
// these categories: a correct temporal chain that miscounts by a day is not a
// retrieval failure, and on an unanswerable item the correct behaviour is to
// decline rather than to produce something.

// LLM is the minimal completion interface the harness needs. Narrow on purpose:
// the harness should not care which provider or SDK is behind it, and a one-method
// interface is trivial to stub deterministically in tests.
type LLM interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// ---------------------------------------------------------------------------
// answer generation
// ---------------------------------------------------------------------------

// answerPromptTemplate instructs the model to answer from the supplied context
// only.
//
// "Only" is load-bearing. If the model may answer from world knowledge, the
// benchmark measures the model rather than the memory system — and would score
// well on a memory system that retrieved nothing at all.
const answerPromptTemplate = `You are answering a question using ONLY the retrieved memory below.

Do not use outside knowledge. If the retrieved memory does not contain the
answer, say that you do not know rather than guessing.

Retrieved memory:
%s

Question: %s

Answer concisely.`

// AnswerGenerator produces an answer from recalled context.
type AnswerGenerator struct{ llm LLM }

// NewAnswerGenerator builds a generator over the supplied model.
func NewAnswerGenerator(llm LLM) *AnswerGenerator { return &AnswerGenerator{llm: llm} }

// Answer renders the prompt and returns the model's completion verbatim.
//
// An empty hit list still produces an attempt rather than short-circuiting: an
// empty recall IS a retrieval failure and belongs in the accuracy denominator, so
// letting the judge mark it incorrect is more honest than recording a harness
// error and excluding it.
func (g *AnswerGenerator) Answer(ctx context.Context, question string, hits []Hit) (string, error) {
	var b strings.Builder
	if len(hits) == 0 {
		b.WriteString("(no memory was retrieved)")
	}
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, h.Text)
	}
	prompt := fmt.Sprintf(answerPromptTemplate, b.String(), question)

	out, err := g.llm.Complete(ctx, prompt)
	if err != nil {
		// Propagated, never swallowed into an empty answer: an LLM fault is an
		// error outcome, not a wrong answer.
		return "", fmt.Errorf("answer generation: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// AnswerPromptSHA256 hashes the answer template for the comparability key. A
// changed prompt changes the answers, so it changes what the numbers mean.
func AnswerPromptSHA256() string {
	sum := sha256.Sum256([]byte(answerPromptTemplate))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// judging
// ---------------------------------------------------------------------------

// judgeDefault grades the straightforward recall categories.
const judgeDefault = `Evaluate whether the model response contains the correct answer to the question.

Set correct=true if the response contains the correct answer, or is equivalent to
it, or contains all the intermediate steps needed to reach it. Set correct=false
if the response is incorrect, or contains only a subset of the information the
answer requires.`

// judgeTemporal forgives arithmetic slips. Without this rule the category is
// close to ungradeable: a response that retrieved both dates correctly and
// reasoned correctly but landed one day out would score as a retrieval failure,
// which is precisely the wrong attribution.
const judgeTemporal = `Evaluate whether the model response contains the correct answer to the question.

Set correct=true if the response contains the correct answer, is equivalent to
it, or contains all the intermediate steps needed to reach it.

Do NOT penalise off-by-one errors in day, week or month counts. If the question
asks for a number of days and the response says 19 where the answer is 18, that
response is still correct.`

// judgeKnowledgeUpdate accepts superseded information appearing alongside the
// current value — a response that shows the history is not wrong so long as it
// lands on the updated answer.
const judgeKnowledgeUpdate = `Evaluate whether the model response contains the correct, UPDATED answer to the
question.

Set correct=true if the response contains the updated answer, even when it also
mentions earlier superseded information. Set correct=false if the response gives
only the superseded value.`

// judgePreference grades against a rubric rather than a string, because "correct"
// here means the response used the person's stated preferences at all.
const judgePreference = `Evaluate whether the model response satisfies the desired personalised response.

The response does not need to reflect every point of the rubric. Set correct=true
if it recalls and uses the user's personal information correctly; set
correct=false if it ignores or contradicts it.`

// judgeAbstention inverts the usual rule: declining is the right answer.
//
// Without this variant every honest refusal scores wrong and confident
// fabrication scores right, which would actively reward the failure mode the
// category exists to detect.
const judgeAbstention = `This question CANNOT be answered from the stored memory.

Set correct=true if the response abstains — saying it does not know, or that
there is no information available. Set correct=false if the response asserts an
answer anyway, however plausible it sounds.`

// judgeEnvelope is appended to every variant so the reply shape is uniform
// regardless of which grading rule applied.
const judgeEnvelope = `

Question: %s
Correct answer: %s
Model response: %s

Reply with JSON only:
{"reasoning": "<one short sentence>", "correct": true|false}`

// judgePrompts maps a dataset category to its grading rule.
var judgePrompts = map[string]string{
	"single-session-user":       judgeDefault,
	"single-session-assistant":  judgeDefault,
	"multi-session":             judgeDefault,
	"locomo":                    judgeDefault,
	"temporal-reasoning":        judgeTemporal,
	"knowledge-update":          judgeKnowledgeUpdate,
	"single-session-preference": judgePreference,
	"unanswerable":              judgeAbstention,
}

// judgePromptFor returns the grading rule for a category, falling back to the
// default. An unknown category must never yield an empty prompt — every item in
// it would score invalid, which looks like a judge outage rather than a missing
// mapping.
func judgePromptFor(category string) string {
	if p, ok := judgePrompts[strings.ToLower(strings.TrimSpace(category))]; ok {
		return p
	}
	return judgeDefault
}

// judgePromptCorpus concatenates every variant in a stable order, for hashing.
//
// Hashing the corpus rather than the default matters: an edit to the temporal
// rule changes grading for that category, and a key that missed it would let two
// differently-graded runs be compared as if identical.
func judgePromptCorpus() string {
	keys := make([]string, 0, len(judgePrompts))
	for k := range judgePrompts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("\x00")
		b.WriteString(judgePrompts[k])
		b.WriteString("\x00")
	}
	b.WriteString(judgeEnvelope)
	return b.String()
}

// JudgePromptSHA256 hashes every category variant plus the envelope, for the
// comparability key.
func JudgePromptSHA256() string {
	sum := sha256.Sum256([]byte(judgePromptCorpus()))
	return hex.EncodeToString(sum[:])
}

// JudgeRequest is one grading request.
type JudgeRequest struct {
	Category   string
	Question   string
	GoldAnswer string
	Answer     string
}

// Judge grades answers with an LLM.
type Judge struct{ llm LLM }

// NewJudge builds a judge over the supplied model.
func NewJudge(llm LLM) *Judge { return &Judge{llm: llm} }

type judgeVerdict struct {
	Reasoning string `json:"reasoning"`
	Correct   bool   `json:"correct"`
}

// jsonObjectRE finds the first JSON object in a reply. Models routinely wrap
// their JSON in prose or code fences, and failing on that would inflate the
// invalid rate for a formatting habit rather than a grading problem.
var jsonObjectRE = regexp.MustCompile(`(?s)\{.*\}`)

// Judge returns the outcome for one answer.
//
// Unparseable output is retried exactly once and then scored OutcomeInvalid —
// never OutcomeIncorrect. We do not know what the verdict was, and guessing would
// attribute a judge formatting failure to retrieval quality. An LLM transport
// error is returned as an error instead, so the runner can record OutcomeError
// and keep infrastructure faults distinguishable from unintelligible verdicts.
func (j *Judge) Judge(ctx context.Context, req JudgeRequest) (Outcome, error) {
	prompt := judgePromptFor(req.Category) +
		fmt.Sprintf(judgeEnvelope, req.Question, req.GoldAnswer, req.Answer)

	const attempts = 2
	for i := 0; i < attempts; i++ {
		raw, err := j.llm.Complete(ctx, prompt)
		if err != nil {
			return "", fmt.Errorf("judge: %w", err)
		}
		if v, ok := parseVerdict(raw); ok {
			if v.Correct {
				return OutcomeCorrect, nil
			}
			return OutcomeIncorrect, nil
		}
	}
	return OutcomeInvalid, nil
}

// parseVerdict extracts a verdict from a possibly-decorated reply.
func parseVerdict(raw string) (judgeVerdict, bool) {
	m := jsonObjectRE.FindString(raw)
	if m == "" {
		return judgeVerdict{}, false
	}
	var v judgeVerdict
	if err := json.Unmarshal([]byte(m), &v); err != nil {
		return judgeVerdict{}, false
	}
	return v, true
}
