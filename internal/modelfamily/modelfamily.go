// Package modelfamily classifies a chat-model id into its PROVIDER FAMILY —
// the organisation that trained it — so the configuration assistant's judge
// can be checked to be of a different family from the assistant
// (2026-09-13 config-assistant design §7.3, test 37).
//
// "Different family" is the load-bearing property, not different size: a
// judge trained by the same lab shares the author's blind spots, and a
// same-family pair is where a confidently-wrong edit is confidently agreed
// with. The check is in code because a config comment cannot enforce it.
//
// The catalogue's ModelInfo.Provider is the ROUTING sub-provider ("http",
// "vertex", "bedrock"), which says where a request goes, not who trained the
// model — so this derives the family from the id's vendor prefix, with a
// table for the ids that do not carry one.
package modelfamily

import (
	"strings"
)

// Unknown is returned when no family can be derived. The callers treat
// Unknown as a REFUSAL, never as "different from everything": an
// unclassifiable pair fails the different-family check (deny by default).
const Unknown = "unknown"

// vendorPrefixes maps the leading token of an id to a family. Order does not
// matter; the longest matching prefix wins in Family.
var vendorPrefixes = map[string]string{
	"anthropic": "anthropic",
	"claude":    "anthropic",
	"openai":    "openai",
	"gpt":       "openai",
	"o1":        "openai",
	"o3":        "openai",
	"o4":        "openai",
	"gemini":    "google",
	"gemma":     "google",
	"google":    "google",
	"nvidia":    "nvidia",
	"nemotron":  "nvidia",
	"glm":       "zhipu",
	"zhipu":     "zhipu",
	"qwen":      "alibaba",
	"qwq":       "alibaba",
	"alibaba":   "alibaba",
	"deepseek":  "deepseek",
	"mistral":   "mistral",
	"mixtral":   "mistral",
	"codestral": "mistral",
	"magistral": "mistral",
	"devstral":  "mistral",
	"llama":     "meta",
	"meta":      "meta",
	"kimi":      "moonshot",
	"moonshot":  "moonshot",
	"minimax":   "minimax",
	"phi":       "microsoft",
	"microsoft": "microsoft",
	"cohere":    "cohere",
	"command":   "cohere",
	"amazon":    "amazon",
	"nova":      "amazon",
	"titan":     "amazon",
	"grok":      "xai",
	"xai":       "xai",
	"gpt-oss":   "openai",
}

// Family returns the provider family of a model id, or Unknown.
//
// Bedrock-style ids ("us.anthropic.claude-3-5-haiku-20241022-v1:0",
// "nvidia.nemotron-nano-9b-v2", "openai.gpt-oss-20b-1:0") carry the vendor
// as a dotted segment; Ollama-style ids ("glm-5.2:cloud", "qwen3.6:35b")
// carry it as the leading token before a digit, dash or colon; hosted ids
// ("claude-haiku-3-5", "gemini-2.5-pro") as the leading dash-separated word.
func Family(modelID string) string {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return Unknown
	}
	// Bedrock region prefixes ("us.", "eu.", "apac.", "global.") are not
	// vendors; drop them so the vendor segment is examined.
	for _, region := range []string{"us.", "eu.", "apac.", "global.", "jp."} {
		id = strings.TrimPrefix(id, region)
	}
	// Candidate tokens, most specific first: the whole id, then each
	// prefix cut at a separator. The longest key in the table that is a
	// prefix of the id wins.
	best := ""
	bestLen := 0
	for key, fam := range vendorPrefixes {
		if strings.HasPrefix(id, key) && len(key) > bestLen {
			// The prefix must end at a token boundary so "o1" does not
			// match "ollama-x" and "nova" does not match "novel".
			if len(id) == len(key) || !isWordByte(id[len(key)]) {
				best, bestLen = fam, len(key)
			}
		}
	}
	if best != "" {
		return best
	}
	// Dotted vendor segment anywhere ("provider/vendor.model" shapes).
	for _, sep := range []string{"/", ".", ":"} {
		if i := strings.Index(id, sep); i > 0 {
			if f := Family(id[i+1:]); f != Unknown {
				return f
			}
		}
	}
	return Unknown
}

// isWordByte reports whether b continues an alphabetic vendor token. Digits,
// dashes, dots, colons and slashes end a token, so "glm-5.2" and "qwen3.6"
// both split after the vendor; letters continue it, so "novel" is not "nova".
func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z'
}

// SameFamily reports whether two ids resolve to the same known family. Two
// Unknown ids are reported as SAME (deny by default): a pair the daemon
// cannot classify must not pass a check whose whole point is separation.
func SameFamily(a, b string) bool {
	fa, fb := Family(a), Family(b)
	if fa == Unknown || fb == Unknown {
		return true
	}
	return fa == fb
}
