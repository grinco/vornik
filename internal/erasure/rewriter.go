package erasure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
)

// The generative step of Art 17 shared-row redaction.
//
// This asks a model to remove one person from a record while preserving everyone
// else in it. NOTHING here is trusted: the caller runs
// datasubject.VerifyRedaction over the output and discards it if any identifier
// survives. That check, not this prompt, is what makes a generative operation
// acceptable on a destructive path.
//
// The prompt is still written carefully, because the failure the verification CANNOT
// catch is over-redaction — a model that deletes the other subject's data too would
// produce text that verifies perfectly clean while destroying data we are required to
// preserve. Operator review (default-on) is the control for that, and the prompt is
// the first line of it.
//
// see LLD § https://docs.vornik.io §3, §11

// completer is the narrow slice of a chat provider a rewrite needs. Declared locally
// so this file depends on a method set rather than a concrete provider, which also
// makes it testable without a model.
type completer interface {
	Complete(ctx context.Context, messages []chat.Message) (*chat.ChatResponse, error)
}

// Rewriter produces redacted chunk text via an LLM.
type Rewriter struct {
	Provider completer
	// Model is recorded against the request. A generative decision about someone's
	// personal data must be attributable to something more specific than "an LLM".
	Model string
}

// NewRewriter builds a Rewriter. Returns an error rather than a half-built value,
// because a nil provider would surface as a deferral on every chunk of a live
// erasure — a failure that looks like a policy decision.
func NewRewriter(p completer, model string) (*Rewriter, error) {
	if p == nil {
		return nil, errors.New("erasure: redaction rewriter needs a chat provider")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("erasure: redaction rewriter needs a model name to record " +
			"against the request — an unattributable rewrite of someone's personal data is not auditable")
	}
	return &Rewriter{Provider: p, Model: model}, nil
}

// ModelVersion identifies what produced a rewrite.
func (r *Rewriter) ModelVersion() string {
	if r == nil {
		return "unknown"
	}
	return r.Model
}

// redactionSystemPrompt is deliberately narrow and negative.
//
// The two instructions that matter most are the ones guarding the OTHER subject:
// preserve everything about everyone else, and do not summarise. A model that
// helpfully condenses the record has destroyed third-party data that Art 17 gives us
// no permission to touch — and unlike a surviving identifier, that loss is invisible
// to the mechanical check.
const redactionSystemPrompt = `You rewrite a stored record to remove ONE person's personal data while preserving everyone else's.

RULES:
1. Remove every trace of the listed person: names, nicknames, initials, email addresses, phone numbers, handles, and any pronoun or role reference that points at them ("she", "the caller", "the patient"). Replace them with a neutral, non-identifying term such as "the client", "a participant", or restructure the sentence.
2. PRESERVE EVERYTHING ELSE EXACTLY. Other people's names, contact details, statements and attributions must survive unchanged. You have no permission to remove or alter their data.
3. DO NOT summarise, shorten, tidy, translate or improve the text. Change only what is necessary to remove the listed person. Keep the original language, tone, structure and level of detail.
4. Do not add commentary, headings, or notes about what you changed. Do not write "[REDACTED]" or similar markers.
5. If a sentence exists solely to convey the listed person's data, remove that sentence rather than replacing it with a placeholder.
6. Never invent facts to fill a gap.

Return ONLY the rewritten record text, with no preamble and no explanation.`

// RewriteWithout returns content with the given identifiers' subject removed.
func (r *Rewriter) RewriteWithout(ctx context.Context, content string, identifiers []string) (string, error) {
	if r == nil || r.Provider == nil {
		return "", errors.New("erasure: no rewrite provider configured")
	}
	live := make([]string, 0, len(identifiers))
	for _, id := range identifiers {
		if strings.TrimSpace(id) != "" {
			live = append(live, strings.TrimSpace(id))
		}
	}
	if len(live) == 0 {
		// Refuse rather than ask a model to remove "nothing" — it would return the
		// text unchanged, which VerifyRedaction would then pass if the text happens
		// to contain no identifiers, reporting a redaction that never happened.
		return "", errors.New("erasure: no identifiers supplied, so there is nothing to " +
			"redact and no basis for claiming the record was changed")
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("erasure: cannot redact empty record text")
	}

	user := fmt.Sprintf("Remove this person's personal data from the record below.\n\n"+
		"THE PERSON IS IDENTIFIED BY:\n%s\n\n"+
		"RECORD (rewrite this, preserving all other people's data):\n---\n%s\n---",
		"- "+strings.Join(live, "\n- "), content)

	resp, err := r.Provider.Complete(ctx, []chat.Message{
		{Role: "system", Content: redactionSystemPrompt},
		{Role: "user", Content: user},
	})
	if err != nil {
		return "", fmt.Errorf("erasure: redaction rewrite failed: %w", err)
	}
	out, err := firstChoiceText(resp)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stripCodeFence(out)), nil
}

// firstChoiceText pulls the reply text, failing loudly on an empty response rather
// than returning "" — an empty rewrite would delete the whole record, including the
// other subjects' data.
func firstChoiceText(resp *chat.ChatResponse) (string, error) {
	if resp == nil || len(resp.Choices) == 0 {
		return "", errors.New("erasure: the model returned no rewrite")
	}
	text := resp.Choices[0].Message.Content
	if strings.TrimSpace(text) == "" {
		return "", errors.New("erasure: the model returned an empty rewrite; treating it as a " +
			"failure rather than deleting the record, which also holds other people's data")
	}
	return text, nil
}

// stripCodeFence removes a wrapping ``` block, which models add unprompted. Left in
// place it would be written into the record as literal backticks.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	} else {
		return s
	}
	if j := strings.LastIndex(t, "```"); j >= 0 {
		t = t[:j]
	}
	return t
}
