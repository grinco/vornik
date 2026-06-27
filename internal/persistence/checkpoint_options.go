package persistence

import (
	"encoding/json"
	"strings"
)

// CheckpointOptionLabel resolves a decision-checkpoint choice id to
// its human-readable option label from the checkpoint message's
// metadata ({"options":[{"id","label"},…]}). Returns "" when the
// metadata has no matching option (malformed JSON, no options, or an
// unknown id).
//
// Used by the answer-submission paths (UI + API) to synthesise the
// answer's Content when the operator picked an option without typing
// free text. Regression context (task …a691c512ebd1c4fd, 2026-06-06):
// choice-only answers persisted Content:"" with the choice id buried
// in metadata — the chat log showed an empty answer and the lead
// agent, which reads Content, ignored the operator's decision and
// re-asked the same checkpoint.
func CheckpointOptionLabel(meta json.RawMessage, choiceID string) string {
	choiceID = strings.TrimSpace(choiceID)
	if len(meta) == 0 || choiceID == "" {
		return ""
	}
	var raw struct {
		Options []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"options"`
	}
	if err := json.Unmarshal(meta, &raw); err != nil {
		return ""
	}
	for _, o := range raw.Options {
		if strings.EqualFold(o.ID, choiceID) {
			return o.Label
		}
	}
	return ""
}
